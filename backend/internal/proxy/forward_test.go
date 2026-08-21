package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestForward_SuccessNoRetry: a healthy upstream returns 200 on the first
// attempt; forward must surface the response with retried=false (no stats
// noise) and no error.
func TestForward_SuccessNoRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	fr := forward(context.Background(), srv.URL+"/chat/completions", []byte("{}"), "sk-test")
	if fr.err != nil {
		t.Fatalf("err: %v", fr.err)
	}
	defer fr.resp.Body.Close()
	if fr.retried {
		t.Fatal("retried=true on a clean first attempt; want false")
	}
	if fr.resp.StatusCode != 200 {
		t.Fatalf("status=%d want 200", fr.resp.StatusCode)
	}
}

// TestForward_RetriesSameAccountOnce: the first upstream attempt fails at the
// transport layer (connection reset mid-write) and the second succeeds. The
// client must see the success response, retried must be true so the caller
// records a network-retry stat — but the request itself is finalized as OK,
// not as an error, so the success-rate denominator is untouched.
func TestForward_RetriesSameAccountOnce(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			// Hijack the connection and forcibly close it to simulate a
			// mid-response transport failure the way a real network blip
			// would (not a clean HTTP error).
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("server does not support hijack")
			}
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// Keep netRetryDelay short so the test is fast.
	orig := netRetryDelay
	netRetryDelay = 5 * time.Millisecond
	t.Cleanup(func() { netRetryDelay = orig })

	fr := forward(context.Background(), srv.URL+"/chat/completions", []byte("{}"), "sk-test")
	if fr.err != nil {
		t.Fatalf("err after retry: %v", fr.err)
	}
	defer fr.resp.Body.Close()
	if !fr.retried {
		t.Fatal("retried=false; want true (first attempt failed, retry succeeded)")
	}
	if fr.resp.StatusCode != 200 {
		t.Fatalf("status=%d want 200", fr.resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("upstream called %d times; want 2 (initial + 1 retry)", got)
	}
}

// TestForward_GivesUpAfterTwoFailures: when both the initial attempt and the
// in-place retry fail at the transport layer, forward returns the error and a
// nil response. The caller then excludes this account and advances to the next.
func TestForward_GivesUpAfterTwoFailures(t *testing.T) {
	// A closed server → connection refused every time.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	orig := netRetryDelay
	netRetryDelay = 5 * time.Millisecond
	t.Cleanup(func() { netRetryDelay = orig })

	fr := forward(context.Background(), srv.URL+"/chat/completions", []byte("{}"), "sk-test")
	if fr.err == nil {
		t.Fatal("err=nil after two failures; want err")
	}
	if fr.resp != nil {
		t.Fatalf("resp non-nil after failure: %v", fr.resp)
	}
	if !fr.retried {
		t.Fatal("retried=false; want true (in-place retry fired)")
	}
}

// TestForward_NoRetryOnCancelledContext: if the client context is already
// cancelled when the first attempt fails, forward must NOT spend a backoff +
// retry on a dead request — it returns the original error immediately.
func TestForward_NoRetryOnCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // dead before we start

	orig := netRetryDelay
	netRetryDelay = 200 * time.Millisecond // long; the test asserts we don't wait this
	t.Cleanup(func() { netRetryDelay = orig })

	start := time.Now()
	fr := forward(ctx, srv.URL+"/chat/completions", []byte("{}"), "sk-test")
	elapsed := time.Since(start)

	if fr.err == nil {
		t.Fatal("err=nil; want err (server is closed)")
	}
	if fr.retried {
		t.Fatal("retried=true on a cancelled context; want false")
	}
	if elapsed >= netRetryDelay {
		t.Fatalf("waited %v for the backoff on a cancelled context; want immediate return", elapsed)
	}
}
