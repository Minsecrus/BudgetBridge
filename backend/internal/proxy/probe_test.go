package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"budgetbridge/internal/pool"

	"github.com/gin-gonic/gin"
)

func TestProbeAccount(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		wantOK bool
	}{
		{"ok", 200, true},
		{"rate_limited", 429, false},
		{"server_error", 500, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
					t.Errorf("Authorization=%q want Bearer sk-test", got)
				}
				w.WriteHeader(c.code)
			}))
			defer fake.Close()

			status, err := probeAccount(context.Background(), fake.URL, "qwen-plus", "sk-test")
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if status != c.code {
				t.Fatalf("status=%d want %d", status, c.code)
			}
			if got := status == 200; got != c.wantOK {
				t.Fatalf("ok=%v want %v", got, c.wantOK)
			}
		})
	}
}

func TestProbeAccount_TransportError(t *testing.T) {
	// closed server → connection refused → err != nil
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	fake.Close()
	_, err := probeAccount(context.Background(), fake.URL, "qwen-plus", "sk-test")
	if err == nil {
		t.Fatal("err=nil on transport failure; want err")
	}
}

func TestTestAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer fake.Close()
	p := pool.New([]pool.AccountConfig{{Alias: "账号1", APIKey: "k1"}}, "round_robin", 20.0, true, 8192, 0)

	run := func(id string) *httptest.ResponseRecorder {
		r := gin.New()
		r.POST("/admin/accounts/:id/test", TestAccount(p, fake.URL, "qwen-plus"))
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/accounts/"+id+"/test", nil)
		r.ServeHTTP(w, req)
		return w
	}

	t.Run("valid_id_ok", func(t *testing.T) {
		w := run("1") // first account gets stable ID 1
		if w.Code != 200 {
			t.Fatalf("code=%d", w.Code)
		}
		if !strings.Contains(w.Body.String(), `"ok":true`) {
			t.Fatalf("body=%s want ok:true", w.Body.String())
		}
	})

	t.Run("invalid_id", func(t *testing.T) {
		w := run(strconv.Itoa(99))
		if w.Code != 400 {
			t.Fatalf("code=%d want 400", w.Code)
		}
	})
}

func TestTestAll(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// fake upstream: always 200
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer fake.Close()
	p := pool.New([]pool.AccountConfig{
		{Alias: "账号1", APIKey: "k1"},
		{Alias: "账号2", APIKey: "k2"},
	}, "round_robin", 20.0, true, 8192, 0)

	r := gin.New()
	r.POST("/admin/test-all", TestAll(p, fake.URL, "qwen-plus"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/test-all", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"alias":"账号1"`) || !strings.Contains(body, `"alias":"账号2"`) {
		t.Fatalf("body missing accounts: %s", body)
	}
	if !strings.Contains(body, `"ok":true`) {
		t.Fatalf("body missing ok:true: %s", body)
	}
	if strings.Index(body, "账号1") > strings.Index(body, "账号2") {
		t.Fatalf("order not preserved: %s", body)
	}
}

// TestTestAccount_UsesWSDomain: 探测配了 ws_domain 的账号时，必须打到 ws_domain
// 上游（测真实链路），而不是全局 upstream。probe_test 已有 httptest 端到端模式，沿用之。
func TestTestAccount_UsesWSDomain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var hitsA, hitsB int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitsA, 1)
		w.WriteHeader(200)
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitsB, 1)
		w.WriteHeader(200)
	}))
	defer srvB.Close()

	wsDomain := strings.TrimPrefix(srvB.URL, "http://") // host:port of B
	p := pool.New([]pool.AccountConfig{
		{Alias: "ws", APIKey: "k1", WSDomain: wsDomain},
	}, "round_robin", 20.0, true, 8192, 0)

	r := gin.New()
	r.POST("/admin/accounts/:id/test", TestAccount(p, srvA.URL, "qwen-plus"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/1/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	if got := atomic.LoadInt32(&hitsB); got != 1 {
		t.Fatalf("ws-domain upstream hits=%d, want 1", got)
	}
	if got := atomic.LoadInt32(&hitsA); got != 0 {
		t.Fatalf("global upstream hit=%d, want 0 (ws_domain should override)", got)
	}
}
