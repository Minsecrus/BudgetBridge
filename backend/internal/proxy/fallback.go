package proxy

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"budgetbridge/internal/fallback"
	"budgetbridge/internal/reqlog"

	"github.com/gin-gonic/gin"
)

// fallbackOK writes one fallback channel's 200 response back to the client and
// returns the TTFT/usage captured for the request log. It owns reading and
// closing resp.Body. The OpenAI handler wraps streamOpenAIResponse (stream) /
// io.ReadAll+Data (non-stream); the Anthropic handler wraps the SSE translator /
// writeAnthropicResponse — fallback endpoints are OpenAI-format, so the
// Anthropic path must translate the 200 back, exactly as it does for the pool.
type fallbackOK func(c *gin.Context, resp *http.Response) (ttft *int64, usage *tokenUsage)

// tryFallback is the post-pool last resort. After the account retry loop exits
// without a 200 (pool exhausted: every account unavailable / excluded / 429'd),
// it tries each fallback channel eligible for origModel in config order. The
// first 200 is written via ok and reported as served. Transport errors and
// non-200s are recorded as reqlog attempts (AccountID 0 + channel name) and the
// next eligible channel is tried; if none succeed, served is false and the
// caller falls through to its existing 503 path.
//
// Scheduler feedback (简易, mirroring the pool): a 429 parks the channel for a
// short cooldown so the next request 平滑切号 past it instead of eating another
// 429 RTT; any channel-attributable fault (429 / bad-cred / 5xx / net) bumps a
// consecutive-failure streak that auto-disables the channel after a few failures
// (多次异常直接禁用). A 200 clears the streak. Request-level 4xx (400/404/…) is
// the caller's fault and does NOT penalize the channel.
func tryFallback(c *gin.Context, s *fallback.Store, origModel string, body []byte, ok fallbackOK, attempts *[]reqlog.Attempt) (served bool, alias string, ttft *int64, usage *tokenUsage) {
	for _, ch := range s.Pick(origModel) {
		attemptStart := time.Now()
		url := strings.TrimRight(ch.BaseURL, "/") + "/chat/completions"
		fr := forward(c.Request.Context(), url, body, ch.APIKey)
		if fr.err != nil {
			*attempts = append(*attempts, reqlog.Attempt{
				AccountID: 0, Alias: ch.Name,
				Status: 0, Outcome: "net_err",
				DurMs: time.Since(attemptStart).Milliseconds(), Err: fr.err.Error(),
			})
			applyFallbackFault(s, ch, false) // transport error → streak
			continue
		}
		resp := fr.resp
		if resp.StatusCode == 200 {
			*attempts = append(*attempts, reqlog.Attempt{
				AccountID: 0, Alias: ch.Name,
				Status: 200, Outcome: "ok", DurMs: time.Since(attemptStart).Milliseconds(),
			})
			s.RecordSuccess(ch.ID) // a 200 clears the failure streak
			ttft, usage = ok(c, resp)
			return true, ch.Name, ttft, usage
		}
		// Non-200: record (the live-stream badge colors by status code, not the
		// outcome label) and try the next channel.
		outcome := "5xx_pass"
		switch {
		case resp.StatusCode == 429:
			outcome = "429"
		case resp.StatusCode >= 400 && resp.StatusCode < 500:
			outcome = "4xx_retry"
		}
		*attempts = append(*attempts, reqlog.Attempt{
			AccountID: 0, Alias: ch.Name,
			Status: resp.StatusCode, Outcome: outcome, DurMs: time.Since(attemptStart).Milliseconds(),
		})
		resp.Body.Close()
		// Penalty: 429 cools down + streak; cred/risk-control 4xx and 5xx streak;
		// request-level 4xx (the switch's omitted default) is not the channel's
		// fault, so advance without penalizing.
		switch {
		case resp.StatusCode == 429:
			applyFallbackFault(s, ch, true)
		case shouldRetryStatus(resp.StatusCode) || resp.StatusCode >= 500:
			applyFallbackFault(s, ch, false)
		}
	}
	return false, "", nil, nil
}

// applyFallbackFault feeds a failure into the channel's scheduler and logs if
// this call crossed the auto-disable threshold. isThrottle=true (a 429) cools
// the channel down AND bumps the streak; false (bad cred / 5xx / net) bumps the
// streak only. Mirrors the pool's split between its fixed 429 cooldown and
// RecordForbidden (sustained 403 → disable), generalized to any anomaly.
func applyFallbackFault(s *fallback.Store, ch fallback.Config, isThrottle bool) {
	var streak int
	var disabled bool
	if isThrottle {
		streak, disabled = s.RecordThrottle(ch.ID)
	} else {
		streak, disabled = s.RecordFailure(ch.ID)
	}
	if disabled {
		log.Printf("[fallback] channel %s (id=%d) auto-disabled after %d consecutive failures", ch.Name, ch.ID, streak)
	}
}

// ListFallbacks returns the API-safe projection of every channel (no APIKey).
func ListFallbacks(s *fallback.Store) gin.HandlerFunc {
	return func(c *gin.Context) { c.JSON(200, s.All()) }
}

// AddFallback appends a channel and persists the registry. base_url and api_key
// are required; models default to empty (matches nothing until listed).
func AddFallback(s *fallback.Store, save func([]fallback.Config) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		var cfg fallback.Config
		if err := c.ShouldBindJSON(&cfg); err != nil || cfg.BaseURL == "" || cfg.APIKey == "" {
			c.JSON(400, gin.H{"error": "base_url and api_key are required"})
			return
		}
		s.Add(cfg)
		if err := save(s.Configs()); err != nil {
			log.Printf("[warn] save fallbacks: %v", err)
		}
		c.JSON(200, gin.H{"ok": true})
	}
}

// DeleteFallback removes a channel by ID and persists.
func DeleteFallback(s *fallback.Store, save func([]fallback.Config) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil || !s.RemoveByID(id) {
			c.JSON(400, gin.H{"error": "invalid id"})
			return
		}
		if err := save(s.Configs()); err != nil {
			log.Printf("[warn] save fallbacks on delete: %v", err)
		}
		c.JSON(200, gin.H{"ok": true})
	}
}

// UpdateFallback overwrites a channel's editable fields (name/base_url/models)
// and persists. api_key is optional in the body: empty → keep the existing key
// (the edit form doesn't receive the secret back). base_url remains required.
func UpdateFallback(s *fallback.Store, save func([]fallback.Config) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			c.JSON(400, gin.H{"error": "invalid id"})
			return
		}
		var req struct {
			Name    string   `json:"name"`
			BaseURL string   `json:"base_url"`
			APIKey  string   `json:"api_key"`
			Models  []string `json:"models"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if req.BaseURL == "" {
			c.JSON(400, gin.H{"error": "base_url is required"})
			return
		}
		if !s.UpdateByID(id, req.Name, req.BaseURL, req.APIKey, req.Models) {
			c.JSON(400, gin.H{"error": "invalid id"})
			return
		}
		if err := save(s.Configs()); err != nil {
			log.Printf("[warn] save fallbacks on update: %v", err)
		}
		c.JSON(200, gin.H{"ok": true})
	}
}

// ToggleFallback flips a channel's enabled flag and persists.
func ToggleFallback(s *fallback.Store, save func([]fallback.Config) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil || !s.ToggleByID(id) {
			c.JSON(400, gin.H{"error": "invalid id"})
			return
		}
		if err := save(s.Configs()); err != nil {
			log.Printf("[warn] save fallbacks on toggle: %v", err)
		}
		c.JSON(200, gin.H{"ok": true})
	}
}

// TestFallback probes one channel's endpoint with its own key (bypassing the
// pool, symmetric with TestAccount). The probe model is the first concrete
// (non-"*") model in the channel's whitelist; a wildcard-only channel falls
// through to probeAccount's internal default, which may not exist on the target
// platform — the returned status lets the operator see that.
func TestFallback(s *fallback.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			c.JSON(400, gin.H{"error": "invalid id"})
			return
		}
		ch, ok := s.ByID(id)
		if !ok {
			c.JSON(400, gin.H{"error": "invalid id"})
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
		defer cancel()
		status, err := probeAccount(ctx, ch.BaseURL, firstConcreteModel(ch.Models), ch.APIKey)
		if err != nil {
			c.JSON(200, gin.H{"ok": false, "status": 0, "error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": status == 200, "status": status, "error": ""})
	}
}

// firstConcreteModel returns the first non-wildcard, non-empty entry in models,
// or "" if there is none (a "*" channel has no concrete model to probe).
func firstConcreteModel(models []string) string {
	for _, m := range models {
		if m != "" && m != "*" {
			return m
		}
	}
	return ""
}
