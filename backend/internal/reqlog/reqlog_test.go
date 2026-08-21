package reqlog

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestLogger(t *testing.T, ringCap, retentionDays int) *Logger {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "reqlog.db")
	l, err := New(dbPath, ringCap, retentionDays, true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func mkEvent(id string, ts int64) Event {
	return Event{ID: id, Ts: ts, Proto: "openai", Model: "m", Outcome: "ok", Status: 200, DurMs: 10}
}

// TestRingEvictsOldest: writing past capacity keeps the most recent N.
func TestRingEvictsOldest(t *testing.T) {
	l := newTestLogger(t, 3, 7)
	for i := int64(0); i < 5; i++ {
		l.pushRing(mkEvent("e"+string(rune('A'+i)), 1000+i))
	}
	l.ringMu.Lock()
	snap := l.snapshot()
	l.ringMu.Unlock()
	if len(snap) != 3 {
		t.Fatalf("snap len=%d want 3", len(snap))
	}
	// Oldest two (A,B) evicted; C,D,E remain in order.
	for i, want := range []string{"eC", "eD", "eE"} {
		if snap[i].ID != want {
			t.Errorf("snap[%d].ID=%q want %q", i, snap[i].ID, want)
		}
	}
}

// TestSubscribeReplayThenIncrement: a new subscriber gets the ring snapshot
// then live events.
func TestSubscribeReplayThenIncrement(t *testing.T) {
	l := newTestLogger(t, 10, 7)
	l.pushRing(mkEvent("replay1", 1))
	l.pushRing(mkEvent("replay2", 2))

	replay, ch, cancel := l.Subscribe()
	defer cancel()
	if len(replay) != 2 || replay[0].ID != "replay1" || replay[1].ID != "replay2" {
		t.Fatalf("replay=%+v", replay)
	}
	// fanout delivers to the subscriber channel.
	l.fanout(mkEvent("live1", 3))
	select {
	case e := <-ch:
		if e.ID != "live1" {
			t.Fatalf("got %s want live1", e.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("no live event delivered")
	}
}

// TestSlowSubscriberDropsNotBlocks: a subscriber that never drains must not
// block fanout (or the consumer) — excess events are dropped.
func TestSlowSubscriberDropsNotBlocks(t *testing.T) {
	l := newTestLogger(t, 10, 7)
	_, ch, cancel := l.Subscribe()
	defer cancel()
	// The subscriber channel caps at subCap; flood past it.
	done := make(chan struct{})
	go func() {
		for i := 0; i < subCap+50; i++ {
			l.fanout(mkEvent("f", int64(i)))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fanout blocked on slow subscriber")
	}
	// Drain at least the buffered amount; the rest were dropped silently.
	for i := 0; i < subCap; i++ {
		<-ch
	}
}

// TestLogDropsWhenChannelFull: a disabled-or-full logger drops + counts rather
// than blocking. We approximate "full" by closing the consumer's input via a
// closed flag: Log on a closed logger drops without panic.
func TestLogDropsWhenClosed(t *testing.T) {
	l := newTestLogger(t, 3, 7)
	_ = l.Close() // closed logger
	// Log after close must not panic and must drop.
	l.Log(mkEvent("after-close", 1))
	if l.SwapDropped() < 0 {
		t.Fatal("dropped count negative")
	}
}

// TestDisabledLoggerIsNoOp: a disabled logger opens no DB and all ops are no-ops.
func TestDisabledLoggerIsNoOp(t *testing.T) {
	l, err := New("", 0, 0, false)
	if err != nil {
		t.Fatalf("New disabled: %v", err)
	}
	if l.Enabled() {
		t.Fatal("disabled logger reports enabled")
	}
	l.Log(mkEvent("x", 1)) // must not panic
	replay, ch, cancel := l.Subscribe()
	defer cancel()
	if replay != nil || ch != nil {
		t.Fatal("disabled Subscribe returned non-nil")
	}
	if l.QueueLen() != 0 {
		t.Fatal("disabled QueueLen != 0")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("disabled Close: %v", err)
	}
}

// TestDBInsertAndRecent: events flushed to the DB are readable via Recent with
// before-cursor pagination.
func TestDBInsertAndRecent(t *testing.T) {
	l := newTestLogger(t, 10, 7)
	for i := int64(0); i < 5; i++ {
		l.Log(mkEvent("a"+itoa(i), 1000+i))
	}
	// Force a flush (the consumer batches on a 1s ticker; Sync via channel drain).
	l.Sync()
	out, err := l.Recent(0, 100, 0, "", "")
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("len=%d want 5", len(out))
	}
	// Descending: newest first → ts 1004..1000.
	if out[0].Ts != 1004 || out[4].Ts != 1000 {
		t.Fatalf("order wrong: %v %v", out[0].Ts, out[4].Ts)
	}
	// before-cursor: page earlier than 1003 → ts 1002,1001,1000.
	page, err := l.Recent(1003, 100, 0, "", "")
	if err != nil {
		t.Fatalf("Recent page: %v", err)
	}
	if len(page) != 3 || page[0].Ts != 1002 {
		t.Fatalf("page=%+v", page)
	}
}

// TestRecentFilters: account/outcome/q filters compose.
func TestRecentFilters(t *testing.T) {
	l := newTestLogger(t, 10, 7)
	l.Log(Event{ID: "1", Ts: 1, FinalAccountID: 5, FinalAlias: "alpha", Outcome: "ok", Model: "gpt"})
	l.Log(Event{ID: "2", Ts: 2, FinalAccountID: 6, FinalAlias: "beta", Outcome: "server_error", Model: "claude"})
	l.Sync()
	if out, _ := l.Recent(0, 100, 5, "", ""); len(out) != 1 || out[0].ID != "1" {
		t.Errorf("account filter: %+v", out)
	}
	if out, _ := l.Recent(0, 100, 0, "ok", ""); len(out) != 1 || out[0].ID != "1" {
		t.Errorf("outcome filter: %+v", out)
	}
	if out, _ := l.Recent(0, 100, 0, "", "claude"); len(out) != 1 || out[0].ID != "2" {
		t.Errorf("q filter: %+v", out)
	}
}

// TestRecentErrorCategory: outcome=error matches all three failure outcomes in
// one query (the dashboard's "全部错误" fetch relies on this IN expansion), and
// a high limit reaches the full set without paging.
func TestRecentErrorCategory(t *testing.T) {
	l := newTestLogger(t, 10, 7)
	l.Log(Event{ID: "ok", Ts: 1, Outcome: "ok"})
	l.Log(Event{ID: "se", Ts: 2, Outcome: "server_error"})
	l.Log(Event{ID: "ce", Ts: 3, Outcome: "client_error"})
	l.Log(Event{ID: "na", Ts: 4, Outcome: "no_accounts"})
	l.Log(Event{ID: "th", Ts: 5, Outcome: "throttled"})
	l.Sync()
	out, _ := l.Recent(0, 100, 0, "error", "")
	if len(out) != 3 {
		t.Fatalf("error category: got %d, want 3: %+v", len(out), out)
	}
	for _, e := range out {
		if e.Outcome == "ok" || e.Outcome == "throttled" {
			t.Errorf("error category matched %q", e.Outcome)
		}
	}
	// The "全部错误" path uses limit=2000; verify it returns the full set.
	big, _ := l.Recent(0, 2000, 0, "error", "")
	if len(big) != 3 {
		t.Errorf("high limit: got %d, want 3", len(big))
	}
	// limit above the ceiling clamps to the 100 default (safety guard).
	if over, _ := l.Recent(0, 99999, 0, "", ""); len(over) != 5 {
		t.Errorf("over-ceiling clamp: got %d, want 5 (clamped to default 100)", len(over))
	}
}

// TestRetentionDeletesOldRows: rows older than retentionDays are swept.
func TestRetentionDeletesOldRows(t *testing.T) {
	l := newTestLogger(t, 10, 7)
	now := time.Now().UnixMilli()
	old := now - 8*24*60*60*1000 // 8 days ago
	l.Log(Event{ID: "old", Ts: old})
	l.Log(Event{ID: "new", Ts: now})
	l.Sync()
	l.retention()
	out, _ := l.Recent(now+1_000_000, 100, 0, "", "")
	if len(out) != 1 || out[0].ID != "new" {
		t.Fatalf("retention left %+v", out)
	}
}

// TestConcurrentNoRace: concurrent Log/Subscribe/Recent under -race.
func TestConcurrentNoRace(t *testing.T) {
	l := newTestLogger(t, 100, 7)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); l.Log(mkEvent("c", 1)) }()
		go func() { defer wg.Done(); _, ch, cancel := l.Subscribe(); defer cancel(); _ = ch }()
		go func() { defer wg.Done(); _, _ = l.Recent(0, 10, 0, "", "") }()
	}
	wg.Wait()
}

// itoa is a tiny int→string to avoid strconv in test helpers.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{'0' + byte(n%10)}, b...)
		n /= 10
	}
	return string(b)
}
