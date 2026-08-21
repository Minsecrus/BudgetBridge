package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"budgetbridge/internal/auth"
	"budgetbridge/internal/devproxy"
	"budgetbridge/internal/fallback"
	"budgetbridge/internal/monitor"
	"budgetbridge/internal/pool"
	"budgetbridge/internal/proxy"
	"budgetbridge/internal/reqlog"
	"budgetbridge/internal/stats"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen          string  `yaml:"listen"`
	FrontendPort    int     `yaml:"frontend_port"`
	UpstreamURL     string  `yaml:"upstream_url"`
	UpstreamProxy   string  `yaml:"upstream_proxy,omitempty"`
	ModelOverride   string  `yaml:"model_override"`
	PublicURL       string  `yaml:"public_url"`
	APIKey          string  `yaml:"api_key"`
	Scheduler       string  `yaml:"scheduler"`
	StatsDB         string  `yaml:"stats_db"`
	LowBalanceFloor float64 `yaml:"low_balance_floor"`
	// ColdDrain routes fresh, low-payload (no assistant turn, small body)
	// conversations to low-balance accounts so their remaining balance drains
	// instead of starving at minWeight. Warm multi-turn conversations keep the
	// high-balance HRW bias for prefix-cache locality. Pointer so a config.yaml
	// written before this field existed (no "cold_drain:" key) decodes to nil →
	// defaults to ON, matching the Enabled-account precedent; a plain bool would
	// decode absence to false and silently disable draining on existing deploys.
	ColdDrain *bool `yaml:"cold_drain"`
	// ColdRequestBytes is the body-size guard below which a fresh (no assistant
	// turn) request is eligible for cold-drain routing. Prevents a large single-
	// shot document (e.g. a 100KB paste with no prior turns) from being routed
	// onto a near-empty account. Default 8192.
	ColdRequestBytes int `yaml:"cold_request_bytes"`
	// FreeQuotaPerModel is the per-(account,model) free-token allowance each
	// account enjoys (e.g. 1M for glm-5.2 AND 1M for deepseek-v4-pro,
	// independently). When > 0, cold routing prefers accounts that still have
	// THIS model's free quota — recovering value from low-balance accounts whose
	// other models are bled dry. 0 disables (cold falls back to balance drain,
	// the pre-feature behavior).
	FreeQuotaPerModel int64 `yaml:"free_quota_per_model"`
	// ReqlogEnabled controls per-request logging: the /admin/events live SSE
	// stream and the /admin/logs one-week history. When nil (key absent) it
	// defaults to ON (pointer so existing configs aren't silently disabled).
	// When off, the proxy skips building log events and never injects
	// stream_options.include_usage into the request body.
	ReqlogEnabled       *bool                `yaml:"reqlog_enabled"`
	ReqlogCapacity      int                  `yaml:"reqlog_capacity"`
	ReqlogDB            string               `yaml:"reqlog_db"`
	ReqlogRetentionDays int                  `yaml:"reqlog_retention_days"`
	Accounts            []pool.AccountConfig `yaml:"accounts"`
	// Fallbacks are non-Alibaba OpenAI-compatible endpoints used only when the
	// account pool is exhausted (no balance to query, no weighted scheduling).
	// Each channel's Models whitelist gates which requests it can back up. The
	// registry is in-memory (rebuilt from config at startup, like accounts); IDs
	// are persisted per-item so they survive restarts.
	Fallbacks []fallback.Config `yaml:"fallbacks,omitempty"`
	// NextAccountID persists the pool's monotonic ID counter so a
	// cleared-then-restarted deployment doesn't reuse IDs whose 7-day stats
	// rows are still in the retention window.
	NextAccountID     int      `yaml:"next_account_id,omitempty"`
	AllowOrigins      []string `yaml:"allow_origins"`
	AdminPassword     string   `yaml:"admin_password,omitempty"`
	AdminPasswordHash string   `yaml:"admin_password_hash,omitempty"`
}

// findConfig resolves the project-root config.yaml. It walks up from CWD and
// returns the first config.yaml it finds, deliberately skipping any directory
// named "backend" — config must live at the repo root (never inside backend/)
// so it stays the single source of truth shared with dev.ps1, vite and
// docker-compose. In the Docker container the WORKDIR is /app, which is not
// "backend", so /app/config.yaml resolves normally.
func findConfig() string {
	dir, _ := os.Getwd()
	for {
		if filepath.Base(dir) != "backend" {
			p := filepath.Join(dir, "config.yaml")
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	log.Fatal("config.yaml not found in any parent directory")
	return ""
}

func main() {
	configPath := findConfig()
	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatalf("config.yaml: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	if cfg.UpstreamURL == "" {
		cfg.UpstreamURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.FrontendPort == 0 {
		cfg.FrontendPort = 5173
	}
	if cfg.Scheduler == "" {
		cfg.Scheduler = "affinity"
	}
	if cfg.LowBalanceFloor <= 0 {
		cfg.LowBalanceFloor = 20.0
	}
	if cfg.ColdRequestBytes <= 0 {
		cfg.ColdRequestBytes = 8192
	}
	if cfg.ColdDrain == nil {
		on := true
		cfg.ColdDrain = &on
	}
	if cfg.ReqlogCapacity <= 0 {
		cfg.ReqlogCapacity = 1000
	}
	if cfg.ReqlogDB == "" {
		cfg.ReqlogDB = "data/reqlog.db"
	}
	if cfg.ReqlogRetentionDays <= 0 {
		cfg.ReqlogRetentionDays = 7
	}
	if cfg.ReqlogEnabled == nil {
		on := true
		cfg.ReqlogEnabled = &on
	}

	// Configure the shared upstream HTTP client once, before serving any
	// request. A non-empty upstream_proxy routes upstream traffic through a
	// SOCKS5 gateway (typical for a cross-border deployment routing the
	// upstream leg over a premium line); empty → direct connection.
	proxy.ConfigureUpstream(cfg.UpstreamProxy)

	// Auto-hash plaintext admin_password on first run
	if cfg.AdminPassword != "" {
		hash, err := auth.HashPassword(cfg.AdminPassword)
		if err != nil {
			log.Fatalf("hash admin password: %v", err)
		}
		cfg.AdminPassword = ""
		cfg.AdminPasswordHash = hash
		out, _ := yaml.Marshal(cfg)
		if err := os.WriteFile(configPath, out, 0644); err != nil {
			log.Fatalf("save hashed password: %v", err)
		}
		log.Printf("admin_password hashed and saved to config.yaml")
	}

	// Auto-generate a unified proxy API key on first run
	if cfg.APIKey == "" {
		cfg.APIKey = auth.GenerateAPIKey()
		out, _ := yaml.Marshal(cfg)
		if err := os.WriteFile(configPath, out, 0644); err != nil {
			log.Fatalf("save generated api key: %v", err)
		}
		log.Printf("api_key auto-generated and saved to config.yaml")
	}

	// Warn loudly when admin auth is disabled: with no password hash every
	// /admin/* endpoint is unauthenticated. Acceptable for local dev only.
	if cfg.AdminPasswordHash == "" {
		log.Printf("WARNING: admin_password is not set — /admin/* endpoints are UNAUTHENTICATED. Set admin_password in config.yaml before exposing this service.")
	}

	// Stats DB (rolling 7-day retention). Path resolves relative to config.yaml
	// so it lives next to config at the repo root (and at /app in Docker).
	statsPath := cfg.StatsDB
	if statsPath == "" {
		statsPath = "data/stats.db"
	}
	if !filepath.IsAbs(statsPath) {
		statsPath = filepath.Join(filepath.Dir(configPath), statsPath)
	}
	if err := os.MkdirAll(filepath.Dir(statsPath), 0755); err != nil {
		log.Fatalf("mkdir stats dir: %v", err)
	}
	st, err := stats.Init(statsPath)
	if err != nil {
		log.Fatalf("stats init: %v", err)
	}
	defer st.Close()

	// Request-log store (per-request traces, one-week retention). Independent
	// DB file from stats so the two single-writers don't contend on the same
	// WAL. Path resolves relative to config.yaml, same convention as stats_db.
	reqlogPath := cfg.ReqlogDB
	if !filepath.IsAbs(reqlogPath) {
		reqlogPath = filepath.Join(filepath.Dir(configPath), reqlogPath)
	}
	if err := os.MkdirAll(filepath.Dir(reqlogPath), 0755); err != nil {
		log.Fatalf("mkdir reqlog dir: %v", err)
	}
	lg, err := reqlog.New(reqlogPath, cfg.ReqlogCapacity, cfg.ReqlogRetentionDays, *cfg.ReqlogEnabled)
	if err != nil {
		log.Fatalf("reqlog init: %v", err)
	}
	defer lg.Close() // runs before st.Close (LIFO) — drains pending log events

	// cfgMu guards every read/write of cfg's mutable fields (Accounts, APIKey,
	// NextAccountID) and serializes config.yaml writes. Defined before pool.New
	// so we can persist newly-assigned account IDs right after construction.
	var cfgMu sync.RWMutex

	p := pool.New(cfg.Accounts, cfg.Scheduler, cfg.LowBalanceFloor, *cfg.ColdDrain, cfg.ColdRequestBytes, cfg.FreeQuotaPerModel)
	// Restore the monotonic ID counter so a cleared-then-restarted deployment
	// doesn't reuse IDs that still have stale stats rows. No-op for legacy
	// configs without next_account_id (New already set nextID = maxID+1).
	p.SeedNextID(cfg.NextAccountID)

	// Fallback channel registry (in-memory, rebuilt from config at startup).
	// Never enters the pool — separate kind of entity (no balance/weight/cooldown).
	fbs := fallback.New(cfg.Fallbacks)

	// saver writes pool accounts back to config.yaml on add. The whole
	// marshal+write happens under cfgMu so concurrent admin mutations can't
	// interleave (which would lose an account) or race with keyAuth reads.
	// It also persists NextAccountID so the monotonic counter survives restarts.
	saver := func(cfgs []pool.AccountConfig) error {
		cfgMu.Lock()
		defer cfgMu.Unlock()
		cfg.Accounts = cfgs
		cfg.NextAccountID = p.NextID()
		out, err := yaml.Marshal(cfg)
		if err != nil {
			return err
		}
		return os.WriteFile(configPath, out, 0644)
	}
	// saveFallbacks writes the fallback registry back to config.yaml on add/
	// delete/toggle, under the same cfgMu so it can't interleave with account
	// mutations or keyAuth reads. Fallback channels have no monotonic-counter
	// persistence need (no stats rows keyed by their ID), so unlike saver it
	// writes only the slice.
	saveFallbacks := func(cfgs []fallback.Config) error {
		cfgMu.Lock()
		defer cfgMu.Unlock()
		cfg.Fallbacks = cfgs
		out, err := yaml.Marshal(cfg)
		if err != nil {
			return err
		}
		return os.WriteFile(configPath, out, 0644)
	}
	// Legacy accounts (no id in config.yaml) get IDs assigned in New; persist
	// them once so they survive restarts (otherwise stats would re-mismatch).
	if err := saver(p.Configs()); err != nil {
		log.Printf("[warn] persist account IDs: %v", err)
	}
	for i := 0; i < p.Len(); i++ {
		acc := p.Get(i)
		ctx, cancel := context.WithCancel(context.Background())
		p.SetCancelByID(acc.ID, cancel)
		go monitor.Run(ctx, p, acc, st)
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	origins := cfg.AllowOrigins
	if len(origins) == 0 {
		origins = []string{"*"}
		log.Printf("WARNING: CORS allows all origins — set allow_origins in config.yaml for production")
	}
	r.Use(gin.Recovery(), cors.New(cors.Config{
		AllowOrigins:     origins,
		AllowMethods:     []string{"GET", "POST", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "X-Api-Key"},
		AllowCredentials: false,
	}))
	// Per-request access log. log.Printf holds a mutex and writes stdout
	// synchronously, so under high concurrency it serializes every request.
	// Gate it behind dev mode (BB_DEV=1) — gin is forced to ReleaseMode
	// regardless, so gin.Mode() can't tell dev from prod here. Production skips
	// the middleware body entirely (no mutex, no I/O); dev keeps the trace for
	// debugging. Stats (the dashboard) covers production observability instead.
	logReq := devproxy.Enabled()
	r.Use(func(c *gin.Context) {
		if !logReq {
			c.Next()
			return
		}
		log.Printf("[req] %s %s", c.Request.Method, c.Request.URL.Path)
		c.Next()
		log.Printf("[res] %s %s → %d", c.Request.Method, c.Request.URL.Path, c.Writer.Status())
	})

	openai := proxy.Handler(p, cfg.UpstreamURL, cfg.ModelOverride, st, lg, fbs)
	anthropic := proxy.AnthropicHandler(p, cfg.UpstreamURL, cfg.ModelOverride, st, lg, fbs)

	// Canonical OpenAI / Anthropic endpoints (protected by the unified API key).
	// keyAuth reads cfg.APIKey on every request, so rotation takes effect live.
	// The read is under cfgMu so it can't race with a concurrent rotation write.
	keyAuth := func(c *gin.Context) {
		cfgMu.RLock()
		key := cfg.APIKey
		cfgMu.RUnlock()
		if auth.CheckAPIKey(c, key) {
			c.Next()
		}
	}
	r.POST("/v1/chat/completions", keyAuth, openai)
	r.POST("/v1/messages", keyAuth, anthropic)
	// OpenAI-style balance endpoints polled by new-api/one-api to read the
	// channel's remaining quota. Same unified-key guard as the proxy routes, so
	// BudgetBridge reports its live aggregate balance as a self-describing
	// OpenAI channel. See internal/proxy/billing.go.
	r.GET("/v1/dashboard/billing/subscription", keyAuth, proxy.SubscriptionHandler(p))
	r.GET("/v1/dashboard/billing/usage", keyAuth, proxy.UsageHandler(p))

	// Dev mode (BB_DEV=1): backend becomes the single entry point and
	// reverse-proxies the frontend to the vite dev server. Constructed once;
	// nil in production (no BB_DEV) — MaybeProxy then always uses the fallback.
	var viteProxy *httputil.ReverseProxy
	if devproxy.Enabled() {
		target := fmt.Sprintf("http://localhost:%d", cfg.FrontendPort)
		vp, err := devproxy.New(target)
		if err != nil {
			log.Fatalf("dev proxy target %s: %v", target, err)
		}
		viteProxy = vp
		log.Printf("dev mode on (BB_DEV=1): reverse-proxying frontend to %s", target)
	}

	// NoRoute: in dev mode, GET/ws requests go to vite (frontend/HMR); POST
	// falls through to the API toleration fallback. When dev mode is off,
	// MaybeProxy delegates everything to the fallback — identical to before.
	noRouteFallback := func(c *gin.Context) {
		// Tolerate base-URL misconfiguration around the /v1 prefix: clients
		// that omit /v1 (→ /chat/completions) or duplicate it (→ /v1/v1/messages)
		// are routed by endpoint suffix so any spelling reaches the right handler.
		if c.Request.Method != http.MethodPost {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		// Codex CLI talks the OpenAI Responses API (/v1/responses), which the
		// DashScope upstream doesn't speak. Return a clear, client-visible hint
		// instead of the generic 404 so the user sees it in their CLI. Checked
		// before keyAuth: the hint should show even if the caller hasn't
		// configured a valid API key (the hint itself isn't sensitive).
		if strings.HasSuffix(c.Request.URL.Path, "/responses") {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
				"message": "当前渠道不适配 Codex CLI 工具，请您使用 Claude Code 等类似工具进行访问。",
				"type":    "invalid_request_error",
				"code":    "unsupported_endpoint",
			}})
			return
		}
		cfgMu.RLock()
		key := cfg.APIKey
		cfgMu.RUnlock()
		if !auth.CheckAPIKey(c, key) {
			return
		}
		switch p := c.Request.URL.Path; {
		case strings.HasSuffix(p, "/chat/completions"):
			openai(c)
		case strings.HasSuffix(p, "/messages"):
			anthropic(c)
		default:
			c.JSON(http.StatusNotFound, gin.H{"error": "not found", "path": p})
		}
	}
	r.NoRoute(devproxy.MaybeProxy(viteProxy, noRouteFallback))

	r.POST("/admin/login", proxy.LoginHandler(cfg.AdminPasswordHash))

	adm := r.Group("/admin")
	adm.Use(auth.Middleware(cfg.AdminPasswordHash))
	adm.GET("/config", func(c *gin.Context) {
		pubURL := cfg.PublicURL
		if pubURL == "" {
			scheme := "http"
			if c.Request.TLS != nil {
				scheme = "https"
			}
			if fwd := c.GetHeader("X-Forwarded-Proto"); fwd != "" {
				scheme = fwd
			}
			host := c.Request.Host
			if idx := strings.Index(host, ":"); idx != -1 {
				host = host[:idx]
			}
			pubURL = fmt.Sprintf("%s://%s:%s", scheme, host, strings.TrimPrefix(cfg.Listen, ":"))
		}
		c.JSON(200, gin.H{"public_url": pubURL})
	})
	adm.GET("/accounts", proxy.ListAccounts(p))
	adm.POST("/accounts", proxy.AddAccount(p, saver, st))
	adm.DELETE("/accounts", proxy.ClearAccounts(p, saver))
	adm.DELETE("/accounts/:id", proxy.DeleteAccount(p, saver))
	adm.POST("/accounts/:id/toggle", proxy.ToggleAccount(p, saver))
	adm.POST("/accounts/:id/refresh", proxy.RefreshAccount(p, st))
	adm.POST("/accounts/:id/cooldown/clear", proxy.ClearCooldown(p))
	adm.GET("/stats", proxy.StatsHandler(p, st))
	adm.GET("/events", proxy.EventsHandler(lg))
	adm.GET("/logs", proxy.LogsHandler(lg))
	adm.POST("/accounts/:id/test", proxy.TestAccount(p, cfg.UpstreamURL, cfg.ModelOverride))
	adm.POST("/test-all", proxy.TestAll(p, cfg.UpstreamURL, cfg.ModelOverride))

	// Fallback channel registry (pool-exhausted backup). No balance → no
	// refresh; no cooldown → no cooldown/clear. CRUD mirrors /admin/accounts.
	adm.GET("/fallbacks", proxy.ListFallbacks(fbs))
	adm.POST("/fallbacks", proxy.AddFallback(fbs, saveFallbacks))
	adm.DELETE("/fallbacks/:id", proxy.DeleteFallback(fbs, saveFallbacks))
	adm.POST("/fallbacks/:id/update", proxy.UpdateFallback(fbs, saveFallbacks))
	adm.POST("/fallbacks/:id/toggle", proxy.ToggleFallback(fbs, saveFallbacks))
	adm.POST("/fallbacks/:id/test", proxy.TestFallback(fbs))

	adm.GET("/api-key", func(c *gin.Context) {
		cfgMu.RLock()
		key := cfg.APIKey
		cfgMu.RUnlock()
		c.JSON(200, gin.H{"api_key": key})
	})
	adm.POST("/api-key/rotate", func(c *gin.Context) {
		// Rotate atomically: stage the new key, persist to disk, and keep it
		// only if the write succeeded — otherwise roll back so the live key
		// always matches what's on disk. Held under the write lock so no request
		// observes a half-rotated state. (Inlined instead of calling saveAll,
		// which would re-lock cfgMu and deadlock.)
		cfgMu.Lock()
		defer cfgMu.Unlock()
		newKey := auth.GenerateAPIKey()
		prev := cfg.APIKey
		cfg.APIKey = newKey
		out, err := yaml.Marshal(cfg)
		if err != nil {
			cfg.APIKey = prev
			c.JSON(500, gin.H{"error": "marshal: " + err.Error()})
			return
		}
		if err := os.WriteFile(configPath, out, 0644); err != nil {
			cfg.APIKey = prev
			c.JSON(500, gin.H{"error": "save: " + err.Error()})
			return
		}
		log.Printf("api_key rotated")
		c.JSON(200, gin.H{"api_key": newKey})
	})

	// Custom http.Server (not gin's r.Run) so we control graceful shutdown:
	// on SIGTERM we stop accepting new conns and let in-flight requests
	// (incl. long streaming completions) finish before the process exits.
	// ReadHeaderTimeout bounds slowloris-style clients that open a connection
	// then trickle bytes — without it a handful of held sockets could exhaust
	// the server's fd table (#4). WriteTimeout is intentionally unset: a global
	// write timeout would truncate legitimate long streams; the per-request
	// ResponseHeaderTimeout on the upstream client already guards the
	// wait-for-first-byte phase.
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Serve in a goroutine; the main goroutine blocks on the shutdown signal so
	// the deferred st.Close() below actually runs — log.Fatal would skip all
	// defers and drop unflushed stats events (#A2 in the graceful-shutdown fix).
	go func() {
		log.Printf("BudgetBridge on %s (upstream: %s)", cfg.Listen, cfg.UpstreamURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	// Wait for SIGINT/SIGTERM. Docker sends SIGTERM on `docker stop`/`up -d`
	// during a recreate; the compose stop_grace_period is the hard kill deadline.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Printf("received %s — draining in-flight requests (max 5m)...", sig)

	// Stop the monitor goroutines first: their cancel funcs stop background
	// balance polling so no new stats/balance events arrive while the stats
	// store is draining its channel.
	p.StopMonitors()

	// Shutdown waits for active handlers to return. A 5-minute ceiling matches
	// the longest reasonable streaming completion; compose's stop_grace_period
	// must be ≥ this so the container isn't SIGKILLed mid-drain.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v (some in-flight requests may have been cut)", err)
	}
	shutdownCancel()
}
