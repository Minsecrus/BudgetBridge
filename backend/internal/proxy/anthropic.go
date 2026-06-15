package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"budgetbridge/internal/pool"

	"github.com/gin-gonic/gin"
)

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
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
	Model     string       `json:"model"`
	Messages  []oaiMessage `json:"messages"`
	MaxTokens int          `json:"max_tokens,omitempty"`
	Stream    bool         `json:"stream"`
	Tools     []oaiTool    `json:"tools,omitempty"`
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

func AnthropicHandler(p *pool.Pool, upstream, modelOverride string) gin.HandlerFunc {
	url := upstream + "/chat/completions"
	return func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		log.Printf("[anthropic] incoming model=%s stream=%v tools=%v",
			func() string {
				var m struct {
					Model string `json:"model"`
				}
				json.Unmarshal(body, &m)
				return m.Model
			}(),
			func() bool {
				var m struct {
					Stream bool `json:"stream"`
				}
				json.Unmarshal(body, &m)
				return m.Stream
			}(),
			func() int {
				var m struct {
					Tools []json.RawMessage `json:"tools"`
				}
				json.Unmarshal(body, &m)
				return len(m.Tools)
			}(),
		)

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
		if modelOverride != "" {
			areq.Model = modelOverride
		}

		msgs := make([]oaiMessage, 0, len(areq.Messages)+1)
		if len(areq.System) > 0 && string(areq.System) != "null" {
			msgs = append(msgs, oaiMessage{Role: "system", Content: rawToText(areq.System)})
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

		reqBody, _ := json.Marshal(oaiRequest{
			Model:     areq.Model,
			Messages:  msgs,
			MaxTokens: areq.MaxTokens,
			Stream:    areq.Stream,
			Tools:     tools,
		})

		for i := 0; i < p.Len(); i++ {
			acc := p.Next()
			if acc == nil {
				break
			}
			req, _ := http.NewRequestWithContext(c.Request.Context(), "POST", url, bytes.NewReader(reqBody))
			req.Header.Set("Authorization", "Bearer "+acc.APIKey)
			req.Header.Set("Content-Type", "application/json")

			resp, err := httpClient.Do(req)
			if err != nil {
				log.Printf("[anthropic] upstream err: %v", err)
				continue
			}
			log.Printf("[anthropic] upstream status: %d", resp.StatusCode)
			switch resp.StatusCode {
			case 429:
				resp.Body.Close()
				p.Cooldown(acc, 60*time.Second)
				continue
			case 200:
				defer resp.Body.Close()
				if areq.Stream {
					streamAnthropicResponse(c, resp.Body, areq.Model)
				} else {
					writeAnthropicResponse(c, resp.Body, areq.Model)
				}
				return
			default:
				defer resp.Body.Close()
				c.DataFromReader(resp.StatusCode, resp.ContentLength,
					resp.Header.Get("Content-Type"), resp.Body, nil)
				return
			}
		}
		c.JSON(503, gin.H{"error": "no available accounts"})
	}
}

// anthropicMsgToOAI converts one Anthropic message into one or more OpenAI messages.
func anthropicMsgToOAI(role string, rawContent json.RawMessage) []oaiMessage {
	// Plain string content
	var s string
	if json.Unmarshal(rawContent, &s) == nil {
		return []oaiMessage{{Role: role, Content: s}}
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
		return []oaiMessage{{Role: role, Content: string(rawContent)}}
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
					Content:    rawToText(b.Content),
				})
			case "text":
				textParts = append(textParts, b.Text)
			}
		}
		if len(textParts) > 0 {
			result = append(result, oaiMessage{Role: "user", Content: strings.Join(textParts, "")})
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
		msg := oaiMessage{Role: "assistant", Content: strings.Join(textParts, "")}
		if len(toolCalls) > 0 {
			msg.ToolCalls = toolCalls
		}
		return []oaiMessage{msg}
	}

	return []oaiMessage{{Role: role, Content: rawToText(rawContent)}}
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

func writeAnthropicResponse(c *gin.Context, body io.Reader, model string) {
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
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	data, _ := io.ReadAll(body)
	json.Unmarshal(data, &resp) //nolint

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

	c.JSON(200, gin.H{
		"id":          "msg_" + resp.ID,
		"type":        "message",
		"role":        "assistant",
		"content":     content,
		"model":       model,
		"stop_reason": stopReason,
		"usage":       gin.H{"input_tokens": resp.Usage.PromptTokens, "output_tokens": resp.Usage.CompletionTokens},
	})
}

func streamAnthropicResponse(c *gin.Context, body io.Reader, model string) {
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

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 512*1024), 512*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := line[6:]
		if payload == "[DONE]" {
			break
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
			continue
		}
		if ch.Delta.Content != "" {
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

	emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]string{"stop_reason": stopReason},
		"usage": map[string]int{"output_tokens": outTokens},
	})
	emit("message_stop", map[string]string{"type": "message_stop"})
}
