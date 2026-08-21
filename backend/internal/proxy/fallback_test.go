package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"budgetbridge/internal/fallback"
	"budgetbridge/internal/reqlog"

	"github.com/gin-gonic/gin"
)

// TestTryFallbackServesEligibleChannel: a pool-exhausted request whose model is
// in a channel's whitelist is forwarded to that channel and the 200 is served.
func TestTryFallbackServesEligibleChannel(t *testing.T) {
	var hitURL string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitURL = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	}))
	defer up.Close()

	s := fallback.New([]fallback.Config{
		{Name: "stub", BaseURL: up.URL, APIKey: "k", Models: []string{"test-model"}},
	})
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"test-model"}`)))

	var attempts []reqlog.Attempt
	var gotBody []byte
	ok := func(c *gin.Context, resp *http.Response) (*int64, *tokenUsage) {
		defer resp.Body.Close()
		gotBody, _ = io.ReadAll(resp.Body)
		return nil, nil
	}
	served, alias, _, _ := tryFallback(c, s, "test-model", []byte(`{"model":"test-model"}`), ok, &attempts)

	if !served {
		t.Fatal("served=false, want true")
	}
	if alias != "stub" {
		t.Fatalf("alias=%q want stub", alias)
	}
	if hitURL != "/chat/completions" {
		t.Fatalf("upstream hit %q want /chat/completions", hitURL)
	}
	if !bytes.Contains(gotBody, []byte(`"content":"hi"`)) {
		t.Fatalf("ok handler did not receive the 200 body: %s", gotBody)
	}
	if len(attempts) != 1 || attempts[0].Status != 200 || attempts[0].Alias != "stub" || attempts[0].Outcome != "ok" {
		t.Fatalf("attempts=%+v want one ok stub attempt", attempts)
	}
}

// TestTryFallbackSkipsNonMatchingModel: a model not in any channel's whitelist
// yields served=false with no upstream call (no channel is eligible).
func TestTryFallbackSkipsNonMatchingModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be hit for a non-matching model")
	}))
	defer up.Close()

	s := fallback.New([]fallback.Config{
		{Name: "stub", BaseURL: up.URL, APIKey: "k", Models: []string{"only-this"}},
	})
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	var attempts []reqlog.Attempt
	served, _, _, _ := tryFallback(c, s, "other-model", []byte("{}"), nil, &attempts)
	if served {
		t.Fatal("served=true for non-matching model, want false")
	}
	if len(attempts) != 0 {
		t.Fatalf("attempts=%+v want none", attempts)
	}
}

// TestTryFallbackTriesNextOnFailure: a 500 from the first eligible channel must
// advance to the next, which serves 200. Both attempts are recorded.
func TestTryFallbackTriesNextOnFailure(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, `{}`)
	}))
	defer up.Close()

	s := fallback.New([]fallback.Config{
		{Name: "bad", BaseURL: up.URL, APIKey: "k-bad", Models: []string{"m"}},
		{Name: "good", BaseURL: up.URL, APIKey: "k-good", Models: []string{"m"}},
	})
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	var attempts []reqlog.Attempt
	ok := func(c *gin.Context, resp *http.Response) (*int64, *tokenUsage) {
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return nil, nil
	}
	served, alias, _, _ := tryFallback(c, s, "m", []byte("{}"), ok, &attempts)
	if !served || alias != "good" {
		t.Fatalf("served=%v alias=%q want served=true alias=good", served, alias)
	}
	if len(attempts) != 2 || attempts[0].Status != 500 || attempts[1].Status != 200 {
		t.Fatalf("attempts=%+v want [500, 200]", attempts)
	}
}
