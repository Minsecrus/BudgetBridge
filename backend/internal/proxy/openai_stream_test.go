package proxy

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"budgetbridge/internal/reqlog"

	"github.com/gin-gonic/gin"
)

// fakeOpenAISSE is an OpenAI-shaped SSE stream: two content deltas, a
// finish_reason, and a final usage chunk (with a cache hit), terminated by
// [DONE] — what dashscope sends when include_usage is on. Each frame is a
// `data: …` line followed by a blank-line terminator (the real SSE framing),
// so the streamer's flush-on-blank-terminator logic is exercised.
func fakeOpenAISSE() string {
	frames := []string{
		`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
		`data: {"choices":[{"delta":{"content":", world"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"cached_tokens":8}}`,
		`data: [DONE]`,
	}
	var sb strings.Builder
	for _, f := range frames {
		sb.WriteString(f)
		sb.WriteString("\n\n") // data line + blank-line terminator
	}
	return sb.String()
}

// flushCounter is an http.ResponseWriter + http.Flusher that records every
// Flush() call so we can assert the OpenAI streamer flushes once per SSE frame
// (not once per non-empty line — the old double-flush behavior).
type flushCounter struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushCounter) Flush() { f.flushes++ }

func newFlushCounter() *flushCounter {
	return &flushCounter{ResponseRecorder: httptest.NewRecorder()}
}

func TestStreamOpenAIResponse_CapturesTTFTAndUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := newFlushCounter()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(""))

	lg, err := reqlog.New(filepath.Join(t.TempDir(), "rl.db"), 10, 7, true)
	if err != nil {
		t.Fatalf("reqlog.New: %v", err)
	}
	defer lg.Close()

	ttft, usage := streamOpenAIResponse(c, strings.NewReader(fakeOpenAISSE()), time.Now(), lg)
	if ttft == nil {
		t.Fatal("TTFT not captured")
	}
	if usage == nil || usage.PromptTokens != 12 || usage.CachedTokens != 8 || usage.CompletionTokens != 3 {
		t.Fatalf("usage capture wrong: %+v", usage)
	}
	body := w.Body.String()
	for _, want := range []string{
		`"content":"Hello"`,
		`"content":", world"`,
		`"finish_reason":"stop"`,
		`"cached_tokens":8`,
		`[DONE]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in stream output:\n%s", want, body)
		}
	}
}

// TestStreamOpenAIResponse_FlushesOncePerFrame asserts the streamer flushes
// once per SSE frame (blank-line terminator) plus one final flush, instead of
// the old per-non-empty-line behavior that flushed twice per frame (once for
// `data: ...\n`, once for `\n`).
func TestStreamOpenAIResponse_FlushesOncePerFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := newFlushCounter()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(""))

	lg, err := reqlog.New(filepath.Join(t.TempDir(), "rl.db"), 10, 7, true)
	if err != nil {
		t.Fatalf("reqlog.New: %v", err)
	}
	defer lg.Close()

	streamOpenAIResponse(c, strings.NewReader(fakeOpenAISSE()), time.Now(), lg)

	// fakeOpenAISSE has 4 data: frames + a [DONE] frame = 5 SSE events, each
	// terminated by a blank line (the trailing "" joined into a "\n\n"). So we
	// expect 5 frame-terminator flushes + 1 final flush = 6. The old per-line
	// code would have flushed ~10 times (one per non-empty line).
	if w.flushes != 6 {
		t.Errorf("flush count: got %d, want 6 (one per frame + final); body:\n%s", w.flushes, w.Body.String())
	}
}

// TestStreamOpenAIResponse_StreamWithoutTrailingBlankLine verifies the final
// post-loop flush covers a stream that ends on a data: line with no blank
// terminator (so the client still receives the last bytes).
func TestStreamOpenAIResponse_StreamWithoutTrailingBlankLine(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := newFlushCounter()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(""))

	lg, err := reqlog.New(filepath.Join(t.TempDir(), "rl.db"), 10, 7, true)
	if err != nil {
		t.Fatalf("reqlog.New: %v", err)
	}
	defer lg.Close()

	// One frame with a blank terminator, then a final data: line with NO
	// trailing newline at all (ends mid-line on EOF).
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

	ttft, usage := streamOpenAIResponse(c, strings.NewReader(stream), time.Now(), lg)
	if ttft == nil {
		t.Fatal("TTFT not captured")
	}
	if usage == nil || usage.TotalTokens != 2 {
		t.Fatalf("usage not captured on EOF-without-blank-line: %+v", usage)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"content":"a"`) {
		t.Errorf("first frame content missing:\n%s", body)
	}
	if !strings.Contains(body, `"total_tokens":2`) {
		t.Errorf("final usage chunk missing:\n%s", body)
	}
}
