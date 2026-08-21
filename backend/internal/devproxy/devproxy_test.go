package devproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestEnabled(t *testing.T) {
	os.Unsetenv("BB_DEV")
	if Enabled() {
		t.Fatal("Enabled()=true; want false when BB_DEV unset")
	}
	t.Setenv("BB_DEV", "1")
	if !Enabled() {
		t.Fatal("Enabled()=false; want true when BB_DEV=1")
	}
	t.Setenv("BB_DEV", "0")
	if Enabled() {
		t.Fatal("Enabled()=true; want false when BB_DEV=0")
	}
}

func TestShouldProxy(t *testing.T) {
	cases := []struct {
		name, method, conn, upg string
		want                    bool
	}{
		{"get", http.MethodGet, "", "", true},
		{"head", http.MethodHead, "", "", false},
		{"post", http.MethodPost, "", "", false},
		{"ws_upgrade", http.MethodGet, "Upgrade", "websocket", true},
		{"ws_upgrade_nonsafe", http.MethodPost, "Upgrade", "websocket", true},
		{"non_ws_upgrade", http.MethodPost, "Upgrade", "h2c", false},
		{"ws_without_connection", http.MethodPost, "", "websocket", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, "/", nil)
			if c.conn != "" {
				req.Header.Set("Connection", c.conn)
			}
			if c.upg != "" {
				req.Header.Set("Upgrade", c.upg)
			}
			if got := ShouldProxy(req); got != c.want {
				t.Fatalf("ShouldProxy(%s)=%v; want %v", c.name, got, c.want)
			}
		})
	}
}

func TestNew_ProxiesGet(t *testing.T) {
	called := false
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("X-Vite", "1")
		_, _ = w.Write([]byte("vite-content"))
	}))
	defer fake.Close()

	proxy, err := New(fake.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/src/main.tsx", nil)
	proxy.ServeHTTP(rec, req)

	if !called {
		t.Fatal("request not forwarded to fake vite")
	}
	if rec.Body.String() != "vite-content" {
		t.Fatalf("body=%q; want vite-content", rec.Body.String())
	}
}

func TestNew_BadURL(t *testing.T) {
	if _, err := New("://bad"); err == nil {
		t.Fatal("New(bad url)=nil err; want err")
	}
}

func TestMaybeProxy(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("from-vite"))
	}))
	defer fake.Close()

	proxy, _ := New(fake.URL)

	// A real httptest.Server is used (not httptest.NewRecorder) because
	// httputil.ReverseProxy requires the server ResponseWriter to implement
	// http.CloseNotifier; gin's responseWriter assumes its underlying writer
	// does too, and httptest.ResponseRecorder does not.
	buildServer := func(devOn bool) (*httptest.Server, *bool) {
		fallbackHit := new(bool)
		if devOn {
			t.Setenv("BB_DEV", "1")
		} else {
			os.Unsetenv("BB_DEV")
		}
		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.NoRoute(MaybeProxy(proxy, func(c *gin.Context) {
			*fallbackHit = true
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		}))
		return httptest.NewServer(r), fallbackHit
	}

	t.Run("dev_on_get_to_vite", func(t *testing.T) {
		ts, fb := buildServer(true)
		defer ts.Close()
		resp, err := http.Get(ts.URL + "/")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if *fb {
			t.Fatal("fallback hit; want vite")
		}
		if string(body) != "from-vite" {
			t.Fatalf("body=%q; want from-vite", string(body))
		}
	})

	t.Run("dev_on_post_to_fallback", func(t *testing.T) {
		ts, fb := buildServer(true)
		defer ts.Close()
		resp, err := http.Post(ts.URL+"/x/chat/completions", "application/json", nil)
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		defer resp.Body.Close()
		if !*fb {
			t.Fatal("fallback not hit; want fallback for POST")
		}
	})

	t.Run("dev_off_get_to_fallback", func(t *testing.T) {
		ts, fb := buildServer(false)
		defer ts.Close()
		resp, err := http.Get(ts.URL + "/")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer resp.Body.Close()
		if !*fb {
			t.Fatal("fallback not hit; want fallback when dev off")
		}
	})
}
