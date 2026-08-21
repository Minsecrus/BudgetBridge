package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"budgetbridge/internal/reqlog"

	"github.com/gin-gonic/gin"
)

// EventsHandler streams request-log Events to the dashboard over SSE. On
// connect it replays the current ring buffer (recent history) then pushes each
// new Event live, with a 15s comment-line heartbeat to keep idle connections
// open through vite/Caddy. The client disconnect (ctx.Done) unregisters the
// subscriber so the broker stops fanning out to a dead channel.
func EventsHandler(lg *reqlog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !lg.Enabled() {
			c.JSON(503, gin.H{"error": "request logging is disabled"})
			return
		}
		replay, ch, cancel := lg.Subscribe()
		defer cancel()

		// Optional ?limit=N caps the replay to the newest N entries. The ring
		// can hold up to reqlog_capacity (default 1000) but on initial page
		// load the client usually only needs the most recent slice; older
		// entries are paginated via /admin/logs ("加载更早"). 0/missing = full
		// ring, preserving the previous behavior for any other consumer.
		if v := c.Query("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n < len(replay) {
				replay = replay[len(replay)-n:]
			}
		}

		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		c.Writer.WriteHeader(200)
		fl, _ := c.Writer.(http.Flusher)

		emit := func(data []byte) {
			c.Writer.WriteString("data: ")
			c.Writer.Write(data)
			c.Writer.WriteString("\n\n")
			if fl != nil {
				fl.Flush()
			}
		}

		// Replay recent history first, oldest→newest, so the client catches up
		// before live events arrive.
		for _, e := range replay {
			if data, err := json.Marshal(e); err == nil {
				emit(data)
			}
		}

		heartbeat := time.NewTicker(15 * time.Second)
		defer heartbeat.Stop()
		ctx := c.Request.Context()
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				if data, err := json.Marshal(ev); err == nil {
					emit(data)
				}
			case <-heartbeat.C:
				// SSE comment — not delivered to EventSource listeners, but it
				// generates traffic so buffering proxies don't time the idle
				// connection out.
				c.Writer.WriteString(": ping\n\n")
				if fl != nil {
					fl.Flush()
				}
			case <-ctx.Done():
				return
			}
		}
	}
}

// LogsHandler serves historical request logs from the SQLite store, newest
// first, with a `before` cursor for "load earlier" pagination and optional
// account/outcome/keyword filters. Backs the /admin/logs endpoint.
//
// `outcome` accepts a single value (ok/server_error/client_error/no_accounts/
// throttled) or the category `error`, which maps to the three failure outcomes
// (no_accounts/server_error/client_error) so the dashboard can fetch the full
// — small — error set in one request via a high `limit` (up to 2000) instead of
// paging through all outcomes.
func LogsHandler(lg *reqlog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !lg.Enabled() {
			c.JSON(503, gin.H{"error": "request logging is disabled"})
			return
		}
		var before int64
		if v := c.Query("before"); v != "" {
			before, _ = strconv.ParseInt(v, 10, 64)
		}
		limit := 100
		if v := c.Query("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
		}
		accountID, _ := strconv.Atoi(c.Query("account"))
		logs, err := lg.Recent(before, limit, accountID, c.Query("outcome"), c.Query("q"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		if logs == nil {
			logs = []reqlog.Event{}
		}
		c.JSON(200, gin.H{"logs": logs})
	}
}
