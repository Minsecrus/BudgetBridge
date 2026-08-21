package proxy

import (
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"budgetbridge/internal/reqlog"

	"github.com/gin-gonic/gin"
)

// fakeSSE returns a reader that yields a normal OpenAI streaming completion,
// a final usage chunk (with a cache hit), then `data: [DONE]` — exactly what
// dashscope sends when include_usage is on. We feed it straight into
// streamAnthropicResponse to verify the Anthropic translation terminates
// correctly AND that token/cache usage is captured.
func fakeSSE() string {
	return strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
		`data: {"choices":[{"delta":{"content":", world"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"cached_tokens":8}}`,
		`data: [DONE]`,
		"",
	}, "\n")
}

func TestStreamAnthropicResponse_TerminatesAndCapturesUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/messages", strings.NewReader(""))

	lg, err := reqlog.New(filepath.Join(t.TempDir(), "rl.db"), 10, 7, true)
	if err != nil {
		t.Fatalf("reqlog.New: %v", err)
	}
	defer lg.Close()

	ttft, usage := streamAnthropicResponse(c, strings.NewReader(fakeSSE()), "claude-test", lg, time.Now())

	body := w.Body.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		`"text":"Hello"`,
		`"text":", world"`,
		"event: content_block_stop",
		"event: message_delta",
		`"stop_reason":"end_turn"`,
		"event: message_stop",
		// usage reflected into the Anthropic message_delta (cache hit surfaced
		// as cache_read_input_tokens).
		`"output_tokens":3`,
		`"input_tokens":12`,
		`"cache_read_input_tokens":8`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in stream output:\n%s", want, body)
		}
	}
	// The terminal event must be message_stop, and nothing after it.
	idx := strings.LastIndex(body, "event: message_stop")
	if idx < 0 {
		t.Fatalf("no message_stop in:\n%s", body)
	}
	tail := body[idx:]
	if strings.Contains(tail, "[DONE]") {
		t.Errorf("translation leaked a raw [DONE] marker:\n%s", tail)
	}
	// Captured observability signals returned to the handler for logging.
	if usage == nil || usage.PromptTokens != 12 || usage.CachedTokens != 8 || usage.CompletionTokens != 3 {
		t.Fatalf("usage capture wrong: %+v", usage)
	}
	if ttft == nil {
		t.Fatal("TTFT not captured")
	}
}

// TestStreamAnthropicResponse_LineExceedingScannerCap guards against a
// regression of the old bufio.Scanner(512KB cap) truncation: a single data:
// line larger than 512KB (a big tool_use argument blob, or a compatible-mode
// endpoint emitting the whole completion as one line) used to make Scan()
// return false with bufio.ErrTooLong, silently ending the stream and emitting
// a synthetic message_stop with input_tokens=0. With bufio.Reader the line is
// parsed in full and its content delta is emitted.
func TestStreamAnthropicResponse_LineExceedingScannerCap(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/messages", strings.NewReader(""))

	lg, err := reqlog.New(filepath.Join(t.TempDir(), "rl.db"), 10, 7, true)
	if err != nil {
		t.Fatalf("reqlog.New: %v", err)
	}
	defer lg.Close()

	// Build a single data: line carrying a content delta well over 512KB.
	big := strings.Repeat("A", 600*1024)
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"` + big + `"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":600,"total_tokens":605}}`,
		`data: [DONE]`,
		"",
	}, "\n")

	ttft, usage := streamAnthropicResponse(c, strings.NewReader(stream), "claude-test", lg, time.Now())
	if ttft == nil {
		t.Fatal("TTFT not captured for oversized line")
	}
	if usage == nil || usage.PromptTokens != 5 {
		t.Fatalf("usage not captured past the oversized line: %+v", usage)
	}
	body := w.Body.String()
	// The full oversized content must have been emitted, not truncated.
	if !strings.Contains(body, big) {
		t.Errorf("oversized content delta was truncated (body len=%d, want %d)", len(body), len(big))
	}
	// And the stream must still terminate cleanly.
	if !strings.Contains(body, "event: message_stop") {
		t.Errorf("missing message_stop after oversized line:\n%s", body)
	}
}

// errReader returns its payload once, then a non-EOF error on the next read,
// simulating a mid-stream upstream connection drop.
type errReader struct {
	payload []byte
	read    int
	err     error
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.read >= len(r.payload) {
		return 0, r.err
	}
	n := copy(p, r.payload[r.read:])
	r.read += n
	return n, nil
}

// TestStreamAnthropicResponse_ReadErrorEmitsErrorEvent verifies that a
// non-EOF upstream read error is surfaced to the client as an Anthropic
// error event with stop_reason "error", rather than masked as a clean
// end_turn (the old scanner-path behavior swallowed read errors entirely).
func TestStreamAnthropicResponse_ReadErrorEmitsErrorEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/messages", strings.NewReader(""))

	lg, err := reqlog.New(filepath.Join(t.TempDir(), "rl.db"), 10, 7, true)
	if err != nil {
		t.Fatalf("reqlog.New: %v", err)
	}
	defer lg.Close()

	payload := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"partial"}}]}`,
		"\n",
	}, "")
	r := &errReader{payload: []byte(payload), err: errors.New("upstream connection reset")}

	streamAnthropicResponse(c, r, "claude-test", lg, time.Now())
	body := w.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Errorf("expected an error event on non-EOF read error, got:\n%s", body)
	}
	if !strings.Contains(body, `"stop_reason":"error"`) {
		t.Errorf("expected stop_reason=error in message_delta, got:\n%s", body)
	}
}
