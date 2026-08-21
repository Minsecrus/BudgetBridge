package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"budgetbridge/internal/pool"

	"github.com/gin-gonic/gin"
)

// probeConcurrency caps how many accounts TestAll probes at once, so probing a
// large pool doesn't fire a burst of requests at upstream simultaneously.
const probeConcurrency = 5

// probeAccount sends a minimal chat-completion request to upstream with the
// given API key and returns the upstream HTTP status. A non-2xx status is NOT
// an error — callers decide availability via status == 200. err is non-nil
// only on transport failure (network/timeout).
func probeAccount(ctx context.Context, upstream, modelOverride, apiKey string) (int, error) {
	model := modelOverride
	if model == "" {
		model = "qwen-plus"
	}
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"stream":   false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// TestAccount probes a single account (by stable ID) against upstream and
// returns {ok, status, error}. It does NOT go through the pool selector — it
// uses the account's own API key directly.
func TestAccount(p *pool.Pool, upstream, modelOverride string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			c.JSON(400, gin.H{"error": "invalid id"})
			return
		}
		acc := p.ByID(id)
		if acc == nil {
			c.JSON(400, gin.H{"error": "invalid id"})
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
		defer cancel()
		effUp := effectiveUpstream(upstream, acc.WSDomain)
		status, err := probeAccount(ctx, effUp, modelOverride, acc.APIKey)
		if err != nil {
			c.JSON(200, gin.H{"ok": false, "status": 0, "error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": status == 200, "status": status, "error": ""})
	}
}

type testResult struct {
	ID     int    `json:"id"`
	Alias  string `json:"alias"`
	OK     bool   `json:"ok"`
	Status int    `json:"status"`
	Error  string `json:"error"`
}

// TestAll probes every account concurrently and returns one result per
// account, keyed by stable ID. Each account uses its own API key directly.
func TestAll(p *pool.Pool, upstream, modelOverride string) gin.HandlerFunc {
	return func(c *gin.Context) {
		statuses := p.All()
		results := make([]testResult, len(statuses))
		// Cap concurrent probes so a large pool doesn't hit upstream all at
		// once (risks rate-limiting / risk-control on the upstream side).
		sem := make(chan struct{}, probeConcurrency)
		var wg sync.WaitGroup
		for i, st := range statuses {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, id int, alias string) {
				defer wg.Done()
				defer func() { <-sem }()
				acc := p.ByID(id)
				if acc == nil {
					results[i] = testResult{ID: id, Alias: alias, OK: false, Error: "account removed"}
					return
				}
				ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
				defer cancel()
				effUp := effectiveUpstream(upstream, acc.WSDomain)
				status, err := probeAccount(ctx, effUp, modelOverride, acc.APIKey)
				if err != nil {
					results[i] = testResult{ID: id, Alias: alias, OK: false, Error: err.Error()}
					return
				}
				results[i] = testResult{ID: id, Alias: alias, OK: status == 200, Status: status}
			}(i, st.ID, st.Alias)
		}
		wg.Wait()
		c.JSON(200, results)
	}
}
