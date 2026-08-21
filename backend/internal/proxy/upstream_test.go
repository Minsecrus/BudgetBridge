package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"budgetbridge/internal/pool"
	"budgetbridge/internal/reqlog"

	"github.com/gin-gonic/gin"
)

func TestEffectiveUpstream(t *testing.T) {
	const base = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	cases := []struct {
		name string
		base string
		ws   string
		want string
	}{
		{"empty_returns_base", base, "", base},
		{"host_only_swaps_keeps_path", base, "ws-abc.cn-beijing.maas.aliyuncs.com",
			"https://ws-abc.cn-beijing.maas.aliyuncs.com/compatible-mode/v1"},
		{"host_with_port", base, "127.0.0.1:54321",
			"https://127.0.0.1:54321/compatible-mode/v1"},
		{"full_url_used_verbatim", base, "https://ws-abc.cn-beijing.maas.aliyuncs.com/custom/v1",
			"https://ws-abc.cn-beijing.maas.aliyuncs.com/custom/v1"},
		{"full_url_trailing_slash_trimmed", base, "https://ws-abc/v1/", "https://ws-abc/v1"},
		{"host_trimmed_whitespace", base, "  ws-abc.aliyuncs.com  ",
			"https://ws-abc.aliyuncs.com/compatible-mode/v1"},
		{"malformed_base_falls_back", "not a url", "ws-abc.aliyuncs.com", "not a url"},
		{"empty_base_with_host_falls_back", "", "ws-abc.aliyuncs.com", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effectiveUpstream(c.base, c.ws); got != c.want {
				t.Fatalf("effectiveUpstream(%q, %q) = %q, want %q", c.base, c.ws, got, c.want)
			}
		})
	}
}

// TestEffectiveUpstream_MemoizationReducesAllocs: the cache must actually be
// consulted on repeat calls — a memoized call should allocate strictly less
// than a direct resolveUpstream call (which url.Parses + rebuilds the string
// every time). Asserting allocs (not just equal output) is what proves the
// cache engages; a non-memoized pure function would allocate identically.
func TestEffectiveUpstream_MemoizationReducesAllocs(t *testing.T) {
	const base = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	ws := "ws-alloc.cn-beijing.maas.aliyuncs.com"
	effectiveUpstream(base, ws) // prime the cache
	memoAllocs := testing.AllocsPerRun(100, func() { effectiveUpstream(base, ws) })
	directAllocs := testing.AllocsPerRun(100, func() { resolveUpstream(base, ws) })
	if memoAllocs >= directAllocs {
		t.Fatalf("memoized allocs/op %v not less than direct %v — cache not engaging", memoAllocs, directAllocs)
	}
}

// BenchmarkEffectiveUpstream_CacheHit: 热路径上每次代理尝试都会调用 effectiveUpstream；
// 命中缓存时应近似零分配，证明记忆化有效（首次解析后不再 url.Parse + 拼串）。
func BenchmarkEffectiveUpstream_CacheHit(b *testing.B) {
	const base = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	effectiveUpstream(base, "ws-bench.cn-beijing.maas.aliyuncs.com") // 预热缓存
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = effectiveUpstream(base, "ws-bench.cn-beijing.maas.aliyuncs.com")
	}
}

// TestOpenAIHandler_RoutesToWSDomain: 一个配了 ws_domain 的账号，其请求必须
// 打到 ws_domain 对应的上游，而不是全局 upstream_url。双 server：A=全局、B=ws 覆盖。
// 证明循环里确实用 effectiveUpstream(upstream, acc.WSDomain) 现算了 URL。
func TestOpenAIHandler_RoutesToWSDomain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var hitsA, hitsB int32
	okBody := `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitsA, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, okBody)
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitsB, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, okBody)
	}))
	defer srvB.Close()

	globalUpstream := srvA.URL + "/compatible-mode/v1"
	wsDomain := strings.TrimPrefix(srvB.URL, "http://") // host:port of B

	p := pool.New([]pool.AccountConfig{
		{Alias: "ws", APIKey: "k1", WSDomain: wsDomain},
	}, "round_robin", 20.0, true, 8192, 0)
	lg, err := reqlog.New("", 0, 0, false) // no-op logger（不开发 DB）
	if err != nil {
		t.Fatalf("reqlog: %v", err)
	}
	defer lg.Close()

	r := gin.New()
	r.POST("/v1/chat/completions", Handler(p, globalUpstream, "", nil, lg, nil))
	body := []byte(`{"model":"qwen-plus","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := atomic.LoadInt32(&hitsB); got != 1 {
		t.Fatalf("ws-domain upstream hits=%d, want 1", got)
	}
	if got := atomic.LoadInt32(&hitsA); got != 0 {
		t.Fatalf("global upstream hit=%d, want 0 (ws_domain should override)", got)
	}
}

// TestAnthropicHandler_RoutesToWSDomain: 与 OpenAI 路径对称——配了 ws_domain 的
// 账号经 /v1/messages 转发时也必须打到 ws_domain 上游。Anthropic handler 会把请求
// 翻译成 OpenAI 格式再 forward，这里证明 URL 选择与翻译正交、ws_domain 同样生效。
func TestAnthropicHandler_RoutesToWSDomain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var hitsA, hitsB int32
	okBody := `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitsA, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, okBody)
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitsB, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, okBody)
	}))
	defer srvB.Close()

	globalUpstream := srvA.URL + "/compatible-mode/v1"
	wsDomain := strings.TrimPrefix(srvB.URL, "http://")

	p := pool.New([]pool.AccountConfig{
		{Alias: "ws", APIKey: "k1", WSDomain: wsDomain},
	}, "round_robin", 20.0, true, 8192, 0)
	lg, err := reqlog.New("", 0, 0, false)
	if err != nil {
		t.Fatalf("reqlog: %v", err)
	}
	defer lg.Close()

	r := gin.New()
	r.POST("/v1/messages", AnthropicHandler(p, globalUpstream, "", nil, lg, nil))
	body := []byte(`{"model":"qwen-plus","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := atomic.LoadInt32(&hitsB); got != 1 {
		t.Fatalf("ws-domain upstream hits=%d, want 1", got)
	}
	if got := atomic.LoadInt32(&hitsA); got != 0 {
		t.Fatalf("global upstream hit=%d, want 0 (ws_domain should override)", got)
	}
}
