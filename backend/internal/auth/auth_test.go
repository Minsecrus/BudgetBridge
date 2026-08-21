package auth

import (
	"testing"
	"time"
)

func TestCleanupEvictsExpired(t *testing.T) {
	mu.Lock()
	tokens["expired-token"] = time.Now().Add(-time.Hour)
	tokens["live-token"] = time.Now().Add(time.Hour)
	mu.Unlock()

	cleanup()

	mu.Lock()
	_, hasExpired := tokens["expired-token"]
	_, hasLive := tokens["live-token"]
	mu.Unlock()

	if hasExpired {
		t.Fatal("expired token was not evicted")
	}
	if !hasLive {
		t.Fatal("live token was wrongly evicted")
	}
}

func TestValidEvictsExpiredLazily(t *testing.T) {
	mu.Lock()
	tokens["stale"] = time.Now().Add(-time.Minute)
	mu.Unlock()

	if Valid("stale") {
		t.Fatal("expired token reported valid")
	}

	mu.Lock()
	_, ok := tokens["stale"]
	mu.Unlock()
	if ok {
		t.Fatal("Valid did not lazily evict the expired token")
	}
}
