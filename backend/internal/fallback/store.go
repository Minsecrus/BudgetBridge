// Package fallback holds the "fallback channel" registry: non-Alibaba
// OpenAI-compatible endpoints used as a last resort when the account pool is
// exhausted. Unlike pool accounts these have no queryable balance, take no part
// in weighted scheduling, and never enter pool.Pool — they are a separate kind
// of entity, served only after the pool's retry loop gives up, gated by a
// per-channel model whitelist.
package fallback

import (
	"sync"
	"time"
)

// Config is one fallback channel as persisted in config.yaml (and returned to
// admin CRUD that needs the key, e.g. forwarding/probing).
type Config struct {
	ID      int      `yaml:"id"        json:"id"`
	Name    string   `yaml:"name"      json:"name"`
	BaseURL string   `yaml:"base_url"  json:"base_url"` // OpenAI-compatible base incl. /v1; forward appends /chat/completions
	APIKey  string   `yaml:"api_key"   json:"api_key"`
	Models  []string `yaml:"models"    json:"models"` // whitelist; "*" = wildcard; empty = matches nothing
	// Enabled is a pointer so a config.yaml written before this field existed
	// decodes to nil → defaults to enabled, matching pool.AccountConfig. A plain
	// bool would decode absence to false and disable every channel on the first
	// restart after this shipped.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Runtime scheduling state (not persisted, not serialized). Mutated under
	// Store.mu. Mirrors the pool's per-account cooldown/forbidden-streak logic,
	// simplified: a fixed cooldown on 429 (so the next request skips a just-
	// throttled channel — 平滑切号) and a consecutive-failure streak that auto-
	// disables a persistently-failing channel (多次异常直接禁用).
	CooldownUntil time.Time `yaml:"-" json:"-"`
	FailStreak    int       `yaml:"-" json:"-"`
	// DisabledByErr marks a channel auto-disabled by the failure streak (vs a
	// manual toggle or balance-disable), so the UI can surface "fix it then
	// re-enable". Mirrors the pool's disabled_by_403 flag.
	DisabledByErr bool `yaml:"-" json:"-"`
}

// EnabledOrDefault returns the resolved enabled state, defaulting to true when
// the config did not specify an explicit value (nil pointer).
func (c Config) EnabledOrDefault() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// View is the API-safe projection of a channel for the admin list endpoint —
// APIKey is stripped so the dashboard never receives secrets, matching how
// pool.Status omits the account key.
type View struct {
	ID            int      `json:"id"`
	Name          string   `yaml:"-" json:"name"`
	BaseURL       string   `yaml:"-" json:"base_url"`
	Models        []string `yaml:"-" json:"models"`
	Enabled       bool     `yaml:"-" json:"enabled"`
	DisabledByErr bool     `yaml:"-" json:"disabled_by_err"`
}

// cooldownSecs is how long a 429'd channel is skipped by Pick — long enough
// that a rate-limited/quota-exhausted channel isn't re-tried every request
// (each retry wastes a full 429 RTT the user feels), short enough that it gets
// another chance if the limit was transient. Fixed rather than escalating
// (简易) — escalate if a flapping channel measurably hurts.
const cooldownSecs = 60

// failThreshold is the consecutive-failure count (429 / bad-cred / 5xx / net)
// at which a channel is auto-disabled. "多次异常直接禁用": a run of failures
// across requests means the channel is quota'd out or misconfigured, so stop
// burning retry slots on it until the operator re-enables.
const failThreshold = 5

// Store is a concurrency-safe registry of fallback channels. It is deliberately
// minimal: CRUD + a model-gated Pick. No balance, no cooldown, no weights —
// those concepts belong to the pool. A failed channel is tried once per request
// and not penalized across requests (YAGNI until a flapping channel hurts).
type Store struct {
	mu     sync.RWMutex
	items  []Config
	nextID int // monotonic in-memory counter; not persisted (no stats rows to collide with)
}

// New builds a store from config, assigning fresh IDs to any channel missing
// one (legacy/first-run) and seeding nextID above the max present ID.
func New(cfgs []Config) *Store {
	s := &Store{}
	maxID := 0
	for i := range cfgs {
		c := cfgs[i]
		if c.ID <= 0 {
			maxID++
			c.ID = maxID
		}
		if c.ID > maxID {
			maxID = c.ID
		}
		s.items = append(s.items, c)
	}
	s.nextID = maxID + 1
	return s
}

// Add appends a channel, assigns it a fresh ID, and returns that ID.
func (s *Store) Add(cfg Config) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg.ID = s.nextID
	s.nextID++
	s.items = append(s.items, cfg)
	return cfg.ID
}

// RemoveByID removes the channel with the given ID. Returns false if absent.
func (s *Store) RemoveByID(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.items {
		if c.ID == id {
			s.items = append(s.items[:i], s.items[i+1:]...)
			return true
		}
	}
	return false
}

// ToggleByID flips the enabled flag of the channel with the given ID. Re-
// enabling clears the auto-disable marker and runtime health state so the
// operator's "fixed it" toggle starts fresh (mirrors the pool's ToggleByID
// clearing forbiddenStreak). Re-disabling via toggle does NOT set DisabledByErr
// — a manual disable is its own thing.
func (s *Store) ToggleByID(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.items {
		if c.ID == id {
			on := !c.EnabledOrDefault()
			s.items[i].Enabled = &on
			if on {
				s.items[i].DisabledByErr = false
				s.items[i].FailStreak = 0
				s.items[i].CooldownUntil = time.Time{}
			}
			return true
		}
	}
	return false
}

// RecordSuccess clears a channel's failure streak after a 200 (one healthy
// request resets the "consecutive failures" count, so a channel that blips
// occasionally isn't disabled by scattered single failures).
func (s *Store) RecordSuccess(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].FailStreak = 0
			return
		}
	}
}

// RecordThrottle handles a 429: parks the channel for cooldownSecs (so the next
// request skips it instead of eating another 429 RTT — 平滑切号) AND bumps its
// failure streak (sustained 429s across requests eventually auto-disable a
// quota-exhausted channel). Returns the post-bump streak and whether this call
// crossed the disable threshold.
func (s *Store) RecordThrottle(id int) (streak int, disabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].CooldownUntil = time.Now().Add(cooldownSecs * time.Second)
			s.items[i].FailStreak++
			streak = s.items[i].FailStreak
			disabled = maybeDisable(&s.items[i], streak)
			return streak, disabled
		}
	}
	return 0, false
}

// RecordFailure bumps the consecutive-failure streak for a non-throttle channel
// fault (bad credentials / 5xx / transport error) and auto-disables at the
// threshold. Returns the post-bump streak and whether this call disabled.
func (s *Store) RecordFailure(id int) (streak int, disabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].FailStreak++
			streak = s.items[i].FailStreak
			disabled = maybeDisable(&s.items[i], streak)
			return streak, disabled
		}
	}
	return 0, false
}

// maybeDisable flips a channel off once its streak crosses failThreshold. Helper
// under s.mu; takes the in-store *Config to mutate. No-op if already disabled.
func maybeDisable(c *Config, streak int) bool {
	if streak >= failThreshold && c.EnabledOrDefault() {
		off := false
		c.Enabled = &off
		c.DisabledByErr = true
		return true
	}
	return false
}

// UpdateByID overwrites the editable fields of a channel (name, base_url,
// models). An empty apiKey means "leave the existing key unchanged" — the edit
// form never receives the secret back (View strips it), so a blank password
// field is the signal for "don't rotate the key". Enabled is NOT touched here
// (the toggle action owns it). Returns false if no such channel.
func (s *Store) UpdateByID(id int, name, baseURL, apiKey string, models []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.items {
		if c.ID == id {
			s.items[i].Name = name
			s.items[i].BaseURL = baseURL
			s.items[i].Models = models
			if apiKey != "" {
				s.items[i].APIKey = apiKey
			}
			return true
		}
	}
	return false
}

// All returns the API-safe projection of every channel (no APIKey), in config
// order, for the admin list endpoint.
func (s *Store) All() []View {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]View, 0, len(s.items))
	for _, c := range s.items {
		models := c.Models
		if models == nil {
			models = []string{}
		}
		out = append(out, View{
			ID: c.ID, Name: c.Name, BaseURL: c.BaseURL,
			Models: models, Enabled: c.EnabledOrDefault(),
			DisabledByErr: c.DisabledByErr,
		})
	}
	return out
}

// Configs returns the full channel configs (with APIKey and ID) for
// persistence to config.yaml.
func (s *Store) Configs() []Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Config, len(s.items))
	copy(out, s.items)
	return out
}

// ByID returns the full config (with APIKey) for the given ID, for forwarding
// or probing. The bool is false if no such channel exists.
func (s *Store) ByID(id int) (Config, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.items {
		if c.ID == id {
			return c, true
		}
	}
	return Config{}, false
}

// Pick returns the enabled channels eligible to serve the given model, in
// config order (the first to return 200 wins). A channel is eligible when its
// Models whitelist contains the model exactly, contains the "*" wildcard, or —
// only for an empty model — when the channel has no whitelist at all (a request
// whose model could not be parsed should not be silently dropped by an
// explicit-whitelist channel, but a wildcard channel may still catch it).
// An explicit non-empty whitelist with no match (and no "*") excludes the
// channel; an empty whitelist excludes it for any non-empty model.
func (s *Store) Pick(model string) []Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	var out []Config
	for _, c := range s.items {
		if !c.EnabledOrDefault() || now.Before(c.CooldownUntil) {
			continue
		}
		if matches(c.Models, model) {
			out = append(out, c)
		}
	}
	return out
}

// matches reports whether the whitelist allows the model. "*" wildcards to any
// model; an empty whitelist matches only an empty model (so a deliberately
// blank whitelist never serves real traffic until the operator lists models).
func matches(whitelist []string, model string) bool {
	for _, m := range whitelist {
		if m == "*" || m == model {
			return true
		}
	}
	return len(whitelist) == 0 && model == ""
}
