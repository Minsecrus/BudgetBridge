package proxy

import (
	"bytes"
	"context"
	"net/http"
	"time"
)

// netRetryDelay is the backoff before the in-place retry on the SAME account
// after a transport error. It is short on purpose: the common case is a
// transient blip (connection reset, momentary DNS hiccup, a single dropped
// packet on a cross-border link) that clears within tens of milliseconds.
// Waiting ~150ms and retrying the same account — instead of immediately
// excluding it and falling to the next account — preserves the prefix cache
// for this account and avoids churning the session map. Only if the second
// attempt also fails do we treat the account as bad and let the retry loop
// advance to another account.
//
// netRetryDelay is a package var (not a const) so tests can shorten it; the
// default is a package-level constant guarding production use.
var netRetryDelay = defaultNetRetryDelay

const defaultNetRetryDelay = 150 * time.Millisecond

// forwardResult is the outcome of one forward call to upstream.
//
//   - err != nil: BOTH attempts (initial + optional in-place retry) failed with
//     transport errors. resp is nil. The caller excludes this account and
//     advances to the next. retried records whether the in-place retry fired
//     (for stats).
//   - err == nil: at least one attempt returned response headers. resp is the
//     caller's to close. A retried=true with err==nil means the first attempt
//     failed and the retry succeeded — the request ultimately succeeded on
//     this account, so the caller records it as a network-retry, not an error.
type forwardResult struct {
	resp    *http.Response
	err     error
	retried bool
}

// forward sends body to url with apiKey as a chat-completion request. On a
// transport error it retries once in place on the SAME account after a short
// backoff (see netRetryDelay) before giving up — this turns most transient
// network blips into a client-transparent success without losing the account's
// prefix cache. If the client context is already cancelled, no retry is
// attempted (the request is dead and retrying would only delay the 503).
//
// The response body is the caller's responsibility to close on a non-nil resp.
func forward(ctx context.Context, url string, body []byte, apiKey string) forwardResult {
	do := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		return httpClient.Do(req)
	}

	resp, err := do()
	if err == nil {
		return forwardResult{resp: resp}
	}
	// First attempt failed at the transport layer. Retry once on the same
	// account unless the client has gone away — there's no point retrying a
	// dead request, and a cancelled context also surfaces as a transport error
	// we don't want to mask with a backoff.
	if ctx.Err() != nil {
		return forwardResult{err: err}
	}
	timer := time.NewTimer(netRetryDelay)
	select {
	case <-ctx.Done():
		timer.Stop()
		return forwardResult{err: err}
	case <-timer.C:
	}
	resp2, err2 := do()
	return forwardResult{resp: resp2, err: err2, retried: true}
}
