package stats

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Init(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRecordAccountAndGlobal(t *testing.T) {
	s := newTestStore(t)
	s.RecordAccount(0, OK)
	s.RecordAccount(0, Throttled)
	s.RecordAccount(0, ClientError)
	s.RecordGlobal(OK)
	s.Sync()

	g, err := s.GlobalAggregate(Window24h)
	if err != nil {
		t.Fatal(err)
	}
	if g.Req != 1 || g.Ok != 1 {
		t.Fatalf("global = %+v, want {Req:1 Ok:1}", g)
	}

	a, err := s.AccountAggregate(0, Window24h)
	if err != nil {
		t.Fatal(err)
	}
	if a.Req != 3 || a.Ok != 1 || a.R429 != 1 || a.Err != 1 {
		t.Fatalf("account = %+v, want Req:3 Ok:1 R429:1 Err:1", a)
	}
}

func TestRetentionDropsOldData(t *testing.T) {
	s := newTestStore(t)
	old := nowBucket() - (retentionDays*24*60 + 10)
	if _, err := s.db.Exec(
		`INSERT INTO global_minute(bucket,req,ok) VALUES(?, 999, 999)`, old); err != nil {
		t.Fatal(err)
	}
	s.retention()
	g, err := s.GlobalAggregate(Window7d)
	if err != nil {
		t.Fatal(err)
	}
	if g.Req != 0 {
		t.Fatalf("retention failed: global req = %d, want 0", g.Req)
	}
}

func TestRecordBalance(t *testing.T) {
	s := newTestStore(t)
	s.RecordBalance(0, 12.5, 3)
	s.Sync()
	pts, err := s.BalanceHistory(0, Window7d)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 || pts[0].Balance != 12.5 || pts[0].Coupons != 3 {
		t.Fatalf("balance pts = %+v", pts)
	}
}

func TestTimelineAndSnapshot(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 3; i++ {
		s.RecordGlobal(OK)
	}
	s.Sync()
	tl, err := s.Timeline(Window1h)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl) == 0 || tl[len(tl)-1].Req < 3 {
		t.Fatalf("timeline = %+v", tl)
	}

	snap, err := s.Snapshot(Window1h)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Global.Req < 3 || len(snap.Timeline) == 0 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestSnapshotSevenDayCap(t *testing.T) {
	s := newTestStore(t)
	// 8-day-old bucket must never surface in any window (hard cap).
	old := nowBucket() - (retentionDays*24*60 + 24*60)
	if _, err := s.db.Exec(
		`INSERT INTO global_minute(bucket,req,ok) VALUES(?, 5000, 5000)`, old); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot(Window7d)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Global.Req >= 5000 {
		t.Fatalf("7d hard-cap failed: global req = %d", snap.Global.Req)
	}
}

// TestBatchMergesSameBucket verifies the consumer coalesces multiple events
// for the same (id,bucket) into a single row with merged counters.
func TestBatchMergesSameBucket(t *testing.T) {
	s := newTestStore(t)
	s.RecordAccount(1, OK)
	s.RecordAccount(1, Throttled)
	s.RecordAccount(1, ClientError)
	s.Sync()

	a, err := s.AccountAggregate(1, Window24h)
	if err != nil {
		t.Fatal(err)
	}
	if a.Req != 3 || a.Ok != 1 || a.R429 != 1 || a.Err != 1 {
		t.Fatalf("merged account = %+v, want Req:3 Ok:1 R429:1 Err:1", a)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM account_minute WHERE id=1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 coalesced row, got %d", n)
	}
}

// TestThrottleDoesNotBumpReq verifies a 429 retry (RecordThrottle) increments
// r429 only, leaving the success-rate denominator untouched.
func TestThrottleDoesNotBumpReq(t *testing.T) {
	s := newTestStore(t)
	s.RecordAccount(2, OK) // one finalized request: req=1, ok=1
	s.RecordThrottle(2)    // a 429 retry on the same account: r429+1, req unchanged
	s.RecordThrottle(2)
	s.Sync()

	a, err := s.AccountAggregate(2, Window24h)
	if err != nil {
		t.Fatal(err)
	}
	if a.Req != 1 || a.Ok != 1 || a.R429 != 2 {
		t.Fatalf("throttle = %+v, want Req:1 Ok:1 R429:2", a)
	}
}

// TestCloseFlushesPending verifies graceful shutdown drains the channel: events
// submitted without Sync() must all be persisted by Close().
func TestCloseFlushesPending(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")
	s, err := Init(path)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	for i := 0; i < 100; i++ {
		s.RecordGlobal(OK)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ro, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var req int
	if err := ro.QueryRow(`SELECT COALESCE(SUM(req),0) FROM global_minute`).Scan(&req); err != nil {
		t.Fatal(err)
	}
	if req != 100 {
		t.Fatalf("lost %d events on close (got %d)", 100-req, req)
	}
}

// TestBalanceHistoryAll verifies balanceHistoryAll groups snapshots by account
// ID in one query (the #7 fix that replaced Snapshot's per-account N+1 loop).
func TestBalanceHistoryAll(t *testing.T) {
	s := newTestStore(t)
	s.RecordBalance(1, 10, 1)
	s.RecordBalance(2, 20, 2)
	s.RecordBalance(3, 30, 3)
	s.Sync()
	all, err := s.balanceHistoryAll(Window7d)
	if err != nil {
		t.Fatal(err)
	}
	// One query returns every account grouped by id (was N queries in Snapshot).
	if len(all) != 3 || all[1][0].Balance != 10 || all[2][0].Balance != 20 || all[3][0].Balance != 30 {
		t.Fatalf("balanceHistoryAll = %+v, want 3 accounts grouped by id", all)
	}
}

// TestStatsDroppedObservability verifies the drop counter send bumps when the
// event channel is full: SwapDropped returns the running count and resets it,
// so each admin poll reports "dropped since last poll".
func TestStatsDroppedObservability(t *testing.T) {
	s := newTestStore(t)
	s.dropped.Add(3)
	s.dropped.Add(2)
	if got := s.SwapDropped(); got != 5 {
		t.Fatalf("SwapDropped = %d, want 5", got)
	}
	if got := s.SwapDropped(); got != 0 {
		t.Fatalf("SwapDropped after reset = %d, want 0", got)
	}
}

// TestNetworkRetryDoesNotBumpReq verifies a transport-failure retry
// (RecordNetworkRetry) increments net_retry only, leaving the success-rate
// denominator (req) untouched — mirroring how RecordThrottle treats 429s.
func TestNetworkRetryDoesNotBumpReq(t *testing.T) {
	s := newTestStore(t)
	s.RecordAccount(2, OK) // one finalized request: req=1, ok=1
	s.RecordGlobal(OK)
	s.RecordNetworkRetry(2) // a network blip retried on the same account
	s.RecordNetworkRetry(2)
	s.RecordGlobalNetworkRetry()
	s.Sync()

	a, err := s.AccountAggregate(2, Window24h)
	if err != nil {
		t.Fatal(err)
	}
	if a.Req != 1 || a.Ok != 1 || a.NetRetry != 2 {
		t.Fatalf("account = %+v, want Req:1 Ok:1 NetRetry:2", a)
	}

	g, err := s.GlobalAggregate(Window24h)
	if err != nil {
		t.Fatal(err)
	}
	if g.Req != 1 || g.Ok != 1 || g.NetRetry != 1 {
		t.Fatalf("global = %+v, want Req:1 Ok:1 NetRetry:1", g)
	}
}

// TestMigrateAddsNetRetryColumn verifies that a stats DB created before the
// net_retry column (simulating an existing deployment) is migrated in place:
// the column is added with DEFAULT 0, existing rows survive, and new counters
// persist. Historical req/ok are preserved.
func TestMigrateAddsNetRetryColumn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")

	// Create an old-shape DB without net_retry and insert a historical row.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`CREATE TABLE account_minute (
		id INTEGER, bucket INTEGER,
		req INTEGER DEFAULT 0, ok INTEGER DEFAULT 0,
		err INTEGER DEFAULT 0, r429 INTEGER DEFAULT 0,
		PRIMARY KEY(id, bucket));
		CREATE TABLE global_minute (
		bucket INTEGER PRIMARY KEY,
		req INTEGER DEFAULT 0, ok INTEGER DEFAULT 0,
		err INTEGER DEFAULT 0, r429 INTEGER DEFAULT 0);
		CREATE TABLE balance_snap (
		id INTEGER, ts INTEGER, balance REAL, coupons INTEGER,
		PRIMARY KEY(id, ts));`)
	if err != nil {
		t.Fatal(err)
	}
	// Use a CURRENT bucket so the row survives Init's initial retention sweep:
	// retention deletes bucket < nowBucket()-7d, so a bucket of 1 is purged
	// asynchronously by the background loop and races this test's assertion
	// (the -race ./... flake: "sql: no rows").
	b := nowBucket()
	if _, err = old.Exec(`INSERT INTO account_minute(id,bucket,req,ok) VALUES (7, ?, 5, 3)`, b); err != nil {
		t.Fatal(err)
	}
	old.Close()

	// Opening via Init runs migrateSchema → ALTER TABLE ADD COLUMN net_retry.
	s, err := Init(path)
	if err != nil {
		t.Fatalf("Init (migrate): %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Historical row survived migration.
	var req, ok int
	if err := s.db.QueryRow(`SELECT req, ok FROM account_minute WHERE id=7 AND bucket=?`, b).Scan(&req, &ok); err != nil {
		t.Fatal(err)
	}
	if req != 5 || ok != 3 {
		t.Fatalf("historical row not preserved: req=%d ok=%d", req, ok)
	}

	// New counter writes and persists.
	s.RecordNetworkRetry(7)
	s.Sync()
	var nr int
	if err := s.db.QueryRow(`SELECT net_retry FROM account_minute WHERE id=7`).Scan(&nr); err != nil {
		t.Fatal(err)
	}
	if nr != 1 {
		t.Fatalf("net_retry=%d want 1", nr)
	}
}

// TestMigrateIdxToIdBackfillsGlobalNetRetry is the regression test for the bug
// where idx→id migration returned early and skipped addColumnIfMissing, leaving
// global_minute without a net_retry column so every global flush failed. It
// constructs the exact legacy state: account_minute keyed by positional `idx`
// (no `id`) — which triggers the idx→id branch — AND a global_minute predating
// net_retry. After migration, a global flush including net_retry must succeed.
func TestMigrateIdxToIdBackfillsGlobalNetRetry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")

	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`CREATE TABLE account_minute (
		idx INTEGER, bucket INTEGER,
		req INTEGER DEFAULT 0, ok INTEGER DEFAULT 0, err INTEGER DEFAULT 0, r429 INTEGER DEFAULT 0,
		PRIMARY KEY(idx, bucket));
		CREATE TABLE global_minute (
		bucket INTEGER PRIMARY KEY,
		req INTEGER DEFAULT 0, ok INTEGER DEFAULT 0, err INTEGER DEFAULT 0, r429 INTEGER DEFAULT 0);`)
	if err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := Init(path)
	if err != nil {
		t.Fatalf("Init (migrate): %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// global_minute must now accept net_retry (the bug: column was missing).
	s.RecordGlobal(OK)
	s.RecordGlobalNetworkRetry()
	s.Sync()

	g, err := s.GlobalAggregate(Window24h)
	if err != nil {
		t.Fatal(err)
	}
	if g.Req != 1 || g.Ok != 1 || g.NetRetry != 1 {
		t.Fatalf("global = %+v, want Req:1 Ok:1 NetRetry:1 — global_minute net_retry not backfilled after idx→id migration", g)
	}
}
