package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
)

// tokenUsage is the per-request token accounting extracted from the upstream
// OpenAI-compatible usage block. Shared by the OpenAI and Anthropic paths and
// recorded on the reqlog Event.
type tokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CachedTokens     int
}

// extractUsage scans an OpenAI-compatible JSON document for a "usage" block and
// returns its token counts, or nil if none is present. It is used for both the
// streaming final chunk (whose payload is just the usage object) and the full
// non-streaming response body. A cheap `bytes.Contains` gate avoids parsing
// every stream chunk.
//
// The cache-hit field name is not standardized across dashscope's compatible
// mode: it has appeared as top-level `cached_tokens`, nested under
// `prompt_tokens_details.cached_tokens` (OpenAI spec), or as the Anthropic
// `cache_read_input_tokens` when the translator reflects it back. We try each
// and take the first non-zero. Real deployments should verify which dashscope
// actually returns and prune the list (see plan: "真实环境校准").
func extractUsage(data []byte) *tokenUsage {
	if len(data) == 0 || !bytes.Contains(data, []byte(`"usage"`)) {
		return nil
	}
	var raw struct {
		Usage struct {
			PromptTokens  int `json:"prompt_tokens"`
			Completion    int `json:"completion_tokens"`
			TotalTokens   int `json:"total_tokens"`
			CachedTokens  int `json:"cached_tokens"`
			CacheRead     int `json:"cache_read_input_tokens"`
			PromptDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	u := raw.Usage
	return tokenUsageFromFields(u.PromptTokens, u.Completion, u.TotalTokens, u.CachedTokens, u.PromptDetails.CachedTokens, u.CacheRead)
}

// tokenUsageFromFields applies the dashscope cache-field precedence and the
// all-zero guard shared by extractUsage (streaming/non-streaming payload parse)
// and the Anthropic non-streaming translator (which already has the fields
// parsed into its response struct and can avoid a second full-body unmarshal).
// Precedence for cached: top-level cached_tokens → prompt_tokens_details.cached_tokens
// → cache_read_input_tokens, first non-zero wins. Returns nil when every field
// is zero (no meaningful usage to record).
func tokenUsageFromFields(prompt, completion, total, cached, cachedNested, cacheRead int) *tokenUsage {
	c := cached
	if c == 0 {
		c = cachedNested
	}
	if c == 0 {
		c = cacheRead
	}
	if prompt == 0 && completion == 0 && total == 0 && c == 0 {
		return nil
	}
	return &tokenUsage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
		CachedTokens:     c,
	}
}

// hashKey reduces an affinity key (the first user message text) to a short,
// non-reversible tag so the log can correlate a conversation's requests
// without storing the prompt itself. FNV-1a truncated to 8 hex chars.
func hashKey(s string) string {
	if s == "" {
		return ""
	}
	h := fnv.New64a()
	h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum64())[:8]
}

// newReqID returns a random 12-hex-char request id, unique enough for the log
// ring/DB and frontend dedup without a process-wide counter.
func newReqID() string {
	b := make([]byte, 6)
	rand.Read(b) //nolint
	return hex.EncodeToString(b)
}
