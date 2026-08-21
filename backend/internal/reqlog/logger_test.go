package reqlog

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
)

// TestEventAttemptsNeverNull guards the data contract the frontend relies on:
// a zero-attempt event (e.g. "no available accounts" — pool empty, the retry
// loop never ran so Attempts stays nil) must serialize attempts as [] not
// null, because the dashboard's RequestLogStream row calls l.attempts.map /
// .some without a null guard and a null would white-screen the whole app
// (no error boundary). Go marshals a nil slice to null, so BOTH serving
// paths must normalize nil → []: the in-memory ring (SSE replay + live
// fan-out) and the SQLite history (/admin/logs).
func TestEventAttemptsNeverNull(t *testing.T) {
	lg, err := New(filepath.Join(t.TempDir(), "reqlog.db"), 8, 7, true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer lg.Close()

	// A no-accounts event: Attempts left nil, exactly as proxy.go / anthropic.go
	// build it when p.Pick returns nil and the retry loop breaks immediately.
	lg.Log(Event{ID: "ev1", Ts: 1, Proto: "openai", Outcome: "no_accounts"})
	lg.Sync() // flush to ring + DB

	check := func(label string, ev Event) {
		t.Helper()
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("%s marshal: %v", label, err)
		}
		if bytes.Contains(b, []byte(`"attempts":null`)) {
			t.Fatalf("%s marshaled attempts as null (frontend would crash): %s", label, b)
		}
		if !bytes.Contains(b, []byte(`"attempts":[]`)) {
			t.Fatalf("%s did not marshal attempts as []: %s", label, b)
		}
	}

	// SSE replay path (in-memory ring populated by Log → loop → pushRing).
	replay, _, cancel := lg.Subscribe()
	defer cancel()
	if len(replay) != 1 {
		t.Fatalf("ring replay: expected 1 event, got %d", len(replay))
	}
	check("ring", replay[0])

	// /admin/logs history path (SQLite, round-tripped through insertBatch/Recent).
	hist, err := lg.Recent(0, 10, 0, "", "")
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("history: expected 1 event, got %d", len(hist))
	}
	check("history", hist[0])
}
