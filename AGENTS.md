# AGENTS.md

This file provides guidance to Codex (Codex.ai/code) when working with code in this repository.

## Commands

### Local development (Windows)
```powershell
# Copy and edit config first (one-time)
copy config.yaml.example config.yaml

# Start both servers (opens two terminal windows)
.\dev.ps1
# Backend: http://localhost:<listen port from config.yaml, default :8080>
# Frontend: http://localhost:<frontend_port from config.yaml, default 5173>
```

> **Single-port dev mode**: set `BB_DEV=1` and the backend reverse-proxies the frontend to the vite dev server, so only the backend port is needed. Without it, `dev.ps1` runs backend + frontend separately.

### Backend
```powershell
cd backend
go run main.go          # standard run (requires config.yaml at repo root)
air                     # hot-reload (requires: go install github.com/air-verse/air@latest)
go build ./...          # compile check
go test ./...           # run tests
```

### Frontend
```bash
cd frontend
npm run dev             # dev server
npm run build           # tsc + vite build
```

### Docker
```bash
docker compose up -d    # production stack
./scripts/deploy.sh     # interactive deploy with optional Caddy/HTTPS setup
```

## Architecture

BudgetBridge is a reverse proxy that aggregates multiple Alibaba Cloud accounts (each with DashScope voucher credits) into a single API endpoint, compatible with both OpenAI and Anthropic clients. It ships with an admin dashboard (account pool + usage stats).

### Backend (`backend/`)

**`main.go`** — Entry point. Resolves `config.yaml` by walking up from CWD (skipping any `backend/` dir, so it stays at the repo root). On first run it auto-hashes a plaintext `admin_password` to `admin_password_hash` and auto-generates the unified `api_key` if empty, writing both back to `config.yaml`. Inits the stats store, constructs the pool, spawns one balance-monitor goroutine per account, and registers Gin routes. Provides two write-back closures: `saver` (accounts only) and `saveAll` (full config, for api-key rotation). Proxy endpoints are guarded by `keyAuth`, which reads `cfg.APIKey` on every request so rotation takes effect live. `NoRoute` tolerates missing/duplicated `/v1` and routes by suffix; in dev mode it proxies to vite.

**`internal/pool/pool.go`** — Thread-safe account pool with two schedulers (selected by the `scheduler` config, default `affinity`):
- `Pick(key, cold, exclude...)` — unified selector. In `affinity` mode with a non-empty key it decides in three tiers:
  1. **Session map** (`lookupSession`) — if the key is already mapped to an account that is available and at/above `low_balance_floor`, reuse it unconditionally. Affinity ranks strictly above weighting so ongoing conversations stay pinned (prefix cache reuse) even if another account's weight rises. A mapped account *below* the floor is reused for **cold** requests (keep draining — no cache to lose) but a **warm** request falls through to the picker, which migrates and re-pins to a healthy account.
  2. **Weighted Rendezvous hashing** (`hrwPick` / `drainPick` — shared `rhwPickWithScore` skeleton) — for fresh keys (or when the mapped account is excluded/unavailable/below the floor). `score_k = score_fn_k / -ln(h_k)` where `h_k = hash(key + "#" + nodeID) ∈ (0,1)`; highest score wins. Warm keys use `pickScore()` (= bucketed `Weight()`, see below); **cold** keys (when `cold_drain` is on) use `drainScore()` = `1 - W + minWeight`, which *inverts* the bias so fresh low-payload conversations drain low-balance accounts instead of piling onto healthy ones. `Weight()` is **bucketed by balance tier** (~50-yuan bands, gradient ~1.1–1.2× per tier) rather than continuous, so within-tier balance wobble never changes the argmax and ongoing sessions don't flap; the bottom tier collapses to `minWeight` so a near-depleted account still drains. `pickScore()` additionally scales an account's weight DOWN linearly while it is under 429 cooldown (just-cooldown → 20%, near-expiry → ~100%, floored at `minWeight`).
  3. **Round-robin** (`Next`) — fallback when all candidates are excluded or unavailable.
- `Next(exclude)` — the round-robin path, also used directly in `round_robin` mode or on empty key. `exclude` is a per-request set of account IDs already tried so a transport error advances to the next account without `Cooldown`-ing (which would mutate shared pool state); the proxy retry loop populates it.
- The affinity key is the **first user message text** (extracted by the proxy via `parseAffinity`, which also returns a `warm` flag = the conversation already has an assistant turn); **cold** = `!warm && len(body) < cold_request_bytes`. Cold-drain is the lever for "drain low-balance accounts with throwaway small requests; let long conversations cache on healthy accounts."
- Accounts carry a **stable ID** (`nextID`, never reused on remove); CRUD uses `ByID`/`*ByID` rather than positional index so deleting an account never reroutes operations. `nodeID` is `alias` (or API-key prefix) + `#ID`, so duplicate aliases don't collide on the hash.
- `SetBalance` auto-disables accounts below ¥3. On 429 the proxy calls `CooldownAdaptive(acc)` (escalates 20→40→80→160→320s with repeated 429s, de-escalating one step per `RecordSuccess` and resetting after `backoffReset`=2min idle — 429s here are per-key QPS limits, not balance exhaustion); on 200 it calls `RecordSuccess(acc)`. A sustained 403 (account billing/credential problem — voucher revoked / AK disabled / arrears) is handled by `RecordForbidden(acc)`: two consecutive 403s auto-disable the account (`forbiddenThreshold`=2; a single transient 403 only excludes it for the rest of that request); `RecordSuccess` resets the streak, and `ToggleByID` re-enabling clears it so the operator's "fixed the billing" toggle starts fresh. `DisabledByForbidden(acc)` lets the UI distinguish a 403 auto-disable from a manual/balance disable. The session map is in-memory (TTL 30 min, janitor sweeps every 10 min, hard cap 100k entries) and **lost on restart**; so are the per-account backoff levels and the 403 streak.

**`internal/monitor/monitor.go`** — Queries Alibaba BSS `QueryCashCoupons` for voucher balance every 5 min per account, calls `pool.SetBalance` and `stats.RecordBalance`.

**`internal/proxy/proxy.go`** — OpenAI-compatible handler (`POST /v1/chat/completions`). Retry loop (up to pool size): pick → forward; 429 → adaptive cooldown + record + next; 200 → stream-pipe or `DataFromReader` + `RecordSuccess`; account-attributable 4xx (`shouldRetryStatus`: 401/403/408/418/425/429/449/451, defined in `retry.go`) → exclude + next (no cooldown, self-heals on the next pick); a **403** additionally calls `RecordForbidden` (2 consecutive → auto-disable + 60s cooldown + log) before excluding; request-level 4xx (400/404/413/422) and 5xx → passthrough as-is. Records per-attempt (`RecordAccount`) and per-request (`RecordGlobal`) stats. `parseAffinity` extracts the affinity key + warmth; `cold := !warm && len(body) < p.ColdRequestBytes()`. Also holds the admin CRUD handlers and `StatsHandler` (merges DB aggregates with live pool state, incl. a `disabled_by_403` flag per account). A per-request `reqlog.Event` trace (attempts/TTFT/usage) is built across the loop and submitted once via `lg.Log` in a defer; when logging is on, streams inject `stream_options.include_usage` and the stream pipe (`streamOpenAIResponse`) scans per-line for TTFT + the final usage chunk; non-stream reads the body once to extract usage. 每个账号可选 ws_domain（业务空间专属域名）：handler 在 retry 循环里用 effectiveUpstream(global_upstream, acc.WSDomain) 现算该账号的上游 base（含 /chat/completions），覆盖全局 upstream_url；空则回退全局。结果按 (global_upstream, ws_domain) 记忆化（sync.Map），命中后不再每请求 url.Parse（主要成本），仅一次小的 key 拼接（实测约 1 alloc/op）。Anthropic handler 与 probe(TestAccount/TestAll) 同样处理。

**`internal/proxy/anthropic.go`** — Anthropic↔OpenAI translation layer (`POST /v1/messages`). Converts messages/tools/system (preserving `cache_control` markers for dashscope explicit caching; mapping `tool_use`/`tool_result`), then reuses the same select/retry/stats loop. Translates responses back, including streaming `content_block_*` SSE events. Symmetric `reqlog.Event` tracing: the translator captures TTFT (first content delta) and usage (final chunk) and reflects cache hits back to the Anthropic `cache_read_input_tokens` usage field.

**`internal/proxy/probe.go`** — `TestAccount` / `TestAll`. Probes upstream directly with an account's own key (bypasses the pool selector); `TestAll` runs all accounts concurrently.

**`internal/stats/store.go`** — SQLite persistence (pure-Go `modernc.org/sqlite`, WAL mode) for metrics. Three tables — `account_minute`, `global_minute` (per-minute req/ok/err/r429), and `balance_snap`. Rolling 7-day retention swept every 10 min; windowed aggregates with downsampling (24h→5-min buckets, 7d→60-min). A single `sync.Mutex` serializes writes — fine for single-process.

**`internal/reqlog/`** — Per-request scheduling traces for the dashboard's live stream + history. Each proxied request produces one `Event` (every attempt's account/status/duration, the outcome, total + TTFT timing, and token/cache usage). Two paths from one non-blocking `Log()` entry: a **ring buffer + subscriber fan-out** (SSE realtime, zero DB reads on the hot path) and a **single-writer SQLite store** (`reqlog_db`, default `data/reqlog.db`, rolling `reqlog_retention_days` default 7) for one-week history. `Log()` is a `select`-default channel send — it never blocks the proxy; one consumer goroutine owns all DB writes (no WAL contention). `Subscribe()` snapshots the ring for SSE replay; `Recent()` paginates history (`ts DESC`, `before` cursor). Mirrors the stats store's drop-on-full + atomic-counter pattern. Disabled (`reqlog_enabled: false`) → a no-op stub; the proxy then skips building events and never injects `stream_options.include_usage`.

**`internal/proxy/usage.go`** — `extractUsage` pulls token/cache counts from an OpenAI-compatible `usage` block (cache field name is not standardized across dashscope — tries `cached_tokens` / `prompt_tokens_details.cached_tokens` / `cache_read_input_tokens`). `hashKey` (FNV, affinity key → 8-hex, never raw) and `newReqID` (random hex) back the log events.

**`internal/auth/auth.go`** — Three layers: bcrypt admin password (plaintext auto-hashed on first run), in-memory Bearer session tokens (24h, background sweeper; **lost on restart**), and the unified proxy API key via `CheckAPIKey` (constant-time compare; accepts both `Authorization: Bearer` and `x-api-key`; disabled when the key is empty).

**`internal/devproxy/devproxy.go`** — When `BB_DEV=1`, the backend reverse-proxies frontend/HMR traffic to the vite dev server and lets POST fall through to the `/v1`-tolerating fallback.

### Config

`config.yaml` (repo root) is the single source of truth. It is **read at startup and written back at runtime** (account mutations, password hashing, api-key generation/rotation). Key fields:

```yaml
listen: ":8080"
frontend_port: 5173       # dev.ps1 / BB_DEV only
stats_db: "data/stats.db" # SQLite path; rolling 7-day retention
upstream_url: "..."       # DashScope compatible-mode endpoint (incl. /v1)
model_override: "..."     # replaces model field in every proxied request
public_url: "..."         # TopBar display; auto-derived from request host if empty
scheduler: "affinity"     # "affinity" (weighted Rendezvous, balance-aware) | "round_robin"
low_balance_floor: 20.0   # below this balance (¥): mapped sessions migrate off, but the floor weight keeps draining
cold_drain: true          # route fresh low-payload (no assistant turn, <cold_request_bytes) conversations to low-balance accounts to drain them; warm multi-turn keeps high-balance bias
cold_request_bytes: 8192  # body-size guard for the cold-drain classification
throttle_max_cooldown: 320 # cap (seconds) for adaptive 429 backoff escalation (20→40→80→160→320)
reqlog_enabled: true     # per-request log: /admin/events live stream + /admin/logs history; off → hot path untouched, no usage injection
reqlog_capacity: 1000    # in-memory ring for live SSE replay; history lives in reqlog_db
reqlog_db: "data/reqlog.db" # SQLite path for request logs; rolling reqlog_retention_days retention
reqlog_retention_days: 7 # how long request logs are kept
api_key: "sk-bb-..."      # proxy auth; auto-generated on first run; rotatable live
admin_password: "..."     # plaintext; hashed → admin_password_hash on first run
admin_password_hash: "..."# bcrypt hash; presence enables admin login
accounts: [...]           # alias / api_key / ak_id / ak_secret / ws_domain(可选)
```

### Frontend (`frontend/src/`)

Single-page React app, no state management library. `App.tsx` owns all state and runs two pollers: accounts (`GET /admin/accounts`, 10s) and stats (`GET /admin/stats?window=`, 15s). Any 401 dispatches a `bb:unauthorized` event → shows `LoginPage`. Layout: `StatOverview` + `TrendChart` + `ShareChart` (stats visualization, window-switchable) above an account grid of `AccountCard` (supports compact mode), with a `RequestLogStream` live feed below it (SSE per-request traces + "load earlier" history). Components: `TopBar`, `AccountCard`, `AddAccountModal`, `TestAllModal`, `LoginPage`, `Toast`, `RequestLogStream`, plus a `components/ui/` kit (`AnimatedNumber`, `KpiCard`, `Sparkline`, `CooldownRing`, `Skeleton`, `GlassModal`). Same-origin API; auth token kept in localStorage.

**`RequestLogStream` / `streamEvents`** — The live feed. Because `EventSource` can't send the Bearer header, `streamEvents` (`api.ts`) fetches `/admin/events` and parses SSE frames off the `ReadableStream` manually, reconnecting with exponential backoff (cap 30s) and surfacing 401 via `bb:unauthorized`. `App.tsx` keeps a 2000-entry client buffer (older history paged from `/admin/logs` on "load earlier"); a `pausedRef` lets pause toggle without re-subscribing. `fetchLogs` does the paginated history fetch.

### API surface

Proxy endpoints are guarded by the unified API key; admin endpoints require a login Bearer token (disabled when `admin_password_hash` is empty).

| Kind | Method + Path | Description |
|---|---|---|
| Proxy | `POST /v1/chat/completions` | OpenAI format (API key) |
| Proxy | `POST /v1/messages` | Anthropic format, translated to OpenAI upstream (API key) |
| Admin | `POST /admin/login` | Returns Bearer token |
| Admin | `GET /admin/config` | Returns `public_url` |
| Admin | `GET /admin/accounts` | Pool status |
| Admin | `POST /admin/accounts` | Add account + triggers balance check |
| Admin | `DELETE /admin/accounts` | Clear all accounts |
| Admin | `DELETE /admin/accounts/:id` | Remove account by stable ID (disable then delete; ID never reused) |
| Admin | `POST /admin/accounts/:id/toggle` | Enable/disable |
| Admin | `POST /admin/accounts/:id/refresh` | Immediate balance check |
| Admin | `POST /admin/accounts/:id/cooldown/clear` | Unfreeze 429 cooldown |
| Admin | `POST /admin/accounts/:id/test` | Probe one account's key |
| Admin | `POST /admin/test-all` | Probe all accounts concurrently |
| Admin | `GET /admin/stats?window=1h\|24h\|7d` | Windowed usage aggregates + timeline |
| Admin | `GET /admin/events` | SSE stream of per-request log events (replay + live; 15s heartbeat) |
| Admin | `GET /admin/logs?before=&limit=&account=&outcome=&q=` | Paginated historical request logs (newest-first; one-week retention) |
| Admin | `GET /admin/api-key` | Read current unified API key |
| Admin | `POST /admin/api-key/rotate` | Rotate unified API key (live) |
