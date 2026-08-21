package proxy

// shouldRetryStatus reports whether an upstream HTTP status is account-
// attributable — i.e. switching to a different account could plausibly succeed
// where this one failed — and therefore the retry loop should exclude this
// account and try the next, rather than passing the response through as-is.
//
// The retry loop's semantics (429 aside, which has its own adaptive-backoff
// branch):
//
//   - 401 / 403: bad/forbidden CREDENTIALS for this account. A different
//     account has different credentials and may serve the request. Retrying
//     self-heals a misconfigured/expired key by routing around it.
//   - 408 / 418 / 425 / 429 / 451 / 4xx-with-Retry-After: transient or
//     risk-controlled failures that are not the request's fault. 408 is an
//     upstream-side timeout (the request itself was valid); 425 Too Early /
//     449 / 451 are risk-control / geo blocks common on cross-border calls.
//   - Everything else is treated as request-level (400/404/413/422 — the
//     request is malformed or too large, so any account returns the same
//     error) and is passed through immediately.
//
// 5xx is deliberately EXCLUDED — upstream 5xx is a service-wide outage where
// switching accounts hits the same downstream failure, and Cooldown-ing every
// account during a flap would drain the pool. Pass it through.
//
// The set is intentionally allowlist-style: a status not on this list falls
// through to "pass through", so an unfamiliar 4xx does NOT trigger a retry
// fan-out that burns the whole pool on one request.
func shouldRetryStatus(code int) bool {
	if code < 400 || code >= 500 {
		return false
	}
	switch code {
	case 401, 403, 408, 418, 425, 429, 449, 451:
		return true
	}
	return false
}
