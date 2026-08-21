// Package devproxy implements the development-mode single-port entry point.
// When BB_DEV=1 the backend reverse-proxies non-API requests to the vite dev
// server so the developer only talks to one port (the backend listen port),
// mirroring the production caddy single-entry setup.
package devproxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// Enabled reports whether dev mode is on (BB_DEV=1).
func Enabled() bool {
	return os.Getenv("BB_DEV") == "1"
}

// ShouldProxy reports whether a request should be forwarded to vite in dev
// mode. GET requests and websocket upgrades go to vite; POST etc. fall through
// to the original NoRoute fallback (API endpoint toleration such as
// /chat/completions).
func ShouldProxy(r *http.Request) bool {
	if r.Method == http.MethodGet {
		return true
	}
	return isWebsocketUpgrade(r)
}

func isWebsocketUpgrade(r *http.Request) bool {
	if !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		return false
	}
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// New returns a ReverseProxy that forwards requests to the vite dev server at
// target (e.g. "http://localhost:5173"). The stdlib ReverseProxy transparently
// handles websocket upgrades (Connection: Upgrade) since Go 1.12, so vite HMR
// works through the proxy without extra plumbing.
func New(target string) (*httputil.ReverseProxy, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	return httputil.NewSingleHostReverseProxy(u), nil
}

// MaybeProxy returns a gin handler that, in dev mode, forwards GET and
// websocket requests to vite via proxy and delegates everything else to
// fallback. When dev mode is off (or proxy is nil) it always calls fallback,
// preserving the original NoRoute behavior exactly — production is unaffected.
func MaybeProxy(proxy *httputil.ReverseProxy, fallback gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if Enabled() && proxy != nil && ShouldProxy(c.Request) {
			proxy.ServeHTTP(c.Writer, c.Request)
			c.Abort()
			return
		}
		fallback(c)
	}
}
