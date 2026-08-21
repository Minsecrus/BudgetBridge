package pool

import (
	"context"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type AccountConfig struct {
	ID       int    `yaml:"id"        json:"id"`
	Alias    string `yaml:"alias"     json:"alias"`
	APIKey   string `yaml:"api_key"   json:"api_key"`
	AKId     string `yaml:"ak_id"     json:"ak_id"`
	AKSecret string `yaml:"ak_secret" json:"ak_secret"`
	WSDomain string `yaml:"ws_domain,omitempty" json:"ws_domain,omitempty"`
	// Enabled is a pointer so a config.yaml written before this field existed
	// (no "enabled:" key) decodes to nil → defaults to enabled. A plain bool
	// would decode absence to false and disable every account on the first
	// restart after this change shipped — a deployment-breaking regression.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// EnabledOrDefault returns the resolved enabled state, defaulting to true
// when the config did not specify an explicit value (nil pointer).
func (c AccountConfig) EnabledOrDefault() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

type Account struct {
	AccountConfig
	mu      sync.RWMutex
	Enabled bool
	// userDisabled marks accounts the operator turned off via ToggleByID (or
	// that loaded from config already disabled). While set, SetBalance must not
	// touch Enabled — otherwise the monitor's 5-min healthy-balance tick
	// re-enables an account the user took out of rotation and the disable
	// reverts on the next poll. Cleared on re-toggle.
	userDisabled  bool
	Balance       float64
	CouponCount   int
	LastChecked   time.Time
	CooldownUntil time.Time
	// modelCooldowns maps effective-model → 429 cooldown expiry (UnixNano). 429 是
	// per-(account,model) 限流:只有命中的模型被冷却,该号其他模型照常服务。与
	// freeUsed 同按 effective model 作 key。惰性过期(janitor + 查询时清);与 mu
	// 分开锁,避免 Pick 热路径不必要的竞争。
	modelCooldowns   map[string]int64
	modelCooldownsMu sync.Mutex
	RequestCount     int64
	// forbiddenStreak counts consecutive upstream 403s on this account. A 403
	// from dashscope is almost always an account-billing/authorization problem
	// (voucher coupon revoked, AK disabled, arrears) — distinct from 429 (per-
	// key QPS). Two consecutive 403s auto-disable the account so the retry
	// loop stops routing live traffic to a credentialed-but-billed-out account
	// and burning a retry slot per request. It is NOT auto-re-enabled: unlike
	// a balance dip, a 403 doesn't self-heal, so the operator must fix the
	// billing and re-enable explicitly. Reset to 0 on a 200 (RecordSuccess) so
	// a one-off 403 amid healthy traffic doesn't trip the disable. Lives under
	// mu, written only on 403/200, never on the hot Pick path.
	forbiddenStreak int
	// freeUsed tracks consumed free-quota tokens per model (local accounting;
	// lost on restart — see plan). Keyed by the effective (post-model_override)
	// model string. Used by cold routing to prefer accounts that still have a
	// model's free quota untouched — e.g. an account bled dry on glm-5.2 but
	// with deepseek-v4-pro's quota pristine.
	freeUsed map[string]int64
	freeMu   sync.Mutex
}

func (a *Account) IsAvailable() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Enabled && time.Now().After(a.CooldownUntil)
}

// IsAvailableFor reports whether the account can serve the given effective
// model: whole-account availability (Enabled + 403 CooldownUntil) AND that this
// specific model isn't under a per-(account,model) 429 cooldown. Expired per-
// model entries are lazily dropped here.
func (a *Account) IsAvailableFor(model string) bool {
	if !a.IsAvailable() {
		return false
	}
	now := time.Now().UnixNano()
	a.modelCooldownsMu.Lock()
	defer a.modelCooldownsMu.Unlock()
	if exp, ok := a.modelCooldowns[model]; ok {
		if exp <= now {
			delete(a.modelCooldowns, model)
		} else {
			return false
		}
	}
	return true
}

// SetBalance updates the cached balance and auto-disables the account when it
// drops below the drain floor. Safe to call from monitor goroutines.
//
// Auto-disable is memory-only and self-heals: the next monitor tick re-enables
// the account if its balance has recovered (e.g. a voucher top-up, or the prior
// reading was a transient API hiccup reporting a too-low value). Persisting
// here would write config.yaml every 5 min per account; the 5-min restart
// window is acceptable existing behavior. Re-enabling here — rather than only
// at restart — means a recovered account re-enters rotation immediately instead
// of sitting disabled until the process restarts, which previously caused
// success-rate dips when an account's balance briefly dipped below drainFloor.
// The self-heal only applies to accounts the operator left in rotation: a
// manually disabled account (userDisabled) is never re-enabled here.
func (a *Account) SetBalance(balance float64, couponCount int) {
	a.mu.Lock()
	a.Balance = balance
	a.CouponCount = couponCount
	a.LastChecked = time.Now()
	// A manually disabled account (user toggle, or loaded disabled from config)
	// is left alone — only balance-drive the enable/disable for accounts the
	// operator hasn't taken out of rotation. Otherwise every 5-min healthy tick
	// would re-enable a disabled account and the disable would revert.
	if !a.userDisabled {
		if balance < drainFloor {
			a.Enabled = false
		} else {
			a.Enabled = true
		}
	}
	a.mu.Unlock()
}

// DisplayAlias returns the alias if set, otherwise a masked form of the API
// key (first 3 + *** + last 4, e.g. "sk-***Tizg") so unnamed accounts are
// recognizable and distinguishable. The stored alias stays empty — display only.
func (a *Account) DisplayAlias() string {
	if a.Alias != "" {
		return a.Alias
	}
	k := a.APIKey
	if len(k) >= 8 {
		return k[:3] + "***" + k[len(k)-4:]
	}
	return k
}

// Weight returns the scheduling weight in [minWeight, 1.0], bucketed by
// balance tier. Weighting is bucketed (not balance/300 continuous) so the
// argmax over a fixed key only changes when an account crosses a tier
// boundary — within a tier the session map keeps affinity absolute and
// ongoing conversations don't flap as the monitor refreshes balances.
// The bottom tier returns minWeight so a low-balance account still receives
// SOME new-key traffic (drain) instead of being starved.
func (a *Account) Weight() float64 {
	a.mu.RLock()
	bal := a.Balance
	a.mu.RUnlock()
	if bal < 0 {
		bal = 0
	}
	for _, t := range weightTiers {
		if bal >= t.floor {
			return t.weight
		}
	}
	return minWeight
}

// freeRemaining returns the unconsumed free-quota tokens for model, floored at
// 0. quota is the per-model free allowance (Pool.freeQuotaPerModel). Local
// accounting: a restart zeroes freeUsed so every model briefly reads as "full"
// — safe under G2 (routing only onto alive accounts): the worst case is "didn't
// save", never "overdrew". Returns 0 when quota <= 0 (disabled) or model == "".
func (a *Account) freeRemaining(quota int64, model string) int64 {
	if quota <= 0 || model == "" {
		return 0
	}
	a.freeMu.Lock()
	defer a.freeMu.Unlock()
	used := a.freeUsed[model]
	if used >= quota {
		return 0
	}
	return quota - used
}

// ConsumeFree records tokens used for model against its free quota. Called by
// the proxy after a successful request, on the account that ultimately served
// it. Never blocks; allocates the map lazily. No-op for empty model or
// non-positive tokens.
func (a *Account) ConsumeFree(model string, tokens int) {
	if tokens <= 0 || model == "" {
		return
	}
	a.freeMu.Lock()
	if a.freeUsed == nil {
		a.freeUsed = map[string]int64{}
	}
	a.freeUsed[model] += int64(tokens)
	a.freeMu.Unlock()
}

// drainFloor is the balance below which an account is considered exhausted:
// SetBalance auto-disables the account here (it cannot serve useful traffic),
// distinct from lowFloor (the session-migration threshold). Kept as a named
// const rather than a magic 3.0 in SetBalance.
const drainFloor = 3.0

// minWeight is the floor weight for any enabled, non-cooldown account. It
// guarantees a low-balance account still receives some traffic so its
// remaining balance drains toward the drainFloor auto-disable in SetBalance
// rather than being starved.
const minWeight float64 = 0.05

// weightTiers buckets balances into ~50-yuan tiers. Two design goals:
//   - Within a tier, all accounts share the same weight, so weighted Rendezvous
//     spreads NEW conversations ~evenly across them ("档内基本均匀分配"). Balance
//     wobbles inside a 50-yuan band never change the argmax, so ongoing
//     sessions stay pinned.
//   - Across tiers the gradient is deliberately gentle (adjacent tiers ~1.1-1.2x)
//     so traffic is NOT over-concentrated on the highest-balance account — a
//     single hot account would otherwise pile up concurrency and hit 429. The
//     proxy's existing 429→Cooldown(60s)→retry path still handles the 429s that
//     do occur; this just keeps them rare by not funneling everything onto one
//     account. E.g. 300 vs 50 → 1.0/(1.0+0.46) ≈ 69/31, not 87/13.
//   - The bottom (<20) tier collapses to minWeight so a near-depleted account
//     still drains (some new-key traffic) rather than being starved, while
//     mapped sessions migrate off it (see Pick).
var weightTiers = []struct {
	floor  float64
	weight float64
}{
	{300, 1.00},
	{250, 0.92},
	{200, 0.84},
	{150, 0.74},
	{100, 0.62},
	{50, 0.46},
	{20, 0.30},
	{0, minWeight},
}

// sessionEntry records the account a conversation was pinned to and the last
// time it was used. Entries past sessTTL are swept by the janitor.
type sessionEntry struct {
	accID int
	last  time.Time
}

type Pool struct {
	mu        sync.RWMutex
	accounts  []*Account
	cancels   []context.CancelFunc // aligned with accounts; stops each monitor goroutine
	scheduler string               // "affinity" | "round_robin"
	counter   atomic.Int64
	nextID    int // monotonically increasing stable ID; never reused on remove

	// sessions maps an affinity key (first user message text) to the account
	// it was pinned to. This is what makes affinity rank strictly above
	// weighting: as long as the mapped account is available and above
	// lowFloor, it is reused unconditionally regardless of how other
	// accounts' weights have shifted. Lock ordering is always
	// sessMu → mu (Pick) or sessMu alone (janitor); mu is never held while
	// acquiring sessMu, so there is no lock-ordering cycle.
	sessions        map[string]*sessionEntry
	sessMu          sync.RWMutex
	sessJanitorOnce sync.Once
	lowFloor        float64 // config: LowBalanceFloor, default 20
	sessTTL         time.Duration

	// cold-drain routing: fresh, low-payload conversations (no assistant turn,
	// body under coldReqBytes) are pinned to low-balance accounts to drain them
	// rather than weighted toward high-balance ones. Drains reuse a low-balance
	// account's remaining balance faster instead of letting it sit starved at
	// minWeight. Warm (multi-turn) conversations still get the existing
	// high-balance HRW bias so their prefix cache lands on healthy accounts.
	drainEnabled bool
	coldReqBytes int
	// freeQuotaPerModel is the per-(account,model) free-token allowance. When
	// > 0, cold routing prefers accounts that still have THIS model's free
	// quota (coldScore), recovering value from low-balance accounts whose other
	// models are bled dry. 0 disables awareness and cold falls back to the
	// original balance-drain score (backward compatible).
	freeQuotaPerModel int64
}

func New(cfgs []AccountConfig, scheduler string, lowBalanceFloor float64, coldDrain bool, coldReqBytes int, freeQuotaPerModel int64) *Pool {
	if scheduler == "" {
		scheduler = "affinity"
	}
	if lowBalanceFloor <= 0 {
		lowBalanceFloor = 20.0
	}
	if coldReqBytes <= 0 {
		coldReqBytes = 8192
	}
	p := &Pool{
		scheduler:         scheduler,
		lowFloor:          lowBalanceFloor,
		drainEnabled:      coldDrain,
		coldReqBytes:      coldReqBytes,
		freeQuotaPerModel: freeQuotaPerModel,
		sessions:          make(map[string]*sessionEntry),
		sessTTL:           time.Hour,
	}
	maxID := 0
	for i := range cfgs {
		c := cfgs[i]
		if c.ID <= 0 {
			// Legacy account without a stable ID: assign one. The caller should
			// persist p.Configs() right after New so the assigned IDs survive a
			// restart (otherwise they'd shift and stats would re-mismatch).
			maxID++
			c.ID = maxID
		}
		if c.ID > maxID {
			maxID = c.ID
		}
		p.accounts = append(p.accounts, &Account{AccountConfig: c, Enabled: c.EnabledOrDefault(), userDisabled: !c.EnabledOrDefault()})
		p.cancels = append(p.cancels, nil)
	}
	p.nextID = maxID + 1
	return p
}

// NextID returns the next stable account ID that will be assigned by Add.
// Used by the config saver to persist the monotonic counter.
func (p *Pool) NextID() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.nextID
}

// SeedNextID raises nextID to n if n is greater than the current value. Used at
// startup to restore the counter from config.yaml so a cleared-then-restarted
// deployment doesn't reuse IDs that still have stale stats rows in the 7-day
// retention window. No-op when n == 0 (legacy config without next_account_id).
func (p *Pool) SeedNextID(n int) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if n > p.nextID {
		p.nextID = n
	}
}

func hashKey(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

// nodeID returns the stable per-account identifier used as part of the HRW
// hash input. It is the alias if set, else the API key (truncated to 12 chars
// to match the legacy ring base-key), suffixed with "#" + ID so duplicate
// aliases or same-prefix API keys don't collide on identical hashes.
func nodeID(a *Account) string {
	base := a.Alias
	if base == "" {
		if len(a.APIKey) > 12 {
			base = a.APIKey[:12]
		} else {
			base = a.APIKey
		}
	}
	return base + "#" + strconv.Itoa(a.ID)
}

func (p *Pool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.accounts)
}

func (p *Pool) Get(idx int) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if idx < 0 || idx >= len(p.accounts) {
		return nil
	}
	return p.accounts[idx]
}

// ByID returns the account with the given stable ID, or nil if not found. CRUD
// routes use this instead of positional Get(i) so removing an account never
// reroutes operations to a different one.
func (p *Pool) ByID(id int) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// IndexByID returns the positional index of the account with the given ID, or
// -1 if not present. Used internally where a positional index is still needed
// (SetCancel alignment, slice removal).
func (p *Pool) IndexByID(id int) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i, a := range p.accounts {
		if a.ID == id {
			return i
		}
	}
	return -1
}

// Next returns the next available account by round-robin (no affinity). Accounts
// whose IDs appear in exclude are skipped — used by the proxy retry loop so a
// transport error on one account doesn't loop back onto the same account.
func (p *Pool) Next(model string, exclude map[int]bool) *Account {
	p.mu.RLock()
	n := len(p.accounts)
	if n == 0 {
		p.mu.RUnlock()
		return nil
	}
	start := int(p.counter.Add(1)-1) % n
	var chosen *Account
	for i := 0; i < n; i++ {
		acc := p.accounts[(start+i)%n]
		if !exclude[acc.ID] && acc.IsAvailableFor(model) {
			atomic.AddInt64(&acc.RequestCount, 1)
			chosen = acc
			break
		}
	}
	p.mu.RUnlock()
	return chosen
}

// pickScore returns the per-request scheduling score for an account in
// HRW: its bucketed balance Weight(). (An earlier version down-weighted
// accounts under cooldown, but cooled accounts are excluded by IsAvailable*
// before they ever reach scoring, so that branch was unreachable.)
func (a *Account) pickScore() float64 {
	return a.Weight()
}

// hrwPick selects an account for the given affinity key using weighted
// Rendezvous hashing: score_k = W_k / -ln(h_k) where h_k is a per-(key,
// account) uniform hash in (0,1), and the highest score wins. Accounts in
// exclude or not IsAvailable are skipped. An account under active 429
// cooldown is down-weighted (see pickScore) but not excluded, so new keys
// avoid it while ongoing sessions pinned to it via the session map can still
// land there once it recovers. Returns nil if no candidate qualifies.
// Caller must NOT hold p.mu.
func (p *Pool) hrwPick(key, model string, exclude map[int]bool) *Account {
	return p.rhwPickWithScore(key, model, exclude, (*Account).pickScore)
}

// rhwPickWithScore is the shared Rendezvous-hashing skeleton for hrwPick and
// drainPick: identical exclude/availability filtering and hash clamping, only
// the per-account score function differs. Factored out so drain routing can
// reuse the proven hash/clamp/argmax logic without duplicating it.
// Caller must NOT hold p.mu.
func (p *Pool) rhwPickWithScore(key, model string, exclude map[int]bool, score func(*Account) float64) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var best *Account
	var bestScore float64 = -1
	for _, acc := range p.accounts {
		if exclude[acc.ID] || !acc.IsAvailableFor(model) {
			continue
		}
		// h in (0,1); clamp away from 0 and 1 to avoid -ln(0)=+Inf and
		// -ln(1)=0 (division by zero).
		h := float64(hashKey(key+"#"+nodeID(acc))) / float64(1<<32)
		if h < 1e-12 {
			h = 1e-12
		} else if h > 1-1e-12 {
			h = 1 - 1e-12
		}
		s := score(acc) / -math.Log(h)
		if s > bestScore {
			bestScore = s
			best = acc
		}
	}
	return best
}

// coldScore is the model-aware cold-routing score. Accounts that still have
// free quota for THIS model rank strictly above those that don't (score in
// (1.0, 2.0] vs <= 1.0): cold requests preferentially burn a model's free quota
// on accounts that still have it — most valuable for low-balance accounts that
// burned through OTHER models but have THIS model's quota untouched. Among
// quota-bearing accounts, more remaining ranks higher (drain the richest
// first). Accounts with no quota for this model fall back to drainScore (burn
// their balance to retire them). Returns plain drainScore when awareness is
// off (freeQuotaPerModel <= 0), preserving pre-feature behavior. Warm requests
// never call this.
func (p *Pool) coldScore(acc *Account, model string) float64 {
	if p.freeQuotaPerModel <= 0 {
		return acc.drainScore()
	}
	if rem := acc.freeRemaining(p.freeQuotaPerModel, model); rem > 0 {
		return 1.0 + float64(rem)/float64(p.freeQuotaPerModel) // (1.0, 2.0]
	}
	return acc.drainScore() // (0.0, 1.0]
}

// drainScore is the cold-request score: score_k = (1.0 - W_k + minWeight) /
// -ln(h_k). Because W_k is HIGHER for high-balance accounts, (1 - W_k) is
// LOWER for them, so low-balance accounts win the argmax — fresh cold
// conversations drain the low-balance accounts rather than piling onto healthy
// ones. minWeight keeps the floor from going negative; the bottom tier
// (W=minWeight) thus maps to the maximum drain score ~1.0, the top tier (W=1.0)
// to ~minWeight. A cold request has no prefix cache to lose by landing on a
// near-empty account, so draining it here is strictly better than letting the
// balance sit starved at minWeight under the warm-biased hrwPick.
func (a *Account) drainScore() float64 {
	return 1.0 - a.Weight() + minWeight
}

// drainPick selects an account for a fresh cold key, model-aware. When
// free-quota awareness is on it uses coldScore (preferring accounts with this
// model's free quota); otherwise the original balance-inverted drainScore.
// Accounts in exclude or not IsAvailable are skipped. Returns nil if no
// candidate qualifies. Caller must NOT hold p.mu.
func (p *Pool) drainPick(key, model string, exclude map[int]bool) *Account {
	return p.rhwPickWithScore(key, model, exclude, func(acc *Account) float64 {
		return p.coldScore(acc, model)
	})
}

// lookupSession returns the mapped account ID for key and whether the mapping
// is present and unexpired. An expired entry is treated as absent (the janitor
// will reclaim it later).
func (p *Pool) lookupSession(key string) (int, bool) {
	p.sessMu.RLock()
	defer p.sessMu.RUnlock()
	e, ok := p.sessions[key]
	if !ok {
		return 0, false
	}
	if time.Since(e.last) > p.sessTTL {
		return 0, false
	}
	return e.accID, true
}

// recordSession stores (or overwrites) the key → accID mapping and updates
// last-seen to now. Launches the janitor goroutine on first call.
func (p *Pool) recordSession(key string, accID int) {
	p.sessMu.Lock()
	if e, ok := p.sessions[key]; ok {
		e.accID = accID
		e.last = time.Now()
		p.sessMu.Unlock()
		return
	}
	p.sessions[key] = &sessionEntry{accID: accID, last: time.Now()}
	p.sessMu.Unlock()
	p.sessJanitorOnce.Do(p.startSessionJanitor)
}

// touchSession updates last-seen for an existing mapping without changing the
// mapped account. No-op if the key is absent.
func (p *Pool) touchSession(key string) {
	p.sessMu.Lock()
	if e, ok := p.sessions[key]; ok {
		e.last = time.Now()
	}
	p.sessMu.Unlock()
}

// startSessionJanitor launches a background goroutine that prunes expired
// session entries every 10 minutes and enforces a hard size cap so a client
// hammering unique first-user-texts can't grow the map unboundedly.
func (p *Pool) startSessionJanitor() {
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			p.sweepSessions()
			p.sweepModelCooldowns()
		}
	}()
}

// sweepModelCooldowns drops expired per-model 429 cooldown entries across all
// accounts. Lazy expiry in IsAvailableFor already keeps reads correct; this
// just prevents stale keys from accumulating. Caller: janitor goroutine.
func (p *Pool) sweepModelCooldowns() {
	now := time.Now().UnixNano()
	p.mu.RLock()
	accs := make([]*Account, len(p.accounts))
	copy(accs, p.accounts)
	p.mu.RUnlock()
	for _, acc := range accs {
		acc.modelCooldownsMu.Lock()
		for m, exp := range acc.modelCooldowns {
			if exp <= now {
				delete(acc.modelCooldowns, m)
			}
		}
		acc.modelCooldownsMu.Unlock()
	}
}

var sessionCap = 100_000

// sweepSessions drops expired entries and, if the map exceeds sessionCap,
// evicts the oldest 10% by last-seen. Caller: janitor goroutine only.
func (p *Pool) sweepSessions() {
	now := time.Now()
	p.sessMu.Lock()
	defer p.sessMu.Unlock()
	for k, e := range p.sessions {
		if now.Sub(e.last) > p.sessTTL {
			delete(p.sessions, k)
		}
	}
	if len(p.sessions) <= sessionCap {
		return
	}
	// Over cap — evict the oldest 10% by last-seen. O(n log n) via stdlib
	// sort; the map is at most ~110k here, acceptable every 10 min. (An earlier
	// hand-rolled selection sort was O(n²) and held sessMu the whole time.)
	evict := sessionCap / 10
	if evict < 1 {
		evict = 1
	}
	type kv struct {
		k string
		e *sessionEntry
	}
	all := make([]kv, 0, len(p.sessions))
	for k, e := range p.sessions {
		all = append(all, kv{k, e})
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].e.last.Before(all[j].e.last)
	})
	for i := 0; i < evict && i < len(all); i++ {
		delete(p.sessions, all[i].k)
	}
}

// Pick is the unified account selector. Affinity mode maps the key to a stable
// account via the session map first (so the same conversation reuses one
// account and hits its prefix cache); only if the mapped account is excluded,
// unavailable, or below the low-balance migration floor does it fall through
// to weighted Rendezvous (which prefers high-balance accounts for new keys).
// round_robin mode, an empty key, or all-accounts-down falls back to plain
// round-robin.
//
// cold marks a fresh, low-payload request (no assistant turn, small body) with
// no prefix-cache value. When cold-drain is enabled, fresh cold keys are routed
// to LOW-balance accounts (drainPick) so their remaining balance drains
// instead of sitting starved; warm (multi-turn) keys keep the existing
// high-balance HRW bias so their cache lands on healthy accounts. Cold picks
// do NOT record a session mapping — only warm picks pin — so a conversation
// that started cold hands off to a high-balance account the moment it turns
// warm, rather than being stuck on its cold drain target. (A cold request that
// happens to find an existing mapping — e.g. a key collision with a warm
// session — still reuses it per branch 1 below.)
//
// exclude is a set of account IDs to skip this round. The proxy retry loop
// passes the accounts already tried so a transport error (no Cooldown) still
// advances to the next account instead of re-picking the same one. It is scoped
// to a single request and never mutates shared pool state, unlike Cooldown.
//
// Migration discipline, now temperature-aware:
//   - A cold request whose mapped account is below lowFloor (still available)
//     reuses it (keeps draining; cold has no cache to lose) and does NOT
//     migrate. This only arises from a key collision with a warm session —
//     cold picks don't create mappings of their own (see branch 2). A WARM
//     request whose mapped account has dipped below lowFloor migrates to a
//     healthy account and RE-PINS — the conversation has grown into a long
//     session and should run its cache on a high-balance account; the small
//     cold cache built in round 1 is cheap to abandon.
//   - A mapped account excluded / disabled at drainFloor / removed is a
//     PERMANENT condition: re-pin to the HRW/drain-chosen fresh account.
func (p *Pool) Pick(key string, cold bool, model string, exclude ...int) *Account {
	ex := make(map[int]bool, len(exclude))
	for _, id := range exclude {
		ex[id] = true
	}
	if p.scheduler != "affinity" || key == "" {
		return p.Next(model, ex)
	}

	// cold routing only kicks in for cold-drain-eligible pools; otherwise cold
	// behaves like warm (high-balance HRW bias), preserving the prior default.
	drain := cold && p.drainEnabled

	// 1. Session-map lookup — affinity ranks strictly above weighting.
	mappedID, mappedOK := p.lookupSession(key)
	if mappedOK {
		acc := p.ByID(mappedID)
		if acc != nil && !ex[acc.ID] && acc.IsAvailableFor(model) {
			acc.mu.RLock()
			bal := acc.Balance
			acc.mu.RUnlock()
			if bal >= p.lowFloor {
				// Mapped account is healthy → reuse unconditionally.
				p.touchSession(key)
				atomic.AddInt64(&acc.RequestCount, 1)
				return acc
			}
			// Below the floor but still enabled/available.
			if drain {
				// Cold request: keep draining the mapped low-balance account —
				// it has no prefix cache worth preserving, and leaving it pinned
				// here burns down its balance instead of starving it.
				p.touchSession(key)
				atomic.AddInt64(&acc.RequestCount, 1)
				return acc
			}
			// Warm request: a low-balance account shouldn't run a long
			// conversation's cache. Fall through to the picker, which migrates
			// to a healthier account and RE-PINS (unlike the old transient-dip
			// behavior, which kept the session pinned to the dipping account).
			// The conversation has grown past cold; its cache belongs on a
			// high-balance account, and the small cold cache built so far is
			// cheap to abandon.
		}
		// else: mapped account excluded / disabled / removed → permanent; the
		// picker below will re-pin to a fresh healthy account.
	}

	// 2. Weighted Rendezvous over the non-excluded, available accounts.
	//    Cold+drain uses the model-aware drain score (prefers accounts with
	//    THIS model's free quota); warm uses the high-balance pick score. Only
	//    WARM picks pin the session: cold has no prefix cache worth preserving,
	//    and leaving it unpinned lets it hand off to a high-balance account the
	//    moment it turns warm, instead of draining on the low-balance account
	//    it started on.
	if drain {
		if chosen := p.drainPick(key, model, ex); chosen != nil {
			atomic.AddInt64(&chosen.RequestCount, 1)
			return chosen // cold never pins
		}
		return p.Next(model, ex)
	}
	if chosen := p.hrwPick(key, model, ex); chosen != nil {
		p.recordSession(key, chosen.ID)
		atomic.AddInt64(&chosen.RequestCount, 1)
		return chosen
	}

	// 3. All excluded or unavailable → fall back to round-robin.
	return p.Next(model, ex)
}

// ColdRequestBytes exposes the configured cold-request byte guard so the proxy
// can classify a request as cold (small body, no assistant turn) without the
// pool exporting its internals.
func (p *Pool) ColdRequestBytes() int { return p.coldReqBytes }

func (p *Pool) Cooldown(acc *Account, d time.Duration) {
	acc.mu.Lock()
	acc.CooldownUntil = time.Now().Add(d)
	acc.mu.Unlock()
}

// CooldownModel parks the account for model under a 429 cooldown of duration d.
// 429 是 per-(account,model) 限流:只挡该号该模型,其他模型不受影响(不误伤)。
// model 为空则 no-op(防御)。与 freeUsed 同按 effective model 作 key。
func (p *Pool) CooldownModel(acc *Account, model string, d time.Duration) {
	if model == "" {
		return
	}
	exp := time.Now().Add(d).UnixNano()
	acc.modelCooldownsMu.Lock()
	if acc.modelCooldowns == nil {
		acc.modelCooldowns = make(map[string]int64)
	}
	acc.modelCooldowns[model] = exp
	acc.modelCooldownsMu.Unlock()
}

// RecordSuccess clears a forbiddenStreak on a 200 so a one-off 403 amid
// healthy traffic doesn't accumulate toward an auto-disable. (429 cooldowns
// are a fixed 60s and simply expire; there's no escalation to relax.)
func (p *Pool) RecordSuccess(acc *Account) {
	acc.mu.Lock()
	acc.forbiddenStreak = 0
	acc.mu.Unlock()
}

// forbiddenThreshold is the consecutive-403 count at which an account is
// auto-disabled. 2 strikes: a single transient 403 (e.g. a momentary upstream
// authorization glitch) only excludes the account for the rest of THAT
// request; only a sustained run of 403s — the signature of a billing/credential
// problem — takes the account out of rotation entirely.
const forbiddenThreshold = 2

// RecordForbidden accounts for one upstream 403 on acc. It bumps the
// consecutive-403 streak and, once it reaches forbiddenThreshold, disables the
// account (Enabled=false) so IsAvailable() returns false and the retry loop /
// scheduler stop routing live traffic to a credentialed-but-billed-out account.
// A 403 from dashscope is an authorization/billing failure (voucher coupon
// revoked, AK disabled, arrears) — unlike a balance dip it does not self-heal,
// so the account is left disabled until the operator fixes the billing and
// re-enables it via the toggle endpoint. Returns the streak after the bump and
// whether the account was auto-disabled by this call.
//
// Caller should also Cooldown the account so, even if a concurrent picker
// already grabbed it this instant, the in-flight attempt is unlikely to land;
// disabling alone doesn't preempt an already-returned *Account.
func (p *Pool) RecordForbidden(acc *Account) (streak int, disabled bool) {
	acc.mu.Lock()
	acc.forbiddenStreak++
	streak = acc.forbiddenStreak
	if streak >= forbiddenThreshold && acc.Enabled {
		acc.Enabled = false
		disabled = true
	} else {
		disabled = false
	}
	acc.mu.Unlock()
	return streak, disabled
}

// DisabledByForbidden reports whether acc was last taken out of rotation by the
// 403 auto-disable (vs. a manual toggle or a balance-driven SetBalance). Used
// by the admin handler so the UI can surface "billing problem — re-enable after
// fixing the account" instead of a bare disabled flag. True iff Enabled is
// false AND the streak is at/above threshold (a streak that was reset by a 200
// means the disable predates any recent 403, so it was a manual/balance one).
func (p *Pool) DisabledByForbidden(acc *Account) bool {
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	return !acc.Enabled && acc.forbiddenStreak >= forbiddenThreshold
}

// ToggleByID flips the enabled flag of the account with the given ID.
func (p *Pool) ToggleByID(id int) bool {
	acc := p.ByID(id)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.Enabled = !acc.Enabled
	acc.userDisabled = !acc.Enabled // sticky: monitor's SetBalance won't re-enable it
	// Re-enabling an account that was auto-disabled by 403s also clears the
	// streak (see Toggle). Re-disabling via toggle does NOT touch the streak —
	// a manual disable is its own thing and shouldn't masquerade as a 403 one.
	if acc.Enabled {
		acc.forbiddenStreak = 0
	}
	acc.mu.Unlock()
	return true
}

func (p *Pool) ClearCooldown(idx int) bool {
	acc := p.Get(idx)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.CooldownUntil = time.Time{}
	acc.mu.Unlock()
	return true
}

// ClearCooldownByID clears the 429 cooldown of the account with the given ID.
func (p *Pool) ClearCooldownByID(id int) bool {
	acc := p.ByID(id)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.CooldownUntil = time.Time{}
	acc.mu.Unlock()
	// 运维"解除冷却"恢复整号所有模型:连 per-model 429 冷却一起清。
	acc.modelCooldownsMu.Lock()
	acc.modelCooldowns = nil
	acc.modelCooldownsMu.Unlock()
	return true
}

// Add appends a new account, assigns it a fresh stable ID, and returns that ID.
// The caller must start the account's monitor goroutine and register its cancel
// func via SetCancelByID so Remove/Clear can stop it.
func (p *Pool) Add(cfg AccountConfig) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg.ID = p.nextID
	p.nextID++
	p.accounts = append(p.accounts, &Account{AccountConfig: cfg, Enabled: cfg.EnabledOrDefault(), userDisabled: !cfg.EnabledOrDefault()})
	p.cancels = append(p.cancels, nil)
	return cfg.ID
}

// SetCancel associates a cancel func with account idx, called after starting
// the account's monitor goroutine.
func (p *Pool) SetCancel(idx int, cancel context.CancelFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx < 0 || idx >= len(p.cancels) {
		return
	}
	p.cancels[idx] = cancel
}

// SetCancelByID associates a cancel func with the account having the given ID.
func (p *Pool) SetCancelByID(id int, cancel context.CancelFunc) {
	p.SetCancel(p.IndexByID(id), cancel)
}

func (p *Pool) Remove(idx int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx < 0 || idx >= len(p.accounts) {
		return false
	}
	if cancel := p.cancels[idx]; cancel != nil {
		cancel()
	}
	p.accounts[idx].mu.Lock()
	p.accounts[idx].Enabled = false
	p.accounts[idx].mu.Unlock()
	p.accounts = append(p.accounts[:idx], p.accounts[idx+1:]...)
	p.cancels = append(p.cancels[:idx], p.cancels[idx+1:]...)
	return true
}

// RemoveByID removes the account with the given ID (ID is never reused).
// Returns false if no such account exists.
func (p *Pool) RemoveByID(id int) bool {
	return p.Remove(p.IndexByID(id))
}

func (p *Pool) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, cancel := range p.cancels {
		if cancel != nil {
			cancel()
		}
	}
	for _, acc := range p.accounts {
		acc.mu.Lock()
		acc.Enabled = false
		acc.mu.Unlock()
	}
	p.accounts = nil
	p.cancels = nil
}

// StopMonitors cancels every account's monitor context so background balance
// polling stops during graceful shutdown. It does not wait for an in-flight
// QueryCashCoupons call to return; each monitor goroutine exits on its next
// context.Done() check. Cancelling up-front means no new stats/balance events
// arrive while the stats store is draining its channel.
func (p *Pool) StopMonitors() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, cancel := range p.cancels {
		if cancel != nil {
			cancel()
		}
	}
}

func (p *Pool) Configs() []AccountConfig {
	p.mu.RLock()
	cfgs := make([]AccountConfig, len(p.accounts))
	for i, acc := range p.accounts {
		acc.mu.RLock()
		cfg := acc.AccountConfig
		// Copy the runtime Enabled into a fresh pointer so the persisted value
		// reflects the current on/off state (toggle / auto-disable). Copied by
		// value under the lock: returning &acc.Enabled directly would expose a
		// field that mutates after the lock releases (data race).
		enabled := acc.Enabled
		cfg.Enabled = &enabled
		cfgs[i] = cfg
		acc.mu.RUnlock()
	}
	p.mu.RUnlock()
	return cfgs
}

// TotalBalance returns the sum of enabled accounts' current balances. Used by
// the OpenAI billing endpoints to report the live aggregate balance to new-api
// (which reads it as the channel's remaining quota). Lock ordering matches
// All(): pool mu → account mu, so there is no cycle with other accessors.
func (p *Pool) TotalBalance() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var total float64
	for _, acc := range p.accounts {
		acc.mu.RLock()
		if acc.Enabled {
			total += acc.Balance
		}
		acc.mu.RUnlock()
	}
	return total
}

type Status struct {
	ID             int      `json:"id"`
	Index          int      `json:"-"`
	Alias          string   `json:"alias"`
	Enabled        bool     `json:"enabled"`
	Available      bool     `json:"available"`
	Balance        float64  `json:"balance"`
	CouponCount    int      `json:"coupon_count"`
	LastChecked    string   `json:"last_checked"`
	CooldownSecs   int      `json:"cooldown_secs"`
	CooldownModels []string `json:"cooldown_models,omitempty"`
	RequestCount   int64    `json:"request_count"`
}

func (p *Pool) All() []Status {
	p.mu.RLock()
	result := make([]Status, len(p.accounts))
	for i, acc := range p.accounts {
		acc.mu.RLock()
		enabled := acc.Enabled
		balance := acc.Balance
		coupon := acc.CouponCount
		lastChecked := acc.LastChecked
		cooldownUntil := acc.CooldownUntil // whole-account (403/balance)
		acc.mu.RUnlock()

		// cooldown_secs = earliest recovery across whole-account + any active
		// per-model 429 cooldowns; cooldown_models lists those models. Expired
		// per-model entries are lazily dropped here. Available stays whole-
		// account (All isn't model-specific), so a per-model 429 doesn't flip it.
		now := time.Now()
		earliest := cooldownUntil
		acc.modelCooldownsMu.Lock()
		models := make([]string, 0)
		for m, expNano := range acc.modelCooldowns {
			exp := time.Unix(0, expNano)
			if exp.After(now) {
				models = append(models, m)
				if earliest.IsZero() || exp.Before(earliest) {
					earliest = exp
				}
			} else {
				delete(acc.modelCooldowns, m)
			}
		}
		acc.modelCooldownsMu.Unlock()

		cooldown := 0
		if rem := time.Until(earliest); rem > 0 {
			cooldown = int(rem.Seconds())
		}
		lc := ""
		if !lastChecked.IsZero() {
			lc = lastChecked.Format(time.RFC3339)
		}
		result[i] = Status{
			ID:             acc.ID,
			Index:          i,
			Alias:          acc.DisplayAlias(),
			Enabled:        enabled,
			Available:      enabled && now.After(cooldownUntil),
			Balance:        balance,
			CouponCount:    coupon,
			LastChecked:    lc,
			CooldownSecs:   cooldown,
			CooldownModels: models,
			RequestCount:   atomic.LoadInt64(&acc.RequestCount),
		}
	}
	p.mu.RUnlock()
	return result
}
