package proxy

import "testing"

// TestShouldRetryStatus verifies the allowlist of account-attributable 4xx that
// the retry loop rotates accounts on, and — just as importantly — the statuses
// that must NOT retry (request-level 4xx and all 5xx).
func TestShouldRetryStatus(t *testing.T) {
	cases := []struct {
		code int
		want bool
		why  string
	}{
		{401, true, "bad credentials → another account may work"},
		{403, true, "forbidden creds → rotate"},
		{408, true, "upstream timeout, not the request's fault"},
		{418, true, "risk control"},
		{425, true, "too early / risk control"},
		{429, true, "per-key QPS (adaptive backoff branch)"},
		{449, true, "retry-with (alibaba risk control)"},
		{451, true, "legal/geo block on this account's exit"},

		{400, false, "malformed request — any account returns 400"},
		{404, false, "model/path not found — same on every account"},
		{413, false, "request too large — same on every account"},
		{422, false, "unprocessable request — same on every account"},
		{414, false, "URI too long — request-level"},
		{431, false, "headers too large — request-level"},

		{500, false, "upstream 5xx is service-wide; don't drain the pool"},
		{502, false, "bad gateway is downstream of all accounts"},
		{503, false, "service unavailable — same backend"},
		{504, false, "gateway timeout — service-wide"},

		{200, false, "success, not a retry path"},
		{301, false, "redirect, not a retry path"},
	}
	for _, c := range cases {
		if got := shouldRetryStatus(c.code); got != c.want {
			t.Errorf("shouldRetryStatus(%d)=%v want %v (%s)", c.code, got, c.want, c.why)
		}
	}
}
