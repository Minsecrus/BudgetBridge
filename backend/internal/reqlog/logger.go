// Package reqlog records per-request scheduling traces for the admin dashboard.
//
// Every proxied request produces one Event capturing the full retry trajectory
// (which account each attempt hit, its upstream status, duration), the outcome,
// timing (total + TTFT for streams), and token/cache usage. Events flow two
// paths from a single Log() entry point:
//
//   - Realtime: an in-memory ring buffer + subscriber fan-out, consumed by the
//     SSE endpoint GET /admin/events for live monitoring (sub-second, zero DB
//     reads on the hot path).
//   - Retention: a single-writer SQLite store (data/reqlog.db) batch-inserts
//     Events for one-week historical lookup via GET /admin/logs.
//
// The hot path calls only Log(), which is a non-blocking channel send
// (select-default drop + atomic counter when full) — it never holds the ring
// lock and never touches the DB. One consumer goroutine drains the channel,
// pushes to the ring + subscribers, and batches DB inserts. This mirrors the
// proven stats store pattern (internal/stats/store.go).
package reqlog

import (
	"database/sql"
	"sync"
	"sync/atomic"
	"time"
)

const (
	evCap           = 4096        // event channel buffer; drop+count when full
	subCap          = 256         // per-subscriber delivery buffer; slow clients drop
	flushBatch      = 256         // flush DB once this many events are queued
	flushPeriod     = time.Second // flush DB at least this often
	retentionPeriod = 10 * time.Minute
)

// Attempt is one per-account try within a request's retry loop.
type Attempt struct {
	AccountID int    `json:"account_id"`
	Alias     string `json:"alias"`
	Status    int    `json:"status"`  // upstream HTTP status; 0 = transport error
	Outcome   string `json:"outcome"` // ok/429/4xx_retry/5xx_pass/4xx_pass/net_err
	DurMs     int64  `json:"dur_ms"`
	Err       string `json:"err,omitempty"`
}

// Event is the complete scheduling trace of one proxied request. It is the SSE
// payload, the DB row, and the frontend log row.
type Event struct {
	ID               string    `json:"id"`
	Ts               int64     `json:"ts"`    // request start, ms
	Proto            string    `json:"proto"` // openai | anthropic
	Model            string    `json:"model"`
	Stream           bool      `json:"stream"`
	Bytes            int       `json:"bytes"`    // request body size
	KeyHash          string    `json:"key_hash"` // affinity key, fnv-truncated (never raw)
	Warm             bool      `json:"warm"`
	Cold             bool      `json:"cold"`
	Attempts         []Attempt `json:"attempts"`
	Retries          int       `json:"retries"` // len(Attempts)-1
	FinalAccountID   int       `json:"final_account_id"`
	FinalAlias       string    `json:"final_alias"`
	Status           int       `json:"final_status"`      // HTTP status returned to client
	Outcome          string    `json:"outcome"`           // ok/client_error/server_error/no_accounts
	DurMs            int64     `json:"dur_ms"`            // total request duration
	TTFTMs           *int64    `json:"ttft_ms,omitempty"` // stream first-byte; nil for non-stream
	PromptTokens     int       `json:"prompt_tokens,omitempty"`
	CompletionTokens int       `json:"completion_tokens,omitempty"`
	TotalTokens      int       `json:"total_tokens,omitempty"`
	CachedTokens     int       `json:"cached_tokens,omitempty"`
}

// item is one entry on the event channel: either a log Event or a sync
// barrier. Sharing one (FIFO) channel guarantees a barrier lands after all
// previously-logged Events, so Sync flushes them — a separate sync channel
// would race and flush an empty batch.
type item struct {
	ev     Event
	syncCh chan struct{} // non-nil → this is a sync barrier
}

// Logger is the request-log sink. Construct once via New and share across
// handlers. Safe for concurrent use.
type Logger struct {
	enabled       bool
	db            *sql.DB   // nil when disabled
	ev            chan item // nil when disabled
	ring          []Event   // fixed-cap ring buffer
	ringCap       int
	head          int        // next write slot
	count         int        // populated count (≤ ringCap)
	ringMu        sync.Mutex // guards ring/head/count
	subs          map[uint64]chan Event
	nextSubID     uint64
	subMu         sync.Mutex   // guards subs/nextSubID
	dropped       atomic.Int64 // events dropped when ev was full
	closed        atomic.Bool
	wg            sync.WaitGroup
	retentionDays int
}

// New opens (or returns a no-op) Logger. When enabled is false the returned
// Logger is a zero-cost stub: Log/Subscribe/Recent are no-ops and no DB is
// opened, so the proxy can unconditionally call lg.Log(...) / lg.Enabled().
func New(dbPath string, ringCap, retentionDays int, enabled bool) (*Logger, error) {
	if !enabled {
		return &Logger{enabled: false}, nil
	}
	if ringCap <= 0 {
		ringCap = 1000
	}
	if retentionDays <= 0 {
		retentionDays = 7
	}
	db, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}
	l := &Logger{
		enabled:       true,
		db:            db,
		ev:            make(chan item, evCap),
		ring:          make([]Event, ringCap),
		ringCap:       ringCap,
		subs:          map[uint64]chan Event{},
		retentionDays: retentionDays,
	}
	// Initial retention sweep before the consumer starts, so no DB write
	// concurrency on the first delete.
	l.retention()
	l.wg.Add(1)
	go l.loop()
	return l, nil
}

// Enabled reports whether logging is on. Handlers gate Event construction and
// stream_options.include_usage injection on this so a disabled logger leaves
// the request body and hot path untouched.
func (l *Logger) Enabled() bool { return l.enabled }

// Log submits one Event. Non-blocking: if the channel is full (consumer can't
// keep up) the event is dropped and counted rather than stalling the proxy.
func (l *Logger) Log(e Event) {
	if !l.enabled || l.closed.Load() {
		return
	}
	// A zero-attempt event (e.g. "no available accounts": the pool was empty so
	// the retry loop never ran) arrives with a nil Attempts. Go marshals a nil
	// slice to JSON null, but the dashboard row maps over l.attempts unguarded
	// — null would white-screen the app. Normalize once at the single entry
	// point so the ring (SSE replay + live fan-out) never holds nil. The DB
	// history path is normalized again in Recent (nil round-trips through the
	// empty TEXT column).
	if e.Attempts == nil {
		e.Attempts = []Attempt{}
	}
	select {
	case l.ev <- item{ev: e}:
	default:
		l.dropped.Add(1)
	}
}

// Subscribe returns a snapshot of the current ring buffer (for replay) plus a
// channel of subsequent Events. The caller must call cancel to unregister when
// the SSE connection ends. Used by the /admin/events SSE handler.
func (l *Logger) Subscribe() (replay []Event, ch <-chan Event, cancel func()) {
	if !l.enabled {
		return nil, nil, func() {}
	}
	l.ringMu.Lock()
	replay = l.snapshot()
	l.ringMu.Unlock()
	c := make(chan Event, subCap)
	l.subMu.Lock()
	id := l.nextSubID
	l.nextSubID++
	l.subs[id] = c
	l.subMu.Unlock()
	return replay, c, func() {
		l.subMu.Lock()
		delete(l.subs, id)
		l.subMu.Unlock()
	}
}

// QueueLen reports unprocessed events in the channel. Surfaced for observability.
func (l *Logger) QueueLen() int {
	if !l.enabled {
		return 0
	}
	return len(l.ev)
}

// SwapDropped returns and resets the count of events dropped since the last
// call. Reset-on-read so each poll reports "dropped since the last poll".
func (l *Logger) SwapDropped() int64 { return l.dropped.Swap(0) }

// Close drains the channel, flushes remaining events to the DB, and closes the
// database. Idempotent. Call in the shutdown sequence before stats.
func (l *Logger) Close() error {
	if !l.enabled {
		return nil
	}
	if l.closed.Swap(true) {
		return nil
	}
	close(l.ev)
	l.wg.Wait()
	return l.db.Close()
}

// Sync blocks until the consumer has flushed all queued events to the DB.
// Tests/graceful-shutdown only — never call on the hot path. Best-effort: if
// the channel is full the barrier is dropped rather than blocking.
func (l *Logger) Sync() {
	if !l.enabled || l.closed.Load() {
		return
	}
	s := make(chan struct{})
	select {
	case l.ev <- item{syncCh: s}:
		<-s
	default:
	}
}

// loop is the single consumer: push to ring + fan-out to subscribers, and
// batch-insert into the DB. One goroutine owns all DB writes (inserts +
// retention) so there is no WAL write contention.
func (l *Logger) loop() {
	defer l.wg.Done()
	flushT := time.NewTicker(flushPeriod)
	defer flushT.Stop()
	retT := time.NewTicker(retentionPeriod)
	defer retT.Stop()
	var batch []Event
	flush := func() {
		if len(batch) == 0 {
			return
		}
		l.insertBatch(batch)
		batch = batch[:0]
	}
	for {
		select {
		case it, ok := <-l.ev:
			if !ok { // Close: flush whatever remains and exit
				flush()
				return
			}
			if it.syncCh != nil { // Sync barrier: flush, then signal.
				flush()
				close(it.syncCh)
				continue
			}
			e := it.ev
			l.pushRing(e)
			l.fanout(e)
			batch = append(batch, e)
			if len(batch) >= flushBatch {
				flush()
			}
		case <-flushT.C:
			flush()
		case <-retT.C:
			flush()
			l.retention()
		}
	}
}

// pushRing appends e to the ring buffer, evicting the oldest entry when full.
func (l *Logger) pushRing(e Event) {
	l.ringMu.Lock()
	l.ring[l.head] = e
	l.head = (l.head + 1) % l.ringCap
	if l.count < l.ringCap {
		l.count++
	}
	l.ringMu.Unlock()
}

// snapshot returns a copy of the ring contents oldest→newest. Caller holds ringMu.
func (l *Logger) snapshot() []Event {
	if l.count == 0 {
		return nil
	}
	out := make([]Event, l.count)
	start := (l.head - l.count + l.ringCap) % l.ringCap
	for i := 0; i < l.count; i++ {
		out[i] = l.ring[(start+i)%l.ringCap]
	}
	return out
}

// fanout delivers e to every subscriber non-blocking; a full subscriber channel
// means that client is behind, so drop (it still has the ring for a future
// reconnect replay — losing live tail is preferable to back-pressuring the
// proxy or stalling other subscribers).
func (l *Logger) fanout(e Event) {
	l.subMu.Lock()
	for _, ch := range l.subs {
		select {
		case ch <- e:
		default:
		}
	}
	l.subMu.Unlock()
}
