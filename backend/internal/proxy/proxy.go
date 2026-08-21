package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"budgetbridge/internal/auth"
	"budgetbridge/internal/fallback"
	"budgetbridge/internal/monitor"
	"budgetbridge/internal/pool"
	"budgetbridge/internal/reqlog"
	"budgetbridge/internal/stats"

	"github.com/gin-gonic/gin"
)

// httpClient is the shared upstream client, built once from config in
// ConfigureUpstream (called by main). See transport.go for the HTTP/2 PING
// keepalive and optional SOCKS5 proxy rationale.

func LoginHandler(passwordHash string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if passwordHash == "" {
			c.JSON(400, gin.H{"error": "auth not configured"})
			return
		}
		ip := c.ClientIP()
		if !auth.Allow(ip) {
			c.JSON(429, gin.H{"error": "too many attempts, try again later"})
			return
		}
		var req struct {
			Password string `json:"password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || req.Password == "" {
			c.JSON(400, gin.H{"error": "password required"})
			return
		}
		if !auth.CheckPassword(passwordHash, req.Password) {
			auth.RecordFail(ip)
			c.JSON(401, gin.H{"error": "invalid password"})
			return
		}
		auth.Reset(ip)
		c.JSON(200, gin.H{"token": auth.NewToken()})
	}
}

func Handler(p *pool.Pool, upstream, modelOverride string, st *stats.Store, lg *reqlog.Logger, fbs *fallback.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(400, gin.H{"error": "read body"})
			return
		}

		// Parse the body once into a map[string]json.RawMessage. This is the
		// single full-body unmarshal on the OpenAI path: the model/stream
		// fields, the model/stream_options rewrite, and the affinity key are
		// all derived from it (small-field / messages-only unmarshals) instead
		// of re-tokenizing the whole body two or three more times. The Anthropic
		// translator was already fixed this way (see anthropic.go:92); the OpenAI
		// path now matches it. Arbitrary client fields (temperature, top_p,
		// vendor extensions) are preserved because we re-marshal from the same
		// map, not from a typed struct that would drop unknowns.
		var m map[string]json.RawMessage
		if json.Unmarshal(body, &m) != nil {
			m = nil // leave body byte-for-byte; upstream will surface the 400
		}
		var payload struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if raw, ok := m["model"]; ok {
			json.Unmarshal(raw, &payload.Model) //nolint
		}
		if raw, ok := m["stream"]; ok {
			json.Unmarshal(raw, &payload.Stream) //nolint
		}

		// Rewrite the body once for the model override and, for streams, the
		// include_usage stream_option so dashscope emits a final chunk with
		// token/cache accounting we can record. Only when logging is enabled —
		// a disabled logger leaves the body byte-for-byte untouched. Existing
		// client stream_options are preserved (only include_usage is forced on).
		injectUsage := payload.Stream && lg.Enabled()
		if (modelOverride != "" || injectUsage) && m != nil {
			if modelOverride != "" {
				m["model"], _ = json.Marshal(modelOverride)
			}
			if injectUsage {
				so := map[string]any{}
				if raw, ok := m["stream_options"]; ok {
					json.Unmarshal(raw, &so) //nolint
				}
				so["include_usage"] = true
				m["stream_options"], _ = json.Marshal(so)
			}
			body, _ = json.Marshal(m)
		}

		affinityKey, warm := parseAffinity(m["messages"])
		cold := !warm && len(body) < p.ColdRequestBytes()
		finalOutcome := stats.ServerError
		var finalAcc *pool.Account
		var finalFb string // fallback channel name when a fallback (not a pool account) served the request
		hitThrottle := false
		hitNetRetry := false
		throttle := func(acc *pool.Account) {
			if st != nil {
				st.RecordThrottle(acc.ID)
			}
			hitThrottle = true
		}
		networkRetry := func(acc *pool.Account) {
			if st != nil {
				st.RecordNetworkRetry(acc.ID)
			}
			hitNetRetry = true
		}
		if st != nil {
			defer func() {
				st.RecordGlobal(finalOutcome)
				if hitThrottle {
					st.RecordGlobalThrottle()
				}
				if hitNetRetry {
					st.RecordGlobalNetworkRetry()
				}
				if finalAcc != nil {
					st.RecordAccount(finalAcc.ID, finalOutcome)
				}
			}()
		}

		// Per-request scheduling trace for the admin log. Populated as the
		// retry loop runs and submitted once at the end (after the response is
		// fully written, so DurMs covers streaming too). Log is a no-op when
		// the logger is disabled; the collectors stay cheap regardless.
		var (
			attempts   []reqlog.Attempt
			ttft       *int64
			usage      *tokenUsage
			noAccounts bool
		)
		effModel := modelOverride
		if effModel == "" {
			effModel = payload.Model
		}
		// Record token usage against the serving account's per-model free
		// quota (local accounting for cold routing). Runs regardless of reqlog
		// — free-quota bookkeeping must not depend on the log being enabled.
		defer func() {
			if usage != nil && finalAcc != nil && usage.TotalTokens > 0 {
				finalAcc.ConsumeFree(effModel, usage.TotalTokens)
			}
		}()
		if lg.Enabled() {
			defer func() {
				var faID int
				var faAlias string
				if finalAcc != nil {
					faID = finalAcc.ID
					faAlias = finalAcc.DisplayAlias()
				} else if finalFb != "" {
					faAlias = finalFb
				}
				outcome := outcomeName(finalOutcome)
				if noAccounts {
					outcome = "no_accounts"
				}
				var pt, ct, tt, cache int
				if usage != nil {
					pt, ct, tt, cache = usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, usage.CachedTokens
				}
				retries := 0
				if n := len(attempts); n > 0 {
					retries = n - 1
				}
				lg.Log(reqlog.Event{
					ID:               newReqID(),
					Ts:               start.UnixMilli(),
					Proto:            "openai",
					Model:            effModel,
					Stream:           payload.Stream,
					Bytes:            len(body),
					KeyHash:          hashKey(affinityKey),
					Warm:             warm,
					Cold:             cold,
					Attempts:         attempts,
					Retries:          retries,
					FinalAccountID:   faID,
					FinalAlias:       faAlias,
					Status:           c.Writer.Status(),
					Outcome:          outcome,
					DurMs:            time.Since(start).Milliseconds(),
					TTFTMs:           ttft,
					PromptTokens:     pt,
					CompletionTokens: ct,
					TotalTokens:      tt,
					CachedTokens:     cache,
				})
			}()
		}

		tried := make([]int, 0, p.Len())
		for i := 0; i < p.Len(); i++ {
			acc := p.Pick(affinityKey, cold, effModel, tried...)
			if acc == nil {
				break
			}

			attemptStart := time.Now()
			accURL := effectiveUpstream(upstream, acc.WSDomain) + "/chat/completions"
			fr := forward(c.Request.Context(), accURL, body, acc.APIKey)
			if fr.retried {
				networkRetry(acc)
			}
			if fr.err != nil {
				// transient upstream error — exclude this account for the rest
				// of THIS request and try the next. affinity mode would otherwise
				// re-pick the same account every loop (the key is unchanged and
				// the account wasn't Cooldown'd) and burn all retries on it.
				// forward already retried once in place on this account; if that
				// also failed we move on rather than burning the whole loop here.
				attempts = append(attempts, reqlog.Attempt{
					AccountID: acc.ID, Alias: acc.DisplayAlias(),
					Status: 0, Outcome: "net_err",
					DurMs: time.Since(attemptStart).Milliseconds(), Err: fr.err.Error(),
				})
				tried = append(tried, acc.ID)
				continue
			}
			resp := fr.resp

			switch resp.StatusCode {
			case 429:
				attempts = append(attempts, reqlog.Attempt{
					AccountID: acc.ID, Alias: acc.DisplayAlias(),
					Status: 429, Outcome: "429", DurMs: time.Since(attemptStart).Milliseconds(),
				})
				resp.Body.Close()
				p.CooldownModel(acc, effModel, 60*time.Second)
				throttle(acc)
				tried = append(tried, acc.ID)
				continue
			case 200:
				attempts = append(attempts, reqlog.Attempt{
					AccountID: acc.ID, Alias: acc.DisplayAlias(),
					Status: 200, Outcome: "ok", DurMs: time.Since(attemptStart).Milliseconds(),
				})
				defer resp.Body.Close()
				finalAcc = acc
				finalOutcome = stats.OK
				p.RecordSuccess(acc)
				if payload.Stream {
					// Per-line passthrough (SSE is a line protocol; ReadString
					// preserves exact bytes incl. \n). On the first data: line
					// we capture TTFT, and on the usage-bearing final chunk we
					// extract token counts — both cheap (one Contains gate per
					// line, parse only on hit).
					ttft, usage = streamOpenAIResponse(c, resp.Body, start, lg)
				} else {
					data, _ := io.ReadAll(resp.Body)
					if lg.Enabled() {
						if u := extractUsage(data); u != nil {
							usage = u
						}
					}
					c.Data(200, resp.Header.Get("Content-Type"), data)
				}
				return
			default:
				if shouldRetryStatus(resp.StatusCode) {
					// Account-attributable 4xx (401/403/408/418/451/…): not the
					// request's fault, and not the per-key QPS limit that 429
					// adaptive backoff addresses. Exclude this account for the
					// rest of THIS request and try the next. No Cooldown — a
					// transient 408 shouldn't park the account, and a persistent
					// 401 self-heals: excluding a mapped account makes Pick
					// re-pin the session to a healthy one on the next iteration
					// (see Pick's exclude fall-through).
					//
					// A 403 is special-cased: two consecutive 403s auto-disable
					// the account (RecordForbidden), since a sustained 403 from
					// dashscope is an account billing/authorization problem, not
					// a transient blip. The disable + a short cooldown make the
					// scheduler drop it while the retry loop rotates to the next.
					attempts = append(attempts, reqlog.Attempt{
						AccountID: acc.ID, Alias: acc.DisplayAlias(),
						Status: resp.StatusCode, Outcome: "4xx_retry", DurMs: time.Since(attemptStart).Milliseconds(),
					})
					if resp.StatusCode == 403 {
						streak, disabled := p.RecordForbidden(acc)
						p.Cooldown(acc, 60*time.Second)
						if disabled {
							log.Printf("[pool] account %s (id=%d) auto-disabled after %d consecutive 403s (billing/credential problem)", acc.DisplayAlias(), acc.ID, streak)
						}
					}
					resp.Body.Close()
					tried = append(tried, acc.ID)
					continue
				}
				// Request-level 4xx (400/404/413/422) or any 5xx: not account-
				// attributable, so switching accounts won't help — pass the
				// upstream response through as-is.
				aOutcome := "4xx_pass"
				if resp.StatusCode >= 500 {
					aOutcome = "5xx_pass"
				}
				attempts = append(attempts, reqlog.Attempt{
					AccountID: acc.ID, Alias: acc.DisplayAlias(),
					Status: resp.StatusCode, Outcome: aOutcome, DurMs: time.Since(attemptStart).Milliseconds(),
				})
				defer resp.Body.Close()
				o := stats.ClientError
				if resp.StatusCode >= 500 {
					o = stats.ServerError
				}
				finalAcc = acc
				finalOutcome = o
				c.DataFromReader(resp.StatusCode, resp.ContentLength,
					resp.Header.Get("Content-Type"), resp.Body, nil)
				return
			}
		}

		// Pool exhausted (no account served a 200). Last resort: try fallback
		// channels eligible for the ORIGINAL client model — model_override is
		// dashscope-specific and must not be forced onto another platform, so the
		// fallback body restores payload.Model when an override was applied. The
		// 200 handler mirrors the in-loop branch: stream pipes via
		// streamOpenAIResponse, non-stream reads once + extracts usage. A
		// non-nil fbs is the whole feature flag (no fallbacks configured → skip).
		if fbs != nil {
			fbBody := body
			if modelOverride != "" && m != nil {
				m["model"], _ = json.Marshal(payload.Model)
				fbBody, _ = json.Marshal(m)
			}
			ok := func(c *gin.Context, resp *http.Response) (*int64, *tokenUsage) {
				defer resp.Body.Close()
				if payload.Stream {
					return streamOpenAIResponse(c, resp.Body, start, lg)
				}
				data, _ := io.ReadAll(resp.Body)
				var u *tokenUsage
				if lg.Enabled() {
					if eu := extractUsage(data); eu != nil {
						u = eu
					}
				}
				c.Data(200, resp.Header.Get("Content-Type"), data)
				return nil, u
			}
			if served, name, t, u := tryFallback(c, fbs, payload.Model, fbBody, ok, &attempts); served {
				finalFb = name
				finalOutcome = stats.OK
				ttft, usage = t, u
				return
			}
		}

		noAccounts = true
		c.JSON(503, gin.H{"error": "no available accounts"})
	}
}

func ClearAccounts(p *pool.Pool, save func([]pool.AccountConfig) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		p.Clear()
		if err := save(p.Configs()); err != nil {
			log.Printf("[warn] save config: %v", err)
		}
		c.JSON(200, gin.H{"ok": true})
	}
}

func AddAccount(p *pool.Pool, save func([]pool.AccountConfig) error, st *stats.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var cfg pool.AccountConfig
		if err := c.ShouldBindJSON(&cfg); err != nil || cfg.APIKey == "" {
			c.JSON(400, gin.H{"error": "api_key is required"})
			return
		}
		id := p.Add(cfg)
		acc := p.ByID(id)

		// Synchronous initial balance check so the returned status carries the
		// real balance (not 0). This uses the account's AccessKey to call BSS
		// QueryCashCoupons — a DIFFERENT credential from the API key used for
		// chat (probe/test-all). A valid API key does not imply a working
		// AccessKey, so surface the error to the caller instead of swallowing
		// it: the frontend can then tell the user exactly why the balance is 0.
		balanceErr := ""
		if err := monitor.CheckBalance(p, acc, st); err != nil {
			log.Printf("[warn] initial balance check: %v", err)
			balanceErr = err.Error()
		}

		// Start the periodic monitor after the synchronous check so the two
		// don't race on SetBalance for the response. Its own initial check is
		// redundant (re-sets the same value) but harmless; adds are rare.
		ctx, cancel := context.WithCancel(context.Background())
		p.SetCancelByID(id, cancel)
		go monitor.Run(ctx, p, acc, st)

		if err := save(p.Configs()); err != nil {
			log.Printf("[warn] save config: %v", err)
		}
		for _, s := range p.All() {
			if s.ID == id {
				c.JSON(200, struct {
					pool.Status
					BalanceError string `json:"balance_error,omitempty"`
				}{Status: s, BalanceError: balanceErr})
				return
			}
		}
		c.JSON(200, gin.H{"ok": true, "balance_error": balanceErr})
	}
}

func ListAccounts(p *pool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) { c.JSON(200, p.All()) }
}

func ToggleAccount(p *pool.Pool, save func([]pool.AccountConfig) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil || !p.ToggleByID(id) {
			c.JSON(400, gin.H{"error": "invalid id"})
			return
		}
		// Persist the toggled enabled state so it survives a restart (was
		// memory-only before — toggled-off accounts re-enabled on restart).
		if err := save(p.Configs()); err != nil {
			log.Printf("[warn] save config on toggle: %v", err)
		}
		c.JSON(200, gin.H{"ok": true})
	}
}

func ClearCooldown(p *pool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil || !p.ClearCooldownByID(id) {
			c.JSON(400, gin.H{"error": "invalid id"})
			return
		}
		c.JSON(200, gin.H{"ok": true})
	}
}

func DeleteAccount(p *pool.Pool, save func([]pool.AccountConfig) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil || !p.RemoveByID(id) {
			c.JSON(400, gin.H{"error": "invalid id"})
			return
		}
		if err := save(p.Configs()); err != nil {
			log.Printf("[warn] save config: %v", err)
		}
		c.JSON(200, gin.H{"ok": true})
	}
}

func RefreshAccount(p *pool.Pool, st *stats.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			c.JSON(400, gin.H{"error": "invalid id"})
			return
		}
		acc := p.ByID(id)
		if acc == nil {
			c.JSON(400, gin.H{"error": "invalid id"})
			return
		}
		if err := monitor.CheckBalance(p, acc, st); err != nil {
			// Surface the specific upstream/AK error to the UI instead of a
			// generic 500. A valid chat API key does NOT guarantee the AccessKey
			// is correct (they are separate credentials); return the error body
			// so the frontend can show what actually went wrong.
			c.JSON(200, gin.H{"ok": false, "error": err.Error()})
			return
		}
		for _, s := range p.All() {
			if s.ID == id {
				c.JSON(200, struct {
					pool.Status
					OK bool `json:"ok"`
				}{Status: s, OK: true})
				return
			}
		}
		c.JSON(200, gin.H{"ok": true})
	}
}

// StatsHandler returns windowed aggregates merged with live pool state.
func StatsHandler(p *pool.Pool, st *stats.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		w := stats.Window(c.DefaultQuery("window", string(stats.Window24h)))
		snap, err := st.Snapshot(w)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		live := p.All()
		// Index aggregates by ID once instead of a nested linear scan per
		// live account (was O(live × accounts)).
		aggByID := make(map[int]stats.Agg, len(snap.Accounts))
		for _, a := range snap.Accounts {
			aggByID[a.ID] = a.Agg
		}
		perAccount := make([]gin.H, 0, len(live))
		for _, s := range live {
			acc := p.ByID(s.ID)
			agg := aggByID[s.ID]
			bh := snap.Balances[s.ID]
			if bh == nil {
				bh = []stats.BalancePoint{}
			}
			// disabled_by_403 is true when the account was taken out of
			// rotation by the consecutive-403 auto-disable (a billing/credential
			// problem), as opposed to a manual toggle or a balance-driven
			// disable. Lets the UI surface "fix billing then re-enable" instead
			// of a bare disabled flag.
			disabledBy403 := false
			if acc != nil {
				disabledBy403 = p.DisabledByForbidden(acc)
			}
			perAccount = append(perAccount, gin.H{
				"id":               s.ID,
				"alias":            s.Alias,
				"requests_window":  agg.Req,
				"success_rate":     successRate(agg),
				"throttled_window": agg.R429,
				"balance":          s.Balance,
				"balance_history":  bh,
				"enabled":          s.Enabled,
				"available":        s.Available,
				"cooldown_secs":    s.CooldownSecs,
				"disabled_by_403":  disabledBy403,
			})
		}
		c.JSON(200, gin.H{
			"window": snap.Window,
			"global": gin.H{
				"total_balance":          sumBalance(live),
				"available":              countAvailable(live),
				"total":                  len(live),
				"requests_total":         totalRequestCount(live),
				"requests_window":        snap.Global.Req,
				"success_rate":           successRate(snap.Global),
				"errors_window":          snap.Global.Err,
				"throttled_429_window":   snap.Global.R429,
				"network_retries_window": snap.Global.NetRetry,
				"stats_queue":            st.QueueLen(),
				"stats_dropped":          st.SwapDropped(),
			},
			"timeline":    snap.Timeline,
			"per_account": perAccount,
		})
	}
}

func successRate(a stats.Agg) float64 {
	if a.Req == 0 {
		return 0
	}
	return float64(a.Ok) / float64(a.Req)
}

func sumBalance(s []pool.Status) float64 {
	var sum float64
	for _, x := range s {
		sum += x.Balance
	}
	return sum
}

func countAvailable(s []pool.Status) int {
	n := 0
	for _, x := range s {
		if x.Available {
			n++
		}
	}
	return n
}

func totalRequestCount(s []pool.Status) int64 {
	var n int64
	for _, x := range s {
		n += x.RequestCount
	}
	return n
}

// parseAffinity extracts the affinity key (first user message text) and a
// warmth signal from the already-extracted "messages" JSON value of a request
// body (the OpenAI handler passes m["messages"], the RawMessage it unmarshaled
// once into map[string]json.RawMessage). The key routes a whole conversation to
// one account so its prefix cache gets reused; warmth is true when the
// conversation already has at least one assistant turn — i.e. it is a multi-turn
// session that has "warmed up", so its prefix cache matters and it should run on
// a healthy high-balance account. A fresh single-turn request has no assistant
// turn → cold, and is a candidate for low-balance drain routing. The messages
// value is unmarshaled once here; the role scan is O(n) in the number of
// messages, not token count — and importantly NOT O(body size), since the
// caller hands us just the messages slice, not the whole body. (The Anthropic
// handler does the equivalent scan inline over its already-parsed areq.Messages
// at anthropic.go:158 and does not call this.) An empty/nil RawMessage yields
// ("", false), matching the prior body-parse-error fallback.
func parseAffinity(messages json.RawMessage) (key string, warm bool) {
	var req []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(messages, &req) != nil {
		return "", false
	}
	for _, m := range req {
		if m.Role == "assistant" {
			warm = true
		}
		if m.Role == "user" && key == "" {
			key = contentToText(m.Content)
		}
	}
	return key, warm
}

// contentToText flattens a content field (plain string or array of blocks) into
// text. Text blocks are concatenated; tool_result blocks recurse into theirs.
func contentToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text":
			sb.WriteString(b.Text)
		case "tool_result":
			sb.WriteString(contentToText(b.Content))
		}
	}
	return sb.String()
}

// streamOpenAIResponse pipes an OpenAI SSE stream to the client line-by-line
// while opportunistically capturing two observability signals: TTFT (elapsed
// until the first data: line) and the token/cache usage carried by the final
// chunk. ReadString('\n') preserves exact bytes (incl. the trailing newline),
// so the client sees a byte-identical stream; the per-line scan is the cost we
// pay for usage extraction (one cheap Contains gate, a parse only on hit).
//
// Flushing: an SSE frame is `data: {...}\n\n`. The client's parser can only
// dispatch a message once the terminating blank line arrives, so flushing after
// just the data line (the old per-non-empty-line flush) only doubled the
// syscall count without delivering anything the client could act on sooner. We
// write every line immediately (so bytes aren't held in our own buffer) but
// flush once per frame — on the blank terminator — plus a final flush after the
// loop to cover a stream that ends without a trailing blank line.
func streamOpenAIResponse(c *gin.Context, body io.Reader, start time.Time, lg *reqlog.Logger) (ttft *int64, usage *tokenUsage) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(200)
	fl, _ := c.Writer.(http.Flusher)
	br := bufio.NewReader(body)
	for {
		line, rerr := br.ReadString('\n')
		if line != "" {
			c.Writer.WriteString(line) //nolint
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data:") {
				if ttft == nil {
					t := time.Since(start).Milliseconds()
					ttft = &t
				}
				if lg.Enabled() && usage == nil {
					payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
					if u := extractUsage([]byte(payload)); u != nil {
						usage = u
					}
				}
			}
			// Flush once per SSE frame: the blank line terminates a data: event,
			// which is exactly when the client can dispatch it. (Latency-equivalent
			// to the old per-line flush since onmessage can't fire before this.)
			if fl != nil && trimmed == "" {
				fl.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	// Cover a stream that ends on a data: line with no trailing blank line.
	if fl != nil {
		fl.Flush()
	}
	return ttft, usage
}

// outcomeName maps a stats outcome to the short string the request log uses.
func outcomeName(o stats.Outcome) string {
	switch o {
	case stats.OK:
		return "ok"
	case stats.ClientError:
		return "client_error"
	case stats.ServerError:
		return "server_error"
	case stats.Throttled:
		return "throttled"
	}
	return "unknown"
}
