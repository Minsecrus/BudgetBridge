// Package stats persists per-account and global request metrics to SQLite
// with a rolling 7-day retention. Writes are asynchronous: hot paths drop
// events into a buffered channel and a single consumer goroutine merges +
// batches them into the DB, so proxy requests never block on disk I/O.
package stats

import (
	"database/sql"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

type Outcome int

const (
	OK Outcome = iota
	ClientError
	ServerError
	Throttled
)

type Window string

const (
	Window1h  Window = "1h"
	Window24h Window = "24h"
	Window7d  Window = "7d"
)

const retentionDays = 7

const (
	chanCap     = 16384       // event buffer; absorb bursts; drop+count when full (see dropped)
	flushBatch  = 256         // flush once this many events are merged in memory
	flushPeriod = time.Second // flush at least this often
)

// eventKind tags an event so the consumer routes it to the right merge map.
type eventKind uint8

const (
	kindAccount eventKind = iota
	kindGlobal
	kindBalance
	kindSync      // barrier: flush then signal; no DB write of its own
	kindRetention // run retention sweeps after flushing pending events
)

// statsEvent is an immutable record of one stats update.
type statsEvent struct {
	kind         eventKind
	id           int           // kindAccount/kindBalance: account stable ID
	bucket       int64         // minute bucket, computed by the caller
	col          string        // outcome column ("ok"/"err"/"r429"); "" = req only
	throttleOnly bool          // true: bump r429 only, not req (see #8)
	syncCh       chan struct{} // kindSync: closed after preceding events are flushed
	balance      float64       // kindBalance
	coupons      int           // kindBalance
}

type Store struct {
	db      *sql.DB
	ev      chan statsEvent
	wg      sync.WaitGroup
	closed  atomic.Bool
	dropped atomic.Int64 // events dropped when ev was full — surfaced via SwapDropped
}

// Agg holds request counters for one bucket or window.
type Agg struct {
	Req      int `json:"req"`
	Ok       int `json:"ok"`
	Err      int `json:"err"`
	R429     int `json:"r429"`
	NetRetry int `json:"net_retry"`
}

const schema = `
CREATE TABLE IF NOT EXISTS account_minute (
  id INTEGER, bucket INTEGER,
  req INTEGER DEFAULT 0, ok INTEGER DEFAULT 0,
  err INTEGER DEFAULT 0, r429 INTEGER DEFAULT 0, net_retry INTEGER DEFAULT 0,
  PRIMARY KEY(id, bucket));
CREATE TABLE IF NOT EXISTS global_minute (
  bucket INTEGER PRIMARY KEY,
  req INTEGER DEFAULT 0, ok INTEGER DEFAULT 0,
  err INTEGER DEFAULT 0, r429 INTEGER DEFAULT 0, net_retry INTEGER DEFAULT 0);
CREATE TABLE IF NOT EXISTS balance_snap (
  id INTEGER, ts INTEGER, balance REAL, coupons INTEGER,
  PRIMARY KEY(id, ts));
CREATE INDEX IF NOT EXISTS idx_am_bucket ON account_minute(bucket);
CREATE INDEX IF NOT EXISTS idx_bs_ts   ON balance_snap(ts);
`

func Init(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if err := migrateSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, ev: make(chan statsEvent, chanCap)}
	s.wg.Add(1)
	go s.loop()
	s.send(statsEvent{kind: kindRetention}) // initial sweep runs on the consumer
	go s.retentionLoop()
	return s, nil
}

// migrateSchema creates the id-keyed tables and, for old databases that used
// the positional `idx` column, drops + recreates account_minute/balance_snap.
// Historical stats (7-day rolling) are cleared on migration: the old idx
// values are positional and don't map to stable IDs, so keeping them would
// misattribute stats to whichever account now occupies that slot.
func migrateSchema(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	hasIdx, hasID, err := columns(db, "account_minute")
	if err != nil {
		return err
	}
	if hasIdx && !hasID {
		log.Printf("[stats] migrating idx→id (historical stats cleared)")
		for _, t := range []string{"account_minute", "balance_snap"} {
			if _, err := db.Exec(`DROP TABLE ` + t); err != nil {
				return err
			}
		}
		if _, err := db.Exec(schema); err != nil {
			return err
		}
		// Do NOT return here. The idx→id branch only recreates account_minute
		// and balance_snap; global_minute is untouched by the DROP, and a DB
		// old enough to need idx→id migration predates the net_retry column on
		// global_minute too. Falling through to addColumnIfMissing backfills
		// it. Returning early here left global_minute without net_retry, so
		// every global flush failed with "no column named net_retry".
	}
	// Add net_retry column to pre-existing tables that predate it. ALTER TABLE
	// ADD COLUMN with DEFAULT 0 backfills existing rows; id-keyed stats are
	// read with COALESCE so a missing column is already tolerated, but adding
	// the column keeps the schema honest and the INSERT/upsert paths uniform.
	if err := addColumnIfMissing(db, "account_minute", "net_retry"); err != nil {
		return err
	}
	return addColumnIfMissing(db, "global_minute", "net_retry")
}

// addColumnIfMissing adds col to table if it is not already present. SQLite
// ADD COLUMN is idempotent only when guarded by a presence check.
func addColumnIfMissing(db *sql.DB, table, col string) error {
	present, _, err := columnExists(db, table, col)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	_, err = db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s INTEGER DEFAULT 0`, table, col))
	return err
}

func columns(db *sql.DB, table string) (hasIdx, hasID bool, err error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, false, err
		}
		switch name {
		case "idx":
			hasIdx = true
		case "id":
			hasID = true
		}
	}
	return hasIdx, hasID, rows.Err()
}

// columnExists reports whether col is present on table (and returns its type).
func columnExists(db *sql.DB, table, col string) (present bool, typ string, err error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctyp string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctyp, &notnull, &dflt, &pk); err != nil {
			return false, "", err
		}
		if name == col {
			return true, ctyp, nil
		}
	}
	return false, "", rows.Err()
}

func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	close(s.ev) // consumer drains remaining events, flushes, and exits
	s.wg.Wait()
	return s.db.Close()
}

// Sync blocks until the consumer has flushed all events submitted so far.
// Tests / graceful shutdown only — do not call on the hot path.
func (s *Store) Sync() {
	if s.closed.Load() {
		return
	}
	ch := make(chan struct{})
	select {
	case s.ev <- statsEvent{kind: kindSync, syncCh: ch}:
		<-ch
	default:
		// consumer backed up; best-effort, no synchronous guarantee
	}
}

// QueueLen reports unflushed events waiting in the channel. StatsHandler
// surfaces it so load on the stats pipeline is observable.
func (s *Store) QueueLen() int { return len(s.ev) }

// SwapDropped returns and resets the count of events dropped since the last
// call. StatsHandler surfaces it as stats_dropped; reset-on-read means each
// admin poll reports "dropped since the last poll" rather than a growing total.
func (s *Store) SwapDropped() int64 { return s.dropped.Swap(0) }

func nowBucket() int64 { return time.Now().Unix() / 60 }

// windowSpec returns (window length in minutes, bucket divisor for downsampling).
func windowSpec(w Window) (int64, int64) {
	switch w {
	case Window1h:
		return 60, 1
	case Window7d:
		return 7 * 24 * 60, 60
	default: // Window24h
		return 24 * 60, 5
	}
}

func outcomeCol(o Outcome) string {
	switch o {
	case OK:
		return "ok"
	case ClientError, ServerError:
		return "err"
	case Throttled:
		return "r429"
	}
	return ""
}

// ---- Hot-path writers: non-blocking event submission ----

// RecordAccount logs the final outcome of a client request to the account that
// ultimately handled it. Contributes to both req and the outcome column, so it
// is the success-rate denominator — call once per request, on the final account.
func (s *Store) RecordAccount(id int, o Outcome) {
	s.send(statsEvent{kind: kindAccount, id: id, bucket: nowBucket(), col: outcomeCol(o)})
}

// RecordGlobal logs one finalized client request outcome (called once per request).
func (s *Store) RecordGlobal(o Outcome) {
	s.send(statsEvent{kind: kindGlobal, bucket: nowBucket(), col: outcomeCol(o)})
}

// RecordThrottle bumps the per-account r429 counter without touching req, so a
// 429 retry mid-request doesn't pollute the success-rate denominator.
func (s *Store) RecordThrottle(id int) {
	s.send(statsEvent{kind: kindAccount, id: id, bucket: nowBucket(), col: "r429", throttleOnly: true})
}

// RecordGlobalThrottle bumps the global r429 counter (a request hit ≥1 429)
// without touching the global req denominator.
func (s *Store) RecordGlobalThrottle() {
	s.send(statsEvent{kind: kindGlobal, bucket: nowBucket(), col: "r429", throttleOnly: true})
}

// RecordNetworkRetry bumps the per-account net_retry counter without touching
// req. A transient upstream transport failure (network blip / deadline) that
// the proxy retried in-place on the SAME account is recorded here so the rate
// of network flakiness is observable on the dashboard without lowering the
// success-rate denominator — the request ultimately either succeeded on this
// account (after the retry) or was finalized on another account.
func (s *Store) RecordNetworkRetry(id int) {
	s.send(statsEvent{kind: kindAccount, id: id, bucket: nowBucket(), col: "net_retry", throttleOnly: true})
}

// RecordGlobalNetworkRetry bumps the global net_retry counter (a request hit
// ≥1 transport failure that was retried) without touching the global req
// denominator. Pair with RecordNetworkRetry at the per-account level.
func (s *Store) RecordGlobalNetworkRetry() {
	s.send(statsEvent{kind: kindGlobal, bucket: nowBucket(), col: "net_retry", throttleOnly: true})
}

// RecordBalance stores a balance snapshot (called by the monitor).
func (s *Store) RecordBalance(id int, balance float64, coupons int) {
	s.send(statsEvent{kind: kindBalance, id: id, balance: balance, coupons: coupons})
}

func (s *Store) send(ev statsEvent) {
	if s.closed.Load() {
		return
	}
	select {
	case s.ev <- ev:
	default:
		// Consumer can't keep up: drop (never block the proxy hot path) but
		// count it so StatsHandler can surface how many were lost.
		s.dropped.Add(1)
	}
}

// ---- consumer: merge + batch flush (single DB writer) ----

type count4 struct{ req, ok, err, r429, netRetry int }

type accountKey struct{ id, bucket int64 }
type globalKey struct{ bucket int64 }
type balanceEv struct {
	id      int64
	ts      int64
	balance float64
	coupons int
}

func (s *Store) loop() {
	defer s.wg.Done()
	ticker := time.NewTicker(flushPeriod)
	defer ticker.Stop()
	acc := map[accountKey]*count4{}
	glb := map[globalKey]*count4{}
	var balances []balanceEv
	retentionPending := false

	flush := func() {
		s.flushMerged(acc, glb, balances)
		for k := range acc {
			delete(acc, k)
		}
		for k := range glb {
			delete(glb, k)
		}
		balances = balances[:0]
	}

	apply := func(c *count4, ev statsEvent) {
		if ev.throttleOnly {
			if ev.col == "net_retry" {
				c.netRetry++
			} else {
				c.r429++
			}
			return
		}
		c.req++
		switch ev.col {
		case "ok":
			c.ok++
		case "err":
			c.err++
		case "r429":
			c.r429++
		case "net_retry":
			c.netRetry++
		}
	}

	for {
		select {
		case ev, ok := <-s.ev:
			if !ok { // Close: flush whatever remains and exit
				flush()
				return
			}
			switch ev.kind {
			case kindAccount:
				k := accountKey{int64(ev.id), ev.bucket}
				c := acc[k]
				if c == nil {
					c = &count4{}
					acc[k] = c
				}
				apply(c, ev)
			case kindGlobal:
				k := globalKey{ev.bucket}
				c := glb[k]
				if c == nil {
					c = &count4{}
					glb[k] = c
				}
				apply(c, ev)
			case kindBalance:
				balances = append(balances, balanceEv{int64(ev.id), time.Now().Unix(), ev.balance, ev.coupons})
			case kindSync:
				flush()
				close(ev.syncCh)
			case kindRetention:
				retentionPending = true
			}
			if retentionPending {
				flush()
				s.retention()
				retentionPending = false
				continue
			}
			if len(acc)+len(glb)+len(balances) >= flushBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// flushMerged writes all accumulated counters + balance snapshots in one
// transaction. Each (key,bucket) is a single upsert row whose four counters
// are the merged increments, replacing the old INSERT+UPDATE two-statement
// pattern per event.
func (s *Store) flushMerged(acc map[accountKey]*count4, glb map[globalKey]*count4, balances []balanceEv) {
	if len(acc)+len(glb)+len(balances) == 0 {
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("stats: begin tx: %v", err)
		return
	}
	defer func() { _ = tx.Rollback() }() //nolint

	if err := execBatch(tx, `INSERT INTO account_minute(id,bucket,req,ok,err,r429,net_retry) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(id,bucket) DO UPDATE SET req=req+excluded.req, ok=ok+excluded.ok, err=err+excluded.err, r429=r429+excluded.r429, net_retry=net_retry+excluded.net_retry`,
		func(yield func(args ...any)) {
			for k, c := range acc {
				yield(k.id, k.bucket, c.req, c.ok, c.err, c.r429, c.netRetry)
			}
		}); err != nil {
		log.Printf("stats: flush account: %v", err)
		return
	}

	if err := execBatch(tx, `INSERT INTO global_minute(bucket,req,ok,err,r429,net_retry) VALUES(?,?,?,?,?,?)
		ON CONFLICT(bucket) DO UPDATE SET req=req+excluded.req, ok=ok+excluded.ok, err=err+excluded.err, r429=r429+excluded.r429, net_retry=net_retry+excluded.net_retry`,
		func(yield func(args ...any)) {
			for k, c := range glb {
				yield(k.bucket, c.req, c.ok, c.err, c.r429, c.netRetry)
			}
		}); err != nil {
		log.Printf("stats: flush global: %v", err)
		return
	}

	if err := execBatch(tx, `INSERT OR REPLACE INTO balance_snap(id,ts,balance,coupons) VALUES(?,?,?,?)`,
		func(yield func(args ...any)) {
			for _, b := range balances {
				yield(b.id, b.ts, b.balance, b.coupons)
			}
		}); err != nil {
		log.Printf("stats: flush balance: %v", err)
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("stats: commit: %v", err)
	}
}

// execBatch prepares stmt once and runs it for every row produced by rows,
// returning on the first error (the surrounding transaction is rolled back).
func execBatch(tx *sql.Tx, query string, rows func(yield func(args ...any))) error {
	stmt, err := tx.Prepare(query)
	if err != nil {
		return err
	}
	defer stmt.Close()
	var batchErr error
	rows(func(args ...any) {
		if batchErr != nil {
			return
		}
		if _, err := stmt.Exec(args...); err != nil {
			batchErr = err
		}
	})
	return batchErr
}

func (s *Store) retention() {
	cutBucket := nowBucket() - retentionDays*24*60
	cutTs := time.Now().Unix() - retentionDays*24*60*60
	_, _ = s.db.Exec(`DELETE FROM account_minute WHERE bucket < ?`, cutBucket)
	_, _ = s.db.Exec(`DELETE FROM global_minute WHERE bucket < ?`, cutBucket)
	_, _ = s.db.Exec(`DELETE FROM balance_snap WHERE ts < ?`, cutTs)
}

func (s *Store) retentionLoop() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for range t.C {
		s.send(statsEvent{kind: kindRetention})
	}
}

// ---- read paths (no lock; WAL allows concurrent reads with the writer) ----

// TimelinePoint is one (downsampled) minute bucket of global traffic.
// Agg is embedded so its counters serialize at the top level.
type TimelinePoint struct {
	Ts int64 `json:"ts"`
	Agg
}

// BalancePoint is one balance snapshot sample.
type BalancePoint struct {
	Ts      int64   `json:"ts"`
	Balance float64 `json:"balance"`
	Coupons int     `json:"coupons"`
}

// AccountAgg is the window aggregate for one account, keyed by stable ID.
type AccountAgg struct {
	ID int `json:"id"`
	Agg
}

// Snapshot bundles all DB-derived aggregates for a window. Live per-account
// fields (balance/enabled/...) are merged by the HTTP handler from the pool.
type Snapshot struct {
	Window   string                 `json:"window"`
	Global   Agg                    `json:"global"`
	Timeline []TimelinePoint        `json:"timeline"`
	Accounts []AccountAgg           `json:"accounts"`
	Balances map[int][]BalancePoint `json:"balances"`
}

func (s *Store) GlobalAggregate(w Window) (Agg, error) {
	mins, _ := windowSpec(w)
	cut := nowBucket() - mins
	row := s.db.QueryRow(
		`SELECT COALESCE(SUM(req),0),COALESCE(SUM(ok),0),COALESCE(SUM(err),0),COALESCE(SUM(r429),0),COALESCE(SUM(net_retry),0)
		 FROM global_minute WHERE bucket >= ?`, cut)
	var a Agg
	return a, row.Scan(&a.Req, &a.Ok, &a.Err, &a.R429, &a.NetRetry)
}

func (s *Store) AccountAggregate(id int, w Window) (Agg, error) {
	mins, _ := windowSpec(w)
	cut := nowBucket() - mins
	row := s.db.QueryRow(
		`SELECT COALESCE(SUM(req),0),COALESCE(SUM(ok),0),COALESCE(SUM(err),0),COALESCE(SUM(r429),0),COALESCE(SUM(net_retry),0)
		 FROM account_minute WHERE id=? AND bucket >= ?`, id, cut)
	var a Agg
	return a, row.Scan(&a.Req, &a.Ok, &a.Err, &a.R429, &a.NetRetry)
}

func (s *Store) Timeline(w Window) ([]TimelinePoint, error) {
	mins, div := windowSpec(w)
	cut := nowBucket() - mins
	rows, err := s.db.Query(
		`SELECT (bucket/?)*?, COALESCE(SUM(req),0),COALESCE(SUM(ok),0),COALESCE(SUM(err),0),COALESCE(SUM(r429),0),COALESCE(SUM(net_retry),0)
		 FROM global_minute WHERE bucket >= ? GROUP BY (bucket/?)*? ORDER BY 1`,
		div, div, cut, div, div)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TimelinePoint
	for rows.Next() {
		var b int64
		var p TimelinePoint
		if err := rows.Scan(&b, &p.Req, &p.Ok, &p.Err, &p.R429, &p.NetRetry); err != nil {
			return nil, err
		}
		p.Ts = b * 60
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) BalanceHistory(id int, w Window) ([]BalancePoint, error) {
	mins, _ := windowSpec(w)
	cut := time.Now().Unix() - mins*60
	rows, err := s.db.Query(
		`SELECT ts,balance,coupons FROM balance_snap WHERE id=? AND ts >= ? ORDER BY ts`,
		id, cut)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BalancePoint
	for rows.Next() {
		var p BalancePoint
		if err := rows.Scan(&p.Ts, &p.Balance, &p.Coupons); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// balanceHistoryAll returns balance snapshots for every account in the window
// in a single query (Snapshot used to do one query per account). Map keys are
// account IDs; accounts with no snapshots are simply absent from the map.
func (s *Store) balanceHistoryAll(w Window) (map[int][]BalancePoint, error) {
	mins, _ := windowSpec(w)
	cut := time.Now().Unix() - mins*60
	rows, err := s.db.Query(
		`SELECT id, ts, balance, coupons FROM balance_snap WHERE ts >= ? ORDER BY id, ts`,
		cut)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int][]BalancePoint)
	for rows.Next() {
		var id int
		var p BalancePoint
		if err := rows.Scan(&id, &p.Ts, &p.Balance, &p.Coupons); err != nil {
			return nil, err
		}
		out[id] = append(out[id], p)
	}
	return out, rows.Err()
}

// Snapshot composes all DB-derived aggregates for a window in a handful of
// queries (the per-account loop is a single GROUP BY — was N queries).
func (s *Store) Snapshot(w Window) (*Snapshot, error) {
	g, err := s.GlobalAggregate(w)
	if err != nil {
		return nil, err
	}
	tl, err := s.Timeline(w)
	if err != nil {
		return nil, err
	}
	mins, _ := windowSpec(w)
	cut := nowBucket() - mins
	rows, err := s.db.Query(`SELECT id, COALESCE(SUM(req),0),COALESCE(SUM(ok),0),COALESCE(SUM(err),0),COALESCE(SUM(r429),0),COALESCE(SUM(net_retry),0)
		FROM account_minute WHERE bucket >= ? GROUP BY id`, cut)
	if err != nil {
		return nil, err
	}
	var accounts []AccountAgg
	for rows.Next() {
		var a AccountAgg
		if err := rows.Scan(&a.ID, &a.Req, &a.Ok, &a.Err, &a.R429, &a.NetRetry); err != nil {
			rows.Close()
			return nil, err
		}
		accounts = append(accounts, a)
	}
	rows.Close()

	// All balance snapshots in one query (was N queries, one per account).
	balances, err := s.balanceHistoryAll(w)
	if err != nil {
		return nil, err
	}
	return &Snapshot{
		Window:   string(w),
		Global:   g,
		Timeline: tl,
		Accounts: accounts,
		Balances: balances,
	}, nil
}
