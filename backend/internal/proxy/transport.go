// Package proxy — upstream transport construction.
//
// newUpstreamClient builds the *http.Client used to forward requests to the
// upstream model service. Every upstream call (chat proxy, anthropic
// translator, and the probe/test handlers) shares one client via the package
// variable httpClient, configured once from config at startup.
//
// Two cross-border concerns are centralized here:
//
//  1. HTTP/2 with PING keepalive — the bridge is commonly deployed overseas
//     while the upstream is in-region, so cross-border NAT/firewalls silently
//     kill idle h2 connections. PING keeps the path warm and retires dead
//     connections before the retry loop pays a full upstream RTT to discover
//     them (see proxy.go's httpClient comment for the full rationale).
//
//  2. An optional SOCKS5 proxy (upstream_proxy in config). When set, upstream
//     TCP is dialed through the SOCKS5 server instead of directly. This lets a
//     cross-border deployment route the upstream leg over a premium line via a
//     SOCKS5 gateway without touching process env vars — http.Transport only
//     reads HTTP(S)_PROXY from the env, not SOCKS5. Empty → direct connection.
//
// The proxy is applied via DialContext so it sits below TLS/HTTP2: the SOCKS5
// tunnel is a transparent TCP pipe, and the h2 PING above it also keeps the
// SOCKS5 tunnel itself alive (SOCKS5 has no keepalive of its own, so without
// PING an idle tunnel gets reaped by NAT).
package proxy

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// httpClient is the shared upstream client. Defaulted to a direct (no-proxy)
// client at package init so tests and any early use work; main.go calls
// ConfigureUpstream once after loading config to (re)build it with the
// configured SOCKS5 proxy before any request is served.
var httpClient = newUpstreamClient("")

// ConfigureUpstream rebuilds the shared upstream client, optionally dialing
// through a SOCKS5 proxy. proxyURL is the raw upstream_proxy value from
// config: empty (no proxy) or socks5(h)://[user:pass@]host:port. An
// unsupported scheme logs a warning and falls back to a direct connection.
// Call once at startup before serving; not safe to call concurrently with
// requests (the caller — main — guarantees that).
func ConfigureUpstream(proxyURL string) {
	httpClient = newUpstreamClient(proxyURL)
}

func newUpstreamClient(proxyURL string) *http.Client {
	dialer, err := socksDialer(proxyURL)
	if err != nil {
		log.Printf("[warn] upstream_proxy: %v — using direct connection", err)
	}

	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 256,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		// ResponseHeaderTimeout bounds how long we wait for upstream to return
		// the *response headers* (not the body). For non-streaming requests
		// carrying ~100k-token prompts, upstream model latency to first byte
		// routinely exceeds a minute, so 60s would falsely time out healthy
		// large requests and flip them to a different account (losing the prefix
		// cache) for no benefit. 180s accommodates the heaviest non-streaming
		// completions while still bounding a genuinely stuck upstream. Streaming
		// requests return 200 headers near-instantly once generation begins, so
		// they are unaffected.
		ResponseHeaderTimeout: 180 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if dialer != nil {
		// Route upstream TCP through the SOCKS5 server. proxy.Dialer.Dial
		// doesn't take a context, so wrap it; request cancellation still
		// propagates because the underlying dial honors the Dialer.Timeout.
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}
		log.Printf("upstream proxy enabled: %s", maskProxyURL(proxyURL))
	} else {
		log.Printf("upstream proxy disabled (direct connection)")
	}

	// h2 PING knobs live on the inner *http2.Transport; ConfigureTransports
	// exposes it. No-op if h2 is already configured.
	if h2t, err := http2.ConfigureTransports(tr); err == nil {
		h2t.ReadIdleTimeout = 15 * time.Second
		h2t.PingTimeout = 15 * time.Second
	} else {
		log.Printf("[warn] http2 configure: %v", err)
	}
	return &http.Client{Transport: tr}
}

// socksDialer parses a socks5(/h) URL and returns a dialer, or (nil, nil) for
// an empty proxyURL. The *proxy.Auth is populated from optional user:pass@.
func socksDialer(proxyURL string) (proxy.Dialer, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil, nil
	}

	rest := proxyURL
	switch {
	case strings.HasPrefix(rest, "socks5h://"):
		rest = strings.TrimPrefix(rest, "socks5h://")
	case strings.HasPrefix(rest, "socks5://"):
		rest = strings.TrimPrefix(rest, "socks5://")
	case strings.HasPrefix(rest, "//"):
		rest = strings.TrimPrefix(rest, "//")
	default:
		// Allow a bare "host:port" → assume socks5.
	}

	host, user, pass, ok := splitUserHost(rest)
	if !ok {
		return nil, fmt.Errorf("invalid proxy address %q", proxyURL)
	}
	var auth *proxy.Auth
	if user != "" {
		auth = &proxy.Auth{User: user, Password: pass}
	}

	// proxy.SOCKS5 always CONNECTs through the proxy, so the proxy resolves the
	// target hostname remotely (socks5h semantics) regardless of scheme. We
	// honor the scheme in the masked log line for operator clarity.
	return proxy.SOCKS5("tcp", host, auth, &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	})
}

// splitUserHost splits "[user:pass@]host:port" into host and optional creds.
// Returns ok=false if the host part is empty.
func splitUserHost(s string) (host, user, pass string, ok bool) {
	if i := strings.LastIndex(s, "@"); i >= 0 {
		cred := s[:i]
		host = s[i+1:]
		if j := strings.Index(cred, ":"); j >= 0 {
			user, pass = cred[:j], cred[j+1:]
		} else {
			user = cred
		}
	} else {
		host = s
	}
	if host == "" {
		return "", "", "", false
	}
	return host, user, pass, true
}

// maskProxyURL hides credentials for log output:
// socks5://secret:pass@h:1080 → socks5://***@h:1080.
func maskProxyURL(s string) string {
	scheme := "socks5"
	rest := strings.TrimSpace(s)
	for _, p := range []string{"socks5h://", "socks5://"} {
		if strings.HasPrefix(rest, p) {
			scheme = strings.TrimSuffix(p, "://")
			rest = strings.TrimPrefix(rest, p)
			break
		}
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		return scheme + "://***@" + rest[i+1:]
	}
	return scheme + "://" + rest
}
