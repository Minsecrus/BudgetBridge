package reqlog

import (
	"database/sql"
	"encoding/json"
	"log"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

const schema = `
CREATE TABLE IF NOT EXISTS request_log (
  rowid INTEGER PRIMARY KEY AUTOINCREMENT,
  ts INTEGER, req_id TEXT, proto TEXT, model TEXT, stream INTEGER,
  bytes INTEGER, key_hash TEXT, warm INTEGER, cold INTEGER,
  final_account_id INTEGER, final_alias TEXT, status INTEGER, outcome TEXT,
  dur_ms INTEGER, ttft_ms INTEGER,
  prompt_tokens INTEGER, completion_tokens INTEGER, total_tokens INTEGER, cached_tokens INTEGER,
  retries INTEGER, attempts TEXT
);
CREATE INDEX IF NOT EXISTS idx_rl_ts      ON request_log(ts);
CREATE INDEX IF NOT EXISTS idx_rl_acc     ON request_log(final_account_id, ts);
CREATE INDEX IF NOT EXISTS idx_rl_outcome ON request_log(outcome, ts);
`

// openDB opens the SQLite database at path with WAL + a busy timeout (matching
// the stats store). WAL allows the SSE/Recent readers to run concurrently with
// the single-writer consumer.
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// insertBatch writes a batch of Events in one transaction. Called only from the
// consumer goroutine (single DB writer).
func (l *Logger) insertBatch(batch []Event) {
	if len(batch) == 0 {
		return
	}
	tx, err := l.db.Begin()
	if err != nil {
		log.Printf("reqlog: begin tx: %v", err)
		return
	}
	defer func() { _ = tx.Rollback() }() //nolint
	stmt, err := tx.Prepare(`INSERT INTO request_log(ts,req_id,proto,model,stream,bytes,key_hash,warm,cold,
final_account_id,final_alias,status,outcome,dur_ms,ttft_ms,
prompt_tokens,completion_tokens,total_tokens,cached_tokens,retries,attempts)
VALUES(` + strings.Repeat("?,", 20) + `?)`)
	if err != nil {
		log.Printf("reqlog: prepare: %v", err)
		return
	}
	defer stmt.Close()
	for _, e := range batch {
		attempts := ""
		if len(e.Attempts) > 0 {
			if b, err := json.Marshal(e.Attempts); err == nil {
				attempts = string(b)
			}
		}
		var ttft any
		if e.TTFTMs != nil {
			ttft = *e.TTFTMs
		}
		if _, err := stmt.Exec(
			e.Ts, e.ID, e.Proto, e.Model, b2i(e.Stream), e.Bytes, e.KeyHash, b2i(e.Warm), b2i(e.Cold),
			e.FinalAccountID, e.FinalAlias, e.Status, e.Outcome, e.DurMs, ttft,
			e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.CachedTokens, e.Retries, attempts,
		); err != nil {
			log.Printf("reqlog: insert: %v", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("reqlog: commit: %v", err)
	}
}

// Recent returns up to limit Events older than `before` (ms, descending), with
// optional account/outcome/keyword filters. `before`<=0 means now. Used by the
// /admin/logs history endpoint for "load earlier" pagination.
func (l *Logger) Recent(before int64, limit int, accountID int, outcome, q string) ([]Event, error) {
	// maxRecentLimit bounds a single query. The default "load earlier" page is
	// 100; the dashboard's "全部错误" fetch raises limit to this ceiling so the
	// (rare) error set ships in one response instead of paging. 2000 matches
	// the live in-memory cap, keeping the rendered DOM bounded.
	const maxRecentLimit = 2000
	if limit <= 0 || limit > maxRecentLimit {
		limit = 100
	}
	if before <= 0 {
		before = time.Now().UnixMilli()
	}
	var b strings.Builder
	args := []any{before}
	b.WriteString(`SELECT ts,req_id,proto,model,stream,bytes,key_hash,warm,cold,
final_account_id,final_alias,status,outcome,dur_ms,ttft_ms,
prompt_tokens,completion_tokens,total_tokens,cached_tokens,retries,attempts
FROM request_log WHERE ts < ?`)
	if accountID > 0 {
		b.WriteString(` AND final_account_id = ?`)
		args = append(args, accountID)
	}
	if outcome != "" {
		if outcome == "error" {
			// Frontend "错误" spans the three failure outcomes; a single
			// outcome=? can't express the OR, so map the category to an IN.
			// idx_rl_outcome(outcome, ts) still serves this range scan.
			b.WriteString(` AND outcome IN ('no_accounts','server_error','client_error')`)
		} else {
			b.WriteString(` AND outcome = ?`)
			args = append(args, outcome)
		}
	}
	if q != "" {
		b.WriteString(` AND (model LIKE ? OR final_alias LIKE ? OR req_id LIKE ?)`)
		pat := "%" + q + "%"
		args = append(args, pat, pat, pat)
	}
	b.WriteString(` ORDER BY ts DESC LIMIT ?`)
	args = append(args, limit)
	rows, err := l.db.Query(b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var stream, warm, cold int
		var ttft sql.NullInt64
		var attempts string
		if err := rows.Scan(
			&e.Ts, &e.ID, &e.Proto, &e.Model, &stream, &e.Bytes, &e.KeyHash, &warm, &cold,
			&e.FinalAccountID, &e.FinalAlias, &e.Status, &e.Outcome, &e.DurMs, &ttft,
			&e.PromptTokens, &e.CompletionTokens, &e.TotalTokens, &e.CachedTokens, &e.Retries, &attempts,
		); err != nil {
			return nil, err
		}
		e.Stream = stream == 1
		e.Warm = warm == 1
		e.Cold = cold == 1
		if ttft.Valid {
			v := ttft.Int64
			e.TTFTMs = &v
		}
		if attempts != "" {
			_ = json.Unmarshal([]byte(attempts), &e.Attempts)
		}
		// Zero-attempt rows are stored as attempts='' (insertBatch writes the
		// column empty when len==0), which leaves e.Attempts nil. Normalize so
		// /admin/logs never serves null — the frontend maps attempts unguarded.
		if e.Attempts == nil {
			e.Attempts = []Attempt{}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// retention deletes Events older than retentionDays. Called from the consumer
// goroutine (single writer) on a 10-minute ticker.
func (l *Logger) retention() {
	cut := time.Now().UnixMilli() - int64(l.retentionDays)*24*60*60*1000
	if _, err := l.db.Exec(`DELETE FROM request_log WHERE ts < ?`, cut); err != nil {
		log.Printf("reqlog: retention: %v", err)
	}
}
