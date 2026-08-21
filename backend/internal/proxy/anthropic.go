package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"budgetbridge/internal/fallback"
	"budgetbridge/internal/pool"
	"budgetbridge/internal/reqlog"
	"budgetbridge/internal/stats"

	"github.com/gin-gonic/gin"
)

type oaiMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []oaiToolCall   `json:"tool_calls,omitempty"`
}

// oaiContentBlock is reserved for future array-form content use; it is no
// longer populated by the translator. Kept so the JSON-tag struct is available
// if explicit dashscope caching is reintroduced behind a config flag.
//
// NOTE: emitting array-form content here previously broke dashscope prefix
// caching for Anthropic-SDK clients — see anthropicSystemContent. Do not
// resurrect array-form emission without a cache-key analysis.
type oaiContentBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

// jsonString wraps a string as a content value; empty string → nil so it is
// omitted (matching the old `omitempty` string behavior).
func jsonString(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	b, _ := json.Marshal(s)
	return b
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiRequest struct {
	Model         string            `json:"model"`
	Messages      []oaiMessage      `json:"messages"`
	MaxTokens     int               `json:"max_tokens,omitempty"`
	Stream        bool              `json:"stream"`
	Tools         []oaiTool         `json:"tools,omitempty"`
	StreamOptions *oaiStreamOptions `json:"stream_options,omitempty"`
}

// oaiStreamOptions forces include_usage so dashscope emits a final chunk with
// token/cache accounting we can record. Only set when request logging is on.
type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiTool struct {
	Type     string      `json:"type"`
	Function oaiToolFunc `json:"function"`
}

type oaiToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func AnthropicHandler(p *pool.Pool, upstream, modelOverride string, st *stats.Store, lg *reqlog.Logger, fbs *fallback.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		body, _ := io.ReadAll(c.Request.Body)

		// Parse once. Previously this body was unmarshaled 5x (3x for the log
		// line below, 1x here, 1x for the affinity key). The log now reads the
		// already-parsed fields, and the affinity key + warmth are derived from
		// areq too.
		var areq struct {
			Model     string          `json:"model"`
			System    json.RawMessage `json:"system"`
			MaxTokens int             `json:"max_tokens"`
			Stream    bool            `json:"stream"`
			Tools     []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"input_schema"`
			} `json:"tools"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &areq); err != nil {
			log.Printf("[anthropic] parse error: %v", err)
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		log.Printf("[anthropic] incoming model=%s stream=%v tools=%d", areq.Model, areq.Stream, len(areq.Tools))
		origModel := areq.Model // fallback channels use the client's original model, not the dashscope override
		if modelOverride != "" {
			areq.Model = modelOverride
		}

		msgs := make([]oaiMessage, 0, len(areq.Messages)+1)
		if len(areq.System) > 0 && string(areq.System) != "null" {
			msgs = append(msgs, oaiMessage{Role: "system", Content: anthropicSystemContent(areq.System)})
		}
		for _, m := range areq.Messages {
			msgs = append(msgs, anthropicMsgToOAI(m.Role, m.Content)...)
		}

		var tools []oaiTool
		for _, t := range areq.Tools {
			tools = append(tools, oaiTool{
				Type: "function",
				Function: oaiToolFunc{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.InputSchema,
				},
			})
		}

		req := oaiRequest{
			Model:     areq.Model,
			Messages:  msgs,
			MaxTokens: areq.MaxTokens,
			Stream:    areq.Stream,
			Tools:     tools,
		}
		if areq.Stream && lg.Enabled() {
			req.StreamOptions = &oaiStreamOptions{IncludeUsage: true}
		}
		reqBody, _ := json.Marshal(req)

		// Affinity key = first user message text, derived from the already-parsed
		// areq (no second body unmarshal). contentToText is the same helper
		// parseAffinity uses internally. warmth is true when the conversation
		// already has an assistant turn (multi-turn); see parseAffinity. Here we
		// scan the already-parsed areq.Messages directly.
		affinityKey := ""
		warm := false
		for _, m := range areq.Messages {
			if m.Role == "assistant" {
				warm = true
			}
			if m.Role == "user" && affinityKey == "" {
				affinityKey = contentToText(m.Content)
			}
		}
		cold := !warm && len(body) < p.ColdRequestBytes()
		finalOutcome := stats.ServerError
		var finalAcc *pool.Account
		var finalFb string // fallback channel name when a fallback (not a pool account) served the request
		hitThrottle := false
		hitNetRetry := false
		throttle := func(acc *pool.Account) {
			if st != nil {
				st.RecordThrottle(acc.ID)
			}
			hitThrottle = true
		}
		networkRetry := func(acc *pool.Account) {
			if st != nil {
				st.RecordNetworkRetry(acc.ID)
			}
			hitNetRetry = true
		}
		if st != nil {
			defer func() {
				st.RecordGlobal(finalOutcome)
				if hitThrottle {
					st.RecordGlobalThrottle()
				}
				if hitNetRetry {
					st.RecordGlobalNetworkRetry()
				}
				if finalAcc != nil {
					st.RecordAccount(finalAcc.ID, finalOutcome)
				}
			}()
		}
		// Per-request scheduling trace for the admin log (see proxy.go). The
		// affinity key is hashed so the prompt text never leaves the process.
		var (
			attempts   []reqlog.Attempt
			ttft       *int64
			usage      *tokenUsage
			noAccounts bool
		)
		effModel := modelOverride
		if effModel == "" {
			effModel = areq.Model
		}
		// Record token usage against the serving account's per-model free
		// quota (local accounting for cold routing). Runs regardless of reqlog.
		defer func() {
			if usage != nil && finalAcc != nil && usage.TotalTokens > 0 {
				finalAcc.ConsumeFree(effModel, usage.TotalTokens)
			}
		}()
		if lg.Enabled() {
			defer func() {
				var faID int
				var faAlias string
				if finalAcc != nil {
					faID = finalAcc.ID
					faAlias = finalAcc.DisplayAlias()
				} else if finalFb != "" {
					faAlias = finalFb
				}
				outcome := outcomeName(finalOutcome)
				if noAccounts {
					outcome = "no_accounts"
				}
				var pt, ct, tt, cache int
				if usage != nil {
					pt, ct, tt, cache = usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, usage.CachedTokens
				}
				retries := 0
				if n := len(attempts); n > 0 {
					retries = n - 1
				}
				lg.Log(reqlog.Event{
					ID:               newReqID(),
					Ts:               start.UnixMilli(),
					Proto:            "anthropic",
					Model:            effModel,
					Stream:           areq.Stream,
					Bytes:            len(body),
					KeyHash:          hashKey(affinityKey),
					Warm:             warm,
					Cold:             cold,
					Attempts:         attempts,
					Retries:          retries,
					FinalAccountID:   faID,
					FinalAlias:       faAlias,
					Status:           c.Writer.Status(),
					Outcome:          outcome,
					DurMs:            time.Since(start).Milliseconds(),
					TTFTMs:           ttft,
					PromptTokens:     pt,
					CompletionTokens: ct,
					TotalTokens:      tt,
					CachedTokens:     cache,
				})
			}()
		}
		tried := make([]int, 0, p.Len())
		for i := 0; i < p.Len(); i++ {
			acc := p.Pick(affinityKey, cold, effModel, tried...)
			if acc == nil {
				break
			}
			attemptStart := time.Now()
			accURL := effectiveUpstream(upstream, acc.WSDomain) + "/chat/completions"
			fr := forward(c.Request.Context(), accURL, reqBody, acc.APIKey)
			if fr.retried {
				networkRetry(acc)
			}
			if fr.err != nil {
				log.Printf("[anthropic] upstream err: %v", fr.err)
				// exclude this account for the rest of THIS request so affinity
				// mode advances to the next account instead of re-picking it.
				attempts = append(attempts, reqlog.Attempt{
					AccountID: acc.ID, Alias: acc.DisplayAlias(),
					Status: 0, Outcome: "net_err",
					DurMs: time.Since(attemptStart).Milliseconds(), Err: fr.err.Error(),
				})
				tried = append(tried, acc.ID)
				continue // transient — try next account, not a final outcome
			}
			resp := fr.resp
			log.Printf("[anthropic] upstream status: %d", resp.StatusCode)
			switch resp.StatusCode {
			case 429:
				attempts = append(attempts, reqlog.Attempt{
					AccountID: acc.ID, Alias: acc.DisplayAlias(),
					Status: 429, Outcome: "429", DurMs: time.Since(attemptStart).Milliseconds(),
				})
				resp.Body.Close()
				p.CooldownModel(acc, effModel, 60*time.Second)
				throttle(acc)
				tried = append(tried, acc.ID)
				continue
			case 200:
				attempts = append(attempts, reqlog.Attempt{
					AccountID: acc.ID, Alias: acc.DisplayAlias(),
					Status: 200, Outcome: "ok", DurMs: time.Since(attemptStart).Milliseconds(),
				})
				defer resp.Body.Close()
				finalAcc = acc
				finalOutcome = stats.OK
				p.RecordSuccess(acc)
				if areq.Stream {
					// Translates the OAI stream to Anthropic SSE events while
					// capturing TTFT (first content delta) and usage (final
					// chunk) for the log.
					ttft, usage = streamAnthropicResponse(c, resp.Body, areq.Model, lg, start)
				} else {
					usage = writeAnthropicResponse(c, resp.Body, areq.Model, lg)
				}
				return
			default:
				if shouldRetryStatus(resp.StatusCode) {
					// Account-attributable 4xx (401/403/408/418/451/…): exclude
					// this account for the rest of THIS request and try the next
					// (see proxy.go's shouldRetryStatus for the full rationale;
					// no Cooldown, exclude-based retry advances the pick).
					//
					// A 403 is special-cased: two consecutive 403s auto-disable
					// the account (RecordForbidden) — a sustained 403 from
					// dashscope is an account billing/authorization problem, not
					// a transient blip. Symmetric with proxy.go's OpenAI path.
					attempts = append(attempts, reqlog.Attempt{
						AccountID: acc.ID, Alias: acc.DisplayAlias(),
						Status: resp.StatusCode, Outcome: "4xx_retry", DurMs: time.Since(attemptStart).Milliseconds(),
					})
					if resp.StatusCode == 403 {
						streak, disabled := p.RecordForbidden(acc)
						p.Cooldown(acc, 60*time.Second)
						if disabled {
							log.Printf("[anthropic] account %s (id=%d) auto-disabled after %d consecutive 403s (billing/credential problem)", acc.DisplayAlias(), acc.ID, streak)
						}
					}
					resp.Body.Close()
					tried = append(tried, acc.ID)
					continue
				}
				// Request-level 4xx (400/404/413/422) or any 5xx: not account-
				// attributable, so switching accounts won't help — pass the
				// upstream response through as-is.
				aOutcome := "4xx_pass"
				if resp.StatusCode >= 500 {
					aOutcome = "5xx_pass"
				}
				attempts = append(attempts, reqlog.Attempt{
					AccountID: acc.ID, Alias: acc.DisplayAlias(),
					Status: resp.StatusCode, Outcome: aOutcome, DurMs: time.Since(attemptStart).Milliseconds(),
				})
				defer resp.Body.Close()
				o := stats.ClientError
				if resp.StatusCode >= 500 {
					o = stats.ServerError
				}
				finalAcc = acc
				finalOutcome = o
				c.DataFromReader(resp.StatusCode, resp.ContentLength,
					resp.Header.Get("Content-Type"), resp.Body, nil)
				return
			}
		}
		// Pool exhausted. Last resort: fallback channels eligible for the
		// ORIGINAL client model. The fallback endpoint is OpenAI-format, so we
		// re-marshal the already-translated req with origModel (model_override is
		// dashscope-only) and reuse the same SSE/non-stream translators that turn
		// an OpenAI 200 back into Anthropic events/bodies.
		if fbs != nil {
			req.Model = origModel
			fbBody, _ := json.Marshal(req)
			ok := func(c *gin.Context, resp *http.Response) (*int64, *tokenUsage) {
				defer resp.Body.Close()
				if areq.Stream {
					return streamAnthropicResponse(c, resp.Body, origModel, lg, start)
				}
				return nil, writeAnthropicResponse(c, resp.Body, origModel, lg)
			}
			if served, name, t, u := tryFallback(c, fbs, origModel, fbBody, ok, &attempts); served {
				finalFb = name
				finalOutcome = stats.OK
				ttft, usage = t, u
				return
			}
		}

		noAccounts = true
		c.JSON(503, gin.H{"error": "no available accounts"})
	}
}

// anthropicSystemContent converts the Anthropic top-level system field (a plain
// string or an array of blocks) into an OpenAI content value: a plain string.
// cache_control markers are intentionally dropped — dashscope's prefix cache
// keys on the tokenized prompt prefix, and array-form content (preserving
// cache_control) tokenizes differently from the plain string OpenAI clients
// send, which silently breaks cache_read when an Anthropic SDK client (which
// attaches cache_control by default) is routed through this translator.
// dashscope applies prefix caching to the leading text regardless of form, so
// the plain-string form maximizes cache hits. The same reasoning applies to
// per-message text blocks in anthropicMsgToOAI (merged into a plain string).
func anthropicSystemContent(raw json.RawMessage) json.RawMessage {
	return jsonString(rawToText(raw))
}

// anthropicMsgToOAI converts one Anthropic message into one or more OpenAI
// messages. Text blocks are merged into a plain-string content value
// (cache_control markers dropped — see anthropicSystemContent).
func anthropicMsgToOAI(role string, rawContent json.RawMessage) []oaiMessage {
	var s string
	if json.Unmarshal(rawContent, &s) == nil {
		return []oaiMessage{{Role: role, Content: jsonString(s)}}
	}

	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
	}
	if json.Unmarshal(rawContent, &blocks) != nil {
		return []oaiMessage{{Role: role, Content: jsonString(string(rawContent))}}
	}

	switch role {
	case "user":
		var result []oaiMessage
		var textParts []string
		for _, b := range blocks {
			switch b.Type {
			case "tool_result":
				result = append(result, oaiMessage{
					Role:       "tool",
					ToolCallID: b.ToolUseID,
					Content:    jsonString(rawToText(b.Content)),
				})
			case "text":
				textParts = append(textParts, b.Text)
			}
		}
		if len(textParts) > 0 {
			result = append(result, oaiMessage{Role: "user", Content: jsonString(strings.Join(textParts, ""))})
		}
		return result

	case "assistant":
		var textParts []string
		var toolCalls []oaiToolCall
		for _, b := range blocks {
			switch b.Type {
			case "text":
				textParts = append(textParts, b.Text)
			case "tool_use":
				args := "{}"
				if len(b.Input) > 0 {
					args = string(b.Input)
				}
				toolCalls = append(toolCalls, oaiToolCall{
					ID:   b.ID,
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: b.Name, Arguments: args},
				})
			}
		}
		msg := oaiMessage{Role: "assistant", Content: jsonString(strings.Join(textParts, ""))}
		if len(toolCalls) > 0 {
			msg.ToolCalls = toolCalls
		}
		return []oaiMessage{msg}
	}

	return []oaiMessage{{Role: role, Content: jsonString(rawToText(rawContent))}}
}

func rawToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "")
	}
	return string(raw)
}

func writeAnthropicResponse(c *gin.Context, body io.Reader, model string, lg *reqlog.Logger) (usage *tokenUsage) {
	var resp struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
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
	data, _ := io.ReadAll(body)
	json.Unmarshal(data, &resp) //nolint
	if lg.Enabled() {
		// Reuse the already-parsed resp.Usage instead of a second full-body
		// extractUsage(data) unmarshal; the cache-field precedence lives in
		// tokenUsageFromFields, shared with extractUsage for the streaming paths.
		u := resp.Usage
		usage = tokenUsageFromFields(u.PromptTokens, u.Completion, u.TotalTokens, u.CachedTokens, u.PromptDetails.CachedTokens, u.CacheRead)
	}

	stopReason := "end_turn"
	var content []map[string]any

	if len(resp.Choices) > 0 {
		ch := resp.Choices[0]
		switch ch.FinishReason {
		case "length":
			stopReason = "max_tokens"
		case "tool_calls":
			stopReason = "tool_use"
		}
		if ch.Message.Content != "" {
			content = append(content, map[string]any{"type": "text", "text": ch.Message.Content})
		}
		for _, tc := range ch.Message.ToolCalls {
			var input any
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
				input = map[string]any{}
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": input,
			})
		}
	}

	if content == nil {
		content = []map[string]any{}
	}

	cacheRead := 0
	if usage != nil {
		cacheRead = usage.CachedTokens
	}
	c.JSON(200, gin.H{
		"id":          "msg_" + resp.ID,
		"type":        "message",
		"role":        "assistant",
		"content":     content,
		"model":       model,
		"stop_reason": stopReason,
		// Anthropic clients expect cache_creation/cache_read token splits. We
		// only get one cached figure from dashscope; report it as cache_read
		// (cache hits, not creation) and zero creation.
		"usage": gin.H{
			"input_tokens":                resp.Usage.PromptTokens,
			"output_tokens":               resp.Usage.Completion,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     cacheRead,
		},
	})
	return usage
}

func streamAnthropicResponse(c *gin.Context, body io.Reader, model string, lg *reqlog.Logger, start time.Time) (ttft *int64, usage *tokenUsage) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(200)
	fl, _ := c.Writer.(http.Flusher)

	emit := func(event string, v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, data)
		if fl != nil {
			fl.Flush()
		}
	}

	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant",
			"content": []any{}, "model": model, "stop_reason": nil,
			"usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
	// Block 0 is always text
	emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]string{"type": "text", "text": ""},
	})
	emit("ping", map[string]string{"type": "ping"})

	// toolBlocks: OAI tool_call index → our block index (starting at 1)
	toolBlocks := map[int]int{}
	nextBlockIdx := 1
	stopReason, outTokens := "end_turn", 0

	// Read upstream SSE with an unbounded bufio.Reader rather than a capped
	// bufio.Scanner: a Scanner with a 512KB token limit silently truncates any
	// `data:` line larger than that (large tool_use argument blobs, or a
	// compatible-mode endpoint emitting a short completion as one line) — Scan()
	// returns false with bufio.ErrTooLong, the loop exits, and the post-loop
	// emits would mask the truncation as a clean end_turn with input_tokens=0.
	// ReadString keeps the trailing newline; we parse the data: prefix robustly
	// (accepting both "data: " and "data:" framing) and surface a non-EOF read
	// error as an Anthropic error event instead of a synthetic clean stop.
	br := bufio.NewReader(body)
	var readErr error
loop:
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil && rerr != io.EOF {
			readErr = rerr
		}
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "data:") {
			// comment / event framing we don't care about; keep draining
			if rerr != nil {
				break loop
			}
			continue
		}
		if trimmed == "" {
			if rerr != nil {
				break loop
			}
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "[DONE]" {
			break loop
		}
		// The final usage chunk has an empty choices array, so capture usage
		// before the choices-empty continue below would skip it.
		if lg.Enabled() && usage == nil {
			if u := extractUsage([]byte(payload)); u != nil {
				usage = u
			}
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil || len(chunk.Choices) == 0 {
			if rerr != nil {
				break loop
			}
			continue
		}
		ch := chunk.Choices[0]
		if ch.FinishReason != nil {
			switch *ch.FinishReason {
			case "length":
				stopReason = "max_tokens"
			case "tool_calls":
				stopReason = "tool_use"
			}
			if rerr != nil {
				break loop
			}
			continue
		}
		if ch.Delta.Content != "" {
			if ttft == nil {
				t := time.Since(start).Milliseconds()
				ttft = &t
			}
			outTokens++
			emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]string{"type": "text_delta", "text": ch.Delta.Content},
			})
		}
		for _, tc := range ch.Delta.ToolCalls {
			blockIdx, exists := toolBlocks[tc.Index]
			if !exists {
				blockIdx = nextBlockIdx
				toolBlocks[tc.Index] = blockIdx
				nextBlockIdx++
				emit("content_block_start", map[string]any{
					"type": "content_block_start", "index": blockIdx,
					"content_block": map[string]any{
						"type": "tool_use", "id": tc.ID,
						"name": tc.Function.Name, "input": map[string]any{},
					},
				})
			}
			if tc.Function.Arguments != "" {
				emit("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": blockIdx,
					"delta": map[string]string{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
				})
			}
		}
		if rerr != nil {
			break loop
		}
	}

	// Close text block
	emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	// Close tool blocks in index order
	blockIndices := make([]int, 0, len(toolBlocks))
	for _, bi := range toolBlocks {
		blockIndices = append(blockIndices, bi)
	}
	sort.Ints(blockIndices)
	for _, bi := range blockIndices {
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": bi})
	}

	// A non-EOF upstream read error means the stream was cut short (network
	// drop, premature close). Surface it to the client as an Anthropic error
	// event with stop_reason "error" rather than masking it as end_turn —
	// otherwise the client sees a cleanly-terminated but content-truncated
	// message with zero input_tokens.
	if readErr != nil {
		stopReason = "error"
		emit("error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": "upstream stream read error",
			},
		})
	}

	// Prefer upstream-reported token counts over our chunk-count heuristic
	// (outTokens counts content deltas, not tokens) when usage was captured.
	outToks := outTokens
	inToks, cacheRead := 0, 0
	if usage != nil {
		if usage.CompletionTokens > 0 {
			outToks = usage.CompletionTokens
		}
		inToks, cacheRead = usage.PromptTokens, usage.CachedTokens
	}
	emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]string{"stop_reason": stopReason},
		"usage": map[string]int{"input_tokens": inToks, "output_tokens": outToks, "cache_read_input_tokens": cacheRead},
	})
	emit("message_stop", map[string]string{"type": "message_stop"})
	return ttft, usage
}
