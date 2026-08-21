package pool

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestPool builds a 2-account affinity pool with cold-drain on, the default
// lowFloor (20), and the default cold-request byte guard (8KB). Centralized so
// the many Pick/New call sites don't each carry the full signature; warm (cold
// = false) behavior here matches the pre-cold-drain semantics exactly.
func newTestPool(cfgs []AccountConfig) *Pool {
	if len(cfgs) == 0 {
		cfgs = []AccountConfig{{Alias: "a", APIKey: "k1"}, {Alias: "b", APIKey: "k2"}}
	}
	return New(cfgs, "affinity", 20.0, true, 8192, 1000000)
}

// TestIsAvailableFor_PerModelIsolation: 429 冷却按 (account,model) 隔离 ——
// 冷却某模型后,该号该模型不可用、其他模型仍可用、整号 IsAvailable() 仍 true;
// 整号 Cooldown(403 路径) 则所有模型都不可用。模型名是任意字符串(这里用 modelA/B 占位)。
func TestIsAvailableFor_PerModelIsolation(t *testing.T) {
	p := newTestPool([]AccountConfig{{Alias: "a", APIKey: "k1"}})
	acc := p.Get(0)

	p.CooldownModel(acc, "modelA", 60*time.Second)
	if acc.IsAvailableFor("modelA") {
		t.Fatal("cooled model should be unavailable")
	}
	if !acc.IsAvailableFor("modelB") {
		t.Fatal("other model should still be available (per-model isolation)")
	}
	if !acc.IsAvailable() {
		t.Fatal("whole account should still be available (403/balance unaffected)")
	}

	// Whole-account cooldown (403 path) blocks every model.
	p.Cooldown(acc, 60*time.Second)
	if acc.IsAvailableFor("modelB") || acc.IsAvailable() {
		t.Fatal("whole-account cooldown should block all models")
	}
}

// TestIsAvailableFor_LazyExpiry: an expired per-model entry is treated as
// available and deleted on read. (modelA is just an arbitrary model string.)
func TestIsAvailableFor_LazyExpiry(t *testing.T) {
	p := newTestPool([]AccountConfig{{Alias: "a", APIKey: "k1"}})
	acc := p.Get(0)
	p.CooldownModel(acc, "modelA", -time.Second) // already expired
	if !acc.IsAvailableFor("modelA") {
		t.Fatal("expired per-model cooldown should be ignored")
	}
	acc.modelCooldownsMu.Lock()
	_, ok := acc.modelCooldowns["modelA"]
	acc.modelCooldownsMu.Unlock()
	if ok {
		t.Fatal("expired entry should be lazily deleted")
	}
}

// TestPick_PerModelCooldownDoesNotBlockOtherModels: 冷却 A 的 modelA 后,对
// modelA 的请求绝不落到 A;但对 modelB 的请求仍可路由到 A(per-model 隔离不误伤)。
func TestPick_PerModelCooldownDoesNotBlockOtherModels(t *testing.T) {
	p := newTestPool([]AccountConfig{
		{ID: 1, Alias: "A", APIKey: "k1"},
		{ID: 2, Alias: "B", APIKey: "k2"},
	})
	setBalance(p.ByID(1), 300)
	setBalance(p.ByID(2), 300)

	p.CooldownModel(p.ByID(1), "modelA", 60*time.Second)

	// Requests for the cooled model must never land on A.
	for i := 0; i < 200; i++ {
		acc := p.Pick(fmt.Sprintf("hot-%d", i), false, "modelA")
		if acc == nil || acc.ID == 1 {
			t.Fatalf("cooled model routed to A or nil: %v", acc)
		}
	}

	// Requests for a DIFFERENT model must still be able to use A.
	aHit := false
	for i := 0; i < 200; i++ {
		if acc := p.Pick(fmt.Sprintf("other-%d", i), false, "modelB"); acc != nil && acc.ID == 1 {
			aHit = true
			break
		}
	}
	if !aHit {
		t.Fatal("modelB should still be routable to A (per-model isolation)")
	}
}

// TestPick_AllAccountsCooledForModelReturnsNil: 当所有可用号对某模型都在冷却,
// Pick 对该模型返回 nil(上层走 fallback)。
func TestPick_AllAccountsCooledForModelReturnsNil(t *testing.T) {
	p := newTestPool([]AccountConfig{
		{ID: 1, Alias: "A", APIKey: "k1"},
		{ID: 2, Alias: "B", APIKey: "k2"},
	})
	p.CooldownModel(p.ByID(1), "modelA", 60*time.Second)
	p.CooldownModel(p.ByID(2), "modelA", 60*time.Second)
	if acc := p.Pick("anykey", false, "modelA"); acc != nil {
		t.Fatalf("expected nil when all accounts cooled for model, got id=%d", acc.ID)
	}
}

// TestClearCooldownByID_ClearsPerModel: 运维"解除冷却"应同时清掉该号所有
// per-model 429 冷却,而不只是整号 CooldownUntil。
func TestClearCooldownByID_ClearsPerModel(t *testing.T) {
	p := newTestPool([]AccountConfig{{ID: 1, Alias: "a", APIKey: "k1"}})
	acc := p.ByID(1)
	p.CooldownModel(acc, "modelA", 60*time.Second)
	if !p.ClearCooldownByID(1) {
		t.Fatal("ClearCooldownByID returned false")
	}
	if !acc.IsAvailableFor("modelA") {
		t.Fatal("ClearCooldownByID should clear per-model cooldowns too")
	}
}

// TestSweepModelCooldowns_DropsExpired: janitor 清理过期 per-model 冷却 key,
// 但保留仍在冷却期内的。
func TestSweepModelCooldowns_DropsExpired(t *testing.T) {
	p := newTestPool([]AccountConfig{{ID: 1, Alias: "a", APIKey: "k1"}})
	acc := p.ByID(1)
	p.CooldownModel(acc, "expired", -time.Second)
	p.CooldownModel(acc, "live", 60*time.Second)
	p.sweepModelCooldowns()
	acc.modelCooldownsMu.Lock()
	_, hasExpired := acc.modelCooldowns["expired"]
	_, hasLive := acc.modelCooldowns["live"]
	acc.modelCooldownsMu.Unlock()
	if hasExpired {
		t.Fatal("expired entry should be swept")
	}
	if !hasLive {
		t.Fatal("live entry should survive sweep")
	}
}

// TestAll_CooldownSecsEarliestAcrossModels: cooldown_secs 取整号与所有 per-model
// 冷却中最早者;cooldown_models 列出冷却中的模型;Available 仍是整号语义
// (per-model 冷却不把整号标记为不可用)。
func TestAll_CooldownSecsEarliestAcrossModels(t *testing.T) {
	p := newTestPool([]AccountConfig{{ID: 1, Alias: "a", APIKey: "k1"}})
	acc := p.ByID(1)
	p.CooldownModel(acc, "modelA", 60*time.Second)
	p.CooldownModel(acc, "modelB", 120*time.Second)

	var s Status
	for _, x := range p.All() {
		if x.ID == 1 {
			s = x
		}
	}
	if s.CooldownSecs < 50 || s.CooldownSecs > 70 {
		t.Fatalf("CooldownSecs should track earliest per-model (60s), got %d", s.CooldownSecs)
	}
	if len(s.CooldownModels) != 2 {
		t.Fatalf("CooldownModels should list 2 models, got %v", s.CooldownModels)
	}
	if !s.Available {
		t.Fatal("Available should stay true: per-model cooldowns don't disable whole account")
	}
}

// TestPickExcludesTriedAccounts verifies the #2 fix: in affinity mode a
// transport error must not loop the retry back onto the same account. Pick
// accepts an exclude set so the proxy retry loop can advance to the next
// account without Cooldown'ing (which would pollute global pool state).
func TestPickExcludesTriedAccounts(t *testing.T) {
	p := newTestPool([]AccountConfig{
		{Alias: "a1", APIKey: "k1"},
		{Alias: "a2", APIKey: "k2"},
	})

	key := "conversation-1"
	first := p.Pick(key, false, "m")
	if first == nil {
		t.Fatal("Pick returned nil with available accounts")
	}

	// Excluding the first pick must yield a different account, not loop back
	// onto the same one (the transport-error retry bug this fixes).
	second := p.Pick(key, false, "m", first.ID)
	if second == nil {
		t.Fatal("Pick returned nil when one account excluded but another is available")
	}
	if second.ID == first.ID {
		t.Fatalf("Pick returned same account %d despite exclusion", first.ID)
	}

	// Excluding both leaves nothing → nil (caller breaks and 503s).
	if got := p.Pick(key, false, "m", first.ID, second.ID); got != nil {
		t.Fatalf("Pick returned %v after excluding all accounts; want nil", got.ID)
	}
}

// TestPickRoundRobinExcludes mirrors the above for round_robin mode (empty key
// falls back to Next), confirming exclude works there too.
func TestPickRoundRobinExcludes(t *testing.T) {
	p := New([]AccountConfig{
		{Alias: "a1", APIKey: "k1"},
		{Alias: "a2", APIKey: "k2"},
	}, "round_robin", 20.0, true, 8192, 1000000)

	first := p.Pick("", false, "m")
	if first == nil {
		t.Fatal("Pick returned nil with available accounts")
	}
	second := p.Pick("", false, "m", first.ID)
	if second == nil {
		t.Fatal("Pick returned nil when one account excluded but another is available")
	}
	if second.ID == first.ID {
		t.Fatalf("Pick returned same account %d despite exclusion", first.ID)
	}
}

// setBalance mutates an account's cached balance without going through the
// monitor (tests only). Hold the account's write lock for the change.
func setBalance(acc *Account, bal float64) {
	acc.mu.Lock()
	acc.Balance = bal
	acc.mu.Unlock()
}

// TestHRW_DistributionFavorsHighBalance: a high-balance account should win the
// majority of fresh keys, but the low-balance account must still win some
// (drain). Two accounts at {300, 50} → weights 1.0 vs 0.46, so P(low) ≈ 32%.
func TestHRW_DistributionFavorsHighBalance(t *testing.T) {
	p := newTestPool([]AccountConfig{
		{Alias: "high", APIKey: "k1"},
		{Alias: "low", APIKey: "k2"},
	})
	setBalance(p.Get(0), 300)
	setBalance(p.Get(1), 50)

	counts := map[int]int{}
	for i := 0; i < 10000; i++ {
		acc := p.Pick(fmt.Sprintf("key-%d", i), false, "m") // warm → high-balance bias
		if acc == nil {
			t.Fatal("Pick returned nil")
		}
		counts[acc.ID]++
	}
	high, low := counts[p.Get(0).ID], counts[p.Get(1).ID]
	if low == 0 {
		t.Fatalf("low-balance account got 0 picks — starved: %+v", counts)
	}
	// P(low) ≈ 0.46/1.46 ≈ 31%. Allow a generous band for RNG noise.
	if low < 2000 || high < 6000 {
		t.Fatalf("distribution off: high=%d low=%d (want high≈6800 low≈3200)", high, low)
	}
}

// TestHRW_MinWeightPreventsStarvation: an account below the migration floor
// (balance 5) must still win some fresh keys via its minWeight floor.
func TestHRW_MinWeightPreventsStarvation(t *testing.T) {
	p := newTestPool([]AccountConfig{
		{Alias: "high", APIKey: "k1"},
		{Alias: "drain", APIKey: "k2"},
	})
	setBalance(p.Get(0), 300)
	setBalance(p.Get(1), 5) // < lowFloor → minWeight 0.05

	counts := map[int]int{}
	for i := 0; i < 20000; i++ {
		acc := p.Pick(fmt.Sprintf("key-%d", i), false, "m") // warm → high-balance bias
		if acc == nil {
			t.Fatal("Pick returned nil")
		}
		counts[acc.ID]++
	}
	if counts[p.Get(1).ID] == 0 {
		t.Fatalf("floor account starved: %+v", counts)
	}
	// P(drain) ≈ 0.05/1.05 ≈ 4.8%. Allow >1% to avoid flakiness.
	if pct := float64(counts[p.Get(1).ID]) / 20000; pct < 0.01 {
		t.Fatalf("floor account share %.3f below 1%%", pct)
	}
}

// TestSessionAffinity_StaysOnMappedAccount: once a key maps to an account, it
// must keep returning to that account as long as the account is healthy — even
// if another account's balance would make it a higher-weight pick. This is the
// core "affinity > weighting" guarantee.
func TestSessionAffinity_StaysOnMappedAccount(t *testing.T) {
	p := newTestPool([]AccountConfig{
		{Alias: "a", APIKey: "k1"},
		{Alias: "b", APIKey: "k2"},
	})
	setBalance(p.Get(0), 300)
	setBalance(p.Get(1), 50)

	key := "sticky-conversation"
	first := p.Pick(key, false, "m")
	for i := 0; i < 5; i++ {
		got := p.Pick(key, false, "m")
		if got.ID != first.ID {
			t.Fatalf("call %d: session migrated %d→%d (affinity broken)", i, first.ID, got.ID)
		}
	}

	// Bump the *other* account's balance higher. Weighting alone would now
	// prefer the other account, but the session must stay pinned.
	if first.ID == p.Get(0).ID {
		setBalance(p.Get(1), 300)
	} else {
		setBalance(p.Get(0), 300)
	}
	if got := p.Pick(key, false, "m"); got.ID != first.ID {
		t.Fatalf("session migrated when other account's weight rose: %d→%d", first.ID, got.ID)
	}
}

// TestMigrationFloor_WarmPromotesOffLowBalance: when the mapped account dips
// below lowFloor (still enabled/available) and the request is WARM (multi-turn),
// the session must migrate AND re-pin to a healthier account — the
// conversation has grown past cold, so its prefix cache belongs on a
// high-balance account. (Replaces the old transient-dip "keep pinned" test:
// that behavior was for the cold path, which now reuses instead.)
func TestMigrationFloor_WarmPromotesOffLowBalance(t *testing.T) {
	p := newTestPool([]AccountConfig{
		{Alias: "a", APIKey: "k1"},
		{Alias: "b", APIKey: "k2"},
	})
	setBalance(p.Get(0), 300)
	setBalance(p.Get(1), 300)

	key := "warm-dip"
	original := p.Pick(key, false, "m")
	other := p.Get(0).ID
	if other == original.ID {
		other = p.Get(1).ID
	}

	// Dip the original account below lowFloor (but NOT below drainFloor, so it
	// stays enabled/available — a dip, not exhaustion).
	setBalance(original, 10)
	migrated := p.Pick(key, false, "m") // warm → migrate off the low-balance account
	if migrated == nil {
		t.Fatal("Pick returned nil during warm dip")
	}
	if migrated.ID == original.ID {
		t.Fatalf("warm session did NOT migrate off low-balance account %d during dip", original.ID)
	}

	// Warm migration RE-PINS the session to the healthy account (unlike the old
	// transient-dip path which kept it pinned to the original).
	mappedID, ok := p.lookupSession(key)
	if !ok || mappedID != migrated.ID {
		t.Fatalf("warm session not re-pinned to healthy account %d; mapped=%d ok=%v", migrated.ID, mappedID, ok)
	}
}

// TestColdPick_KeepsDrainingBelowFloor: a below-lowFloor (but available) account
// stays the cold-drain target on repeated cold picks — drainScore favors the
// lowest balance — WITHOUT pinning the session. Cold picks never record a
// mapping, so a conversation that started cold is free to land on a high-balance
// account the moment it turns warm (see TestWarmAfterCold_PromotesToHighBalance),
// rather than being stuck on the low-balance account it drained on.
func TestColdPick_KeepsDrainingBelowFloor(t *testing.T) {
	p := newTestPool([]AccountConfig{
		{Alias: "a", APIKey: "k1"},
		{Alias: "b", APIKey: "k2"},
	})
	setBalance(p.Get(0), 300)
	setBalance(p.Get(1), 300)

	key := "cold-dip"
	original := p.Pick(key, true, "m")
	if original == nil {
		t.Fatal("Pick returned nil")
	}
	// Cold must NOT have pinned the session.
	if mappedID, ok := p.lookupSession(key); ok {
		t.Fatalf("cold pick pinned session to %d; cold must not pin", mappedID)
	}

	// Dip the original below lowFloor but keep it available.
	setBalance(original, 10)

	// A second cold pick keeps draining the (now lowest-balance) original —
	// drainScore favors it — and still does not pin.
	got := p.Pick(key, true, "m")
	if got == nil {
		t.Fatal("Pick returned nil during cold dip")
	}
	if got.ID != original.ID {
		t.Fatalf("cold pick drifted off the below-floor account %d → %d (drainScore should favor it)", original.ID, got.ID)
	}
	if mappedID, ok := p.lookupSession(key); ok {
		t.Fatalf("second cold pick pinned session to %d; cold must not pin", mappedID)
	}
}

// TestMigrationFloor_PermanentRepinsToFreshAccount: when the mapped account is
// PERMANENTLY unavailable (disabled at drainFloor), the session must re-pin to
// the picker-chosen fresh account so subsequent requests don't keep trying to
// revive a drained account.
func TestMigrationFloor_PermanentRepinsToFreshAccount(t *testing.T) {
	p := newTestPool([]AccountConfig{
		{Alias: "a", APIKey: "k1"},
		{Alias: "b", APIKey: "k2"},
	})
	setBalance(p.Get(0), 300)
	setBalance(p.Get(1), 300)

	key := "permanent-disable"
	original := p.Pick(key, false, "m")
	other := p.Get(0).ID
	if other == original.ID {
		other = p.Get(1).ID
	}

	// Disable the original account (simulates SetBalance dropping it below
	// drainFloor — a permanent condition, not a transient dip).
	original.mu.Lock()
	original.Enabled = false
	original.mu.Unlock()

	migrated := p.Pick(key, false, "m")
	if migrated == nil {
		t.Fatal("Pick returned nil after permanent disable")
	}
	if migrated.ID != other {
		t.Fatalf("did not migrate to other account %d; got %d", other, migrated.ID)
	}
	// Permanently unavailable → session re-pinned to the fresh account.
	mappedID, ok := p.lookupSession(key)
	if !ok || mappedID != other {
		t.Fatalf("session not re-pinned to fresh account %d; mapped=%d ok=%v", other, mappedID, ok)
	}
}

// TestSetBalance_SelfHealsEnabled: an account disabled by a prior sub-drain
// balance reading must be re-enabled once SetBalance sees a healthy balance —
// so a transient dip (or a bad reading) doesn't park the account until restart.
func TestSetBalance_SelfHealsEnabled(t *testing.T) {
	p := newTestPool([]AccountConfig{{Alias: "a", APIKey: "k1"}})
	acc := p.Get(0)

	acc.SetBalance(1, 1) // below drainFloor → disabled
	if acc.Enabled {
		t.Fatal("account should be disabled below drainFloor")
	}
	acc.SetBalance(100, 1) // recovered → re-enabled
	if !acc.Enabled {
		t.Fatal("account should self-heal to enabled on recovery")
	}
}

// TestHRW_ExcludeStillWorks: exclude semantics are preserved in the weighted
// path — excluding the picked account advances to the next, excluding all
// returns nil.
func TestHRW_ExcludeStillWorks(t *testing.T) {
	p := newTestPool([]AccountConfig{
		{Alias: "a", APIKey: "k1"},
		{Alias: "b", APIKey: "k2"},
		{Alias: "c", APIKey: "k3"},
	})
	for _, acc := range []int{0, 1, 2} {
		setBalance(p.Get(acc), 300)
	}

	key := "exclude-test"
	first := p.Pick(key, false, "m")
	if first == nil {
		t.Fatal("Pick returned nil")
	}
	second := p.Pick(key, false, "m", first.ID)
	if second == nil || second.ID == first.ID {
		t.Fatalf("exclude failed: first=%d second=%v", first.ID, second)
	}
	third := p.Pick(key, false, "m", first.ID, second.ID)
	if third == nil || third.ID == first.ID || third.ID == second.ID {
		t.Fatalf("exclude of two failed: %v", third)
	}
	if got := p.Pick(key, false, "m", first.ID, second.ID, third.ID); got != nil {
		t.Fatalf("expected nil after excluding all, got %d", got.ID)
	}
}

// TestHRW_StableWithinTier: balance changes WITHIN a tier must not move a key's
// mapping — only crossing a tier boundary (or the migration floor) can.
func TestHRW_StableWithinTier(t *testing.T) {
	p := newTestPool([]AccountConfig{
		{Alias: "a", APIKey: "k1"},
		{Alias: "b", APIKey: "k2"},
	})
	setBalance(p.Get(0), 270) // 250–299 tier
	setBalance(p.Get(1), 60)  // 50–99 tier

	key := "tier-stability"
	first := p.Pick(key, false, "m")

	// Wobble within the same tier — mapping must not move.
	setBalance(p.Get(0), 255)
	if got := p.Pick(key, false, "m"); got.ID != first.ID {
		t.Fatalf("mapping moved within tier: %d→%d", first.ID, got.ID)
	}
	setBalance(p.Get(0), 252)
	if got := p.Pick(key, false, "m"); got.ID != first.ID {
		t.Fatalf("mapping moved within tier: %d→%d", first.ID, got.ID)
	}
}

// TestNew_LegacyConfigDefaultsEnabled verifies that an AccountConfig with a nil
// Enabled pointer (a config.yaml written before the Enabled field existed)
// resolves to enabled at runtime — so the fix doesn't disable every account on
// the first restart after it ships.
func TestNew_LegacyConfigDefaultsEnabled(t *testing.T) {
	p := New([]AccountConfig{
		{ID: 1, APIKey: "sk-abc12345"}, // Enabled == nil
	}, "affinity", 20.0, true, 8192, 1000000)
	if !p.Get(0).Enabled {
		t.Fatalf("legacy account (nil Enabled) should default to enabled")
	}
}

func TestEnabledOrDefault(t *testing.T) {
	t.Run("nil defaults true", func(t *testing.T) {
		var c AccountConfig
		if !c.EnabledOrDefault() {
			t.Fatal("nil Enabled should default to true")
		}
	})
	t.Run("explicit false honored", func(t *testing.T) {
		f := false
		c := AccountConfig{Enabled: &f}
		if c.EnabledOrDefault() {
			t.Fatal("explicit false should be honored")
		}
	})
}

// TestEnabled_PersistedViaConfigs verifies that toggling an account's enabled
// state is reflected in Configs() so it survives a restart via the saver.
func TestEnabled_PersistedViaConfigs(t *testing.T) {
	p := New([]AccountConfig{{ID: 1, APIKey: "sk-abc12345"}}, "affinity", 20.0, true, 8192, 1000000)

	// Default enabled → Configs persists enabled=true (non-nil pointer).
	cfgs := p.Configs()
	if cfgs[0].Enabled == nil || !*cfgs[0].Enabled {
		t.Fatalf("default enabled should persist as true, got %v", cfgs[0].Enabled)
	}

	// Toggle off → Configs persists enabled=false.
	if !p.ToggleByID(1) {
		t.Fatal("toggle off failed")
	}
	cfgs = p.Configs()
	if cfgs[0].Enabled == nil || *cfgs[0].Enabled {
		t.Fatalf("disabled state should persist as false, got %v", cfgs[0].Enabled)
	}

	// Toggle back on → Configs persists enabled=true.
	if !p.ToggleByID(1) {
		t.Fatal("toggle on failed")
	}
	cfgs = p.Configs()
	if cfgs[0].Enabled == nil || !*cfgs[0].Enabled {
		t.Fatalf("re-enabled state should persist as true, got %v", cfgs[0].Enabled)
	}
}

// TestSeedNextID verifies that seeding the monotonic ID counter raises the
// next assigned ID, so a cleared-then-restarted deployment doesn't reuse IDs
// whose 7-day stats rows are still in the retention window.
func TestSeedNextID(t *testing.T) {
	p := New(nil, "affinity", 20.0, true, 8192, 1000000)
	p.SeedNextID(100)
	id := p.Add(AccountConfig{APIKey: "sk-abc12345"})
	if id != 100 {
		t.Fatalf("expected Add to use seeded ID 100, got %d", id)
	}
	if p.NextID() != 101 {
		t.Fatalf("expected NextID 101 after Add, got %d", p.NextID())
	}
}

// TestSeedNextID_NoOpWhenLower verifies SeedNextID never lowers the counter —
// when the persisted value is below the derived maxID+1 it is ignored.
func TestSeedNextID_NoOpWhenLower(t *testing.T) {
	// New with an account at ID 50 sets nextID = 51.
	p := New([]AccountConfig{{ID: 50, APIKey: "sk-abc12345"}}, "affinity", 20.0, true, 8192, 1000000)
	p.SeedNextID(10) // lower than 51 — must be ignored
	p.SeedNextID(0)  // legacy config — must be ignored
	id := p.Add(AccountConfig{APIKey: "sk-def67890"})
	if id != 51 {
		t.Fatalf("expected Add to use 51 (maxID+1), got %d", id)
	}
}

// TestSweepSessions_EvictsOldestTenPercent verifies the cap-exceeded path
// deletes the OLDEST 10% (sessionCap/10), not ~90% as the buggy formula did,
// and that the oldest entry is the one evicted. Lowers sessionCap for speed.
func TestSweepSessions_EvictsOldestTenPercent(t *testing.T) {
	orig := sessionCap
	sessionCap = 10
	t.Cleanup(func() { sessionCap = orig })

	p := New(nil, "affinity", 20.0, true, 8192, 1000000)
	p.sessTTL = 24 * time.Hour // prevent the TTL branch from deleting entries

	base := time.Now().Add(-time.Hour)
	// Insert cap+1 entries with strictly increasing last-seen.
	for i := 0; i < sessionCap+1; i++ {
		p.sessions[strconv.Itoa(i)] = &sessionEntry{
			accID: 0,
			last:  base.Add(time.Duration(i) * time.Second),
		}
	}

	p.sweepSessions()

	want := sessionCap + 1 - sessionCap/10 // 11 - 1 = 10
	if got := len(p.sessions); got != want {
		t.Fatalf("expected %d entries after eviction, got %d", want, got)
	}
	// The oldest entry (key "0") must be the one evicted.
	if _, ok := p.sessions["0"]; ok {
		t.Fatalf("oldest entry should be evicted")
	}
	// A newer entry must survive.
	if _, ok := p.sessions[strconv.Itoa(sessionCap)]; !ok {
		t.Fatalf("newest entry should survive")
	}
}

// TestSweepSessions_TTLExpiration verifies the TTL branch drops only expired
// entries and leaves fresh ones (the cap is never reached here).
func TestSweepSessions_TTLExpiration(t *testing.T) {
	p := New(nil, "affinity", 20.0, true, 8192, 1000000)
	p.sessTTL = 10 * time.Minute

	now := time.Now()
	p.sessions["fresh"] = &sessionEntry{accID: 0, last: now}
	p.sessions["expired"] = &sessionEntry{accID: 0, last: now.Add(-time.Hour)}

	p.sweepSessions()

	if _, ok := p.sessions["fresh"]; !ok {
		t.Fatal("fresh entry should survive")
	}
	if _, ok := p.sessions["expired"]; ok {
		t.Fatal("expired entry should be evicted")
	}
}

// TestAll_RequestCountNoRace exercises concurrent atomic writes to
// RequestCount against reads in All(). It only fails under `go test -race`.
func TestAll_RequestCountNoRace(t *testing.T) {
	p := New([]AccountConfig{{ID: 1, APIKey: "sk-abc12345"}}, "affinity", 20.0, true, 8192, 1000000)
	acc := p.Get(0)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			atomic.AddInt64(&acc.RequestCount, 1)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.All()
		}()
	}
	wg.Wait()
}

// cooldownSecs reads an account's remaining cooldown in seconds (test helper).
func cooldownSecs(acc *Account) int {
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	if rem := time.Until(acc.CooldownUntil); rem > 0 {
		return int(rem.Seconds())
	}
	return 0
}

// TestColdRequest_DrainsLowBalance: fresh cold keys must favor the LOW-balance
// account (drain bias), the inverse of the warm-biased distribution. Two
// accounts at {300, 15}: warm gives low ~5%, cold should give low a clear
// majority.
func TestColdRequest_DrainsLowBalance(t *testing.T) {
	p := newTestPool([]AccountConfig{
		{Alias: "high", APIKey: "k1"},
		{Alias: "low", APIKey: "k2"},
	})
	setBalance(p.Get(0), 300)
	setBalance(p.Get(1), 15) // < lowFloor → bottom tier, minWeight

	counts := map[int]int{}
	for i := 0; i < 10000; i++ {
		acc := p.Pick(fmt.Sprintf("cold-key-%d", i), true, "m") // cold → drain bias
		if acc == nil {
			t.Fatal("Pick returned nil")
		}
		counts[acc.ID]++
	}
	low := counts[p.Get(1).ID]
	if low == 0 {
		t.Fatalf("low-balance account got 0 cold picks — drain not working: %+v", counts)
	}
	// Drain bias inverts the weights: low (drainScore≈1.0) vs high (≈minWeight),
	// so low should win the large majority. Require a clear inversion (>50%).
	if pct := float64(low) / 10000; pct < 0.5 {
		t.Fatalf("cold drain did not favor low-balance account: low=%.0f%% (want >50%%)", pct*100)
	}
}

// TestColdRequest_DisabledFallsBackToWarmBias: when cold_drain is off, the cold
// flag is ignored and requests take the warm high-balance-biased path — so the
// low-balance account does NOT get a majority.
func TestColdRequest_DisabledFallsBackToWarmBias(t *testing.T) {
	p := New([]AccountConfig{
		{Alias: "high", APIKey: "k1"},
		{Alias: "low", APIKey: "k2"},
	}, "affinity", 20.0, false, 8192, 1000000) // cold_drain: false
	setBalance(p.Get(0), 300)
	setBalance(p.Get(1), 15)

	counts := map[int]int{}
	for i := 0; i < 10000; i++ {
		acc := p.Pick(fmt.Sprintf("key-%d", i), true, "m") // cold flag ignored when drain disabled
		if acc == nil {
			t.Fatal("Pick returned nil")
		}
		counts[acc.ID]++
	}
	low := counts[p.Get(1).ID]
	// With drain off, cold falls back to warm HRW → low gets ~5%, definitely
	// NOT a majority (that would mean drain is still active).
	if pct := float64(low) / 10000; pct >= 0.5 {
		t.Fatalf("cold drain should be OFF but low got %.0f%% (looks drained)", pct*100)
	}
}

// TestWarmAfterCold_PromotesToHighBalance: a conversation that starts cold
// (drained to the low-balance account, NOT pinned) must, once it turns warm
// (assistant turn present), land on the high-balance account and pin there —
// because cold never pinned, warm re-runs HRW and chooses high-balance rather
// than reusing the low-balance drain target.
func TestWarmAfterCold_PromotesToHighBalance(t *testing.T) {
	p := newTestPool([]AccountConfig{
		{Alias: "high", APIKey: "k1"},
		{Alias: "low", APIKey: "k2"},
	})
	setBalance(p.Get(0), 300)
	setBalance(p.Get(1), 15)

	key := "growing-convo"
	// Round 1: cold → drained to the low-balance account (drain bias), no pin.
	first := p.Pick(key, true, "m")
	if first == nil {
		t.Fatal("Pick returned nil")
	}
	if first.ID != p.Get(1).ID {
		t.Fatalf("cold pick did not land on low-balance account: got %d", first.ID)
	}

	// Round 2: same key, now warm (multi-turn). Cold left no mapping, so warm
	// re-runs HRW → high-balance account, and pins there.
	warm := p.Pick(key, false, "m")
	if warm == nil {
		t.Fatal("Pick returned nil on warm turn")
	}
	if warm.ID != p.Get(0).ID {
		t.Fatalf("warm turn did not promote to high-balance account: got %d", warm.ID)
	}
	mappedID, ok := p.lookupSession(key)
	if !ok || mappedID != p.Get(0).ID {
		t.Fatalf("session not re-pinned to high-balance account: mapped=%d ok=%v", mappedID, ok)
	}
}

// TestRecordSuccess_NoRace exercises concurrent Cooldown/RecordSuccess
// against each other and against reads. Only fails under `go test -race`.
func TestRecordSuccess_NoRace(t *testing.T) {
	p := newTestPool([]AccountConfig{{Alias: "a", APIKey: "k1"}})
	acc := p.Get(0)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Cooldown(acc, 60*time.Second)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.RecordSuccess(acc)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = cooldownSecs(acc)
		}()
	}
	wg.Wait()
}

// TestRecordForbidden_DisablesAfterTwoStrikes: a sustained 403 streak
// auto-disables the account exactly at forbiddenThreshold (2), not after one.
// The first 403 only bumps the streak; the second flips Enabled→false.
func TestRecordForbidden_DisablesAfterTwoStrikes(t *testing.T) {
	p := newTestPool([]AccountConfig{{Alias: "a", APIKey: "k1"}})
	acc := p.Get(0)
	if !acc.Enabled {
		t.Fatal("account should start enabled")
	}

	streak, disabled := p.RecordForbidden(acc)
	if streak != 1 || disabled {
		t.Fatalf("first 403: streak=%d disabled=%v, want 1/false", streak, disabled)
	}
	if !acc.Enabled {
		t.Fatal("account disabled after a single 403 (should need two)")
	}

	streak, disabled = p.RecordForbidden(acc)
	if streak != 2 || !disabled {
		t.Fatalf("second 403: streak=%d disabled=%v, want 2/true", streak, disabled)
	}
	if acc.Enabled {
		t.Fatal("account still enabled after 2 consecutive 403s")
	}

	// A third 403 keeps the streak climbing but is NOT a fresh disable (already
	// disabled), so disabled=false — the log/handler only fires once.
	streak, disabled = p.RecordForbidden(acc)
	if streak != 3 || disabled {
		t.Fatalf("third 403 on already-disabled: streak=%d disabled=%v, want 3/false", streak, disabled)
	}
}

// TestRecordForbidden_SuccessResetsStreak: a 200 between 403s resets the
// streak, so a one-off 403 amid healthy traffic does NOT accumulate toward an
// auto-disable.
func TestRecordForbidden_SuccessResetsStreak(t *testing.T) {
	p := newTestPool([]AccountConfig{{Alias: "a", APIKey: "k1"}})
	acc := p.Get(0)

	if _, disabled := p.RecordForbidden(acc); disabled {
		t.Fatal("first 403 should not disable")
	}
	// A success in between resets the streak to 0.
	p.RecordSuccess(acc)

	// Now another single 403 — streak should be 1, not 2, so still enabled.
	streak, disabled := p.RecordForbidden(acc)
	if streak != 1 || disabled {
		t.Fatalf("post-success 403: streak=%d disabled=%v, want 1/false", streak, disabled)
	}
	if !acc.Enabled {
		t.Fatal("account disabled by a non-consecutive 403 (success should have reset the streak)")
	}
}

// TestToggleByID_ClearsForbiddenStreak: re-enabling an auto-disabled account
// via the toggle clears the streak, so the operator's "I fixed the billing"
// re-enable starts fresh rather than re-disabling on the next 403.
func TestToggleByID_ClearsForbiddenStreak(t *testing.T) {
	p := newTestPool([]AccountConfig{{Alias: "a", APIKey: "k1"}})
	acc := p.Get(0)

	// Disable via two 403s.
	p.RecordForbidden(acc)
	p.RecordForbidden(acc)
	if acc.Enabled {
		t.Fatal("account should be auto-disabled")
	}

	// Operator re-enables via toggle.
	if !p.ToggleByID(acc.ID) {
		t.Fatal("toggle failed")
	}
	if !acc.Enabled {
		t.Fatal("toggle should re-enable")
	}
	// Streak reset → a single new 403 must NOT immediately re-disable.
	streak, disabled := p.RecordForbidden(acc)
	if streak != 1 || disabled {
		t.Fatalf("after toggle-re-enable, first 403: streak=%d disabled=%v, want 1/false (streak should have been cleared)", streak, disabled)
	}
	if !acc.Enabled {
		t.Fatal("re-disabled on the first 403 after a toggle-re-enable (streak was not cleared)")
	}
}

// TestDisabledByForbidden_OnlyFor403Disable: DisabledByForbidden distinguishes
// a 403 auto-disable from a manual toggle-off and a balance-driven disable.
func TestDisabledByForbidden_OnlyFor403Disable(t *testing.T) {
	p := newTestPool([]AccountConfig{{Alias: "a", APIKey: "k1"}})
	acc := p.Get(0)

	// Manual toggle-off → NOT a 403 disable.
	p.ToggleByID(acc.ID)
	if p.DisabledByForbidden(acc) {
		t.Fatal("manual disable reported as 403 disable")
	}

	// Re-enable, then two 403s → 403 disable.
	p.ToggleByID(acc.ID)
	p.RecordForbidden(acc)
	p.RecordForbidden(acc)
	if !p.DisabledByForbidden(acc) {
		t.Fatal("403 auto-disable not reported by DisabledByForbidden")
	}

	// Balance-driven disable (SetBalance below drainFloor) → NOT a 403 disable,
	// even though Enabled is false: the streak sits at threshold but SetBalance
	// is the authoritative reason. We approximate by resetting the streak first
	// to simulate a clean balance path.
	acc.mu.Lock()
	acc.forbiddenStreak = 0
	acc.mu.Unlock()
	acc.SetBalance(1, 1) // below drainFloor → Enabled=false
	if acc.Enabled {
		t.Fatal("balance disable didn't take")
	}
	if p.DisabledByForbidden(acc) {
		t.Fatal("balance-driven disable reported as 403 disable")
	}
}

// TestRecordForbidden_NoRace exercises concurrent RecordForbidden / RecordSuccess
// / DisabledByForbidden against each other. Only fails under `go test -race`.
func TestRecordForbidden_NoRace(t *testing.T) {
	p := newTestPool([]AccountConfig{{Alias: "a", APIKey: "k1"}})
	acc := p.Get(0)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.RecordForbidden(acc)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.RecordSuccess(acc)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.DisabledByForbidden(acc)
		}()
	}
	wg.Wait()
}

// TestWSDomain_PersistedViaConfigs: ws_domain 随 Configs() 往返，确保新增账号
// 填的专属域名能随 saver 写回 config.yaml 并在重启后读回。
func TestWSDomain_PersistedViaConfigs(t *testing.T) {
	p := New([]AccountConfig{
		{ID: 1, APIKey: "sk-abc12345", WSDomain: "ws-x.cn-beijing.maas.aliyuncs.com"},
	}, "affinity", 20.0, true, 8192, 1000000)

	cfgs := p.Configs()
	if cfgs[0].WSDomain != "ws-x.cn-beijing.maas.aliyuncs.com" {
		t.Fatalf("WSDomain not round-tripped: got %q", cfgs[0].WSDomain)
	}
}

// TestColdPick_DoesNotPinSession: cold (fresh, low-payload) requests must NOT
// record an affinity mapping — only warm (multi-turn) requests pin. This is the
// lever that lets a cold-drained conversation hand off to a high-balance account
// when it turns warm: because cold never pinned, the warm pick re-runs HRW and
// pins high-balance, instead of reusing the low-balance drain target.
func TestColdPick_DoesNotPinSession(t *testing.T) {
	p := newTestPool([]AccountConfig{
		{Alias: "high", APIKey: "k1"},
		{Alias: "low", APIKey: "k2"},
	})
	setBalance(p.Get(0), 300)
	setBalance(p.Get(1), 15) // below lowFloor → drain target

	key := "cold-no-pin"
	if acc := p.Pick(key, true, "m"); acc == nil {
		t.Fatal("Pick returned nil")
	}
	if mappedID, ok := p.lookupSession(key); ok {
		t.Fatalf("cold pick pinned session to %d; cold must not pin", mappedID)
	}
}

// TestSetBalance_RespectsManualDisable: a manually disabled account must stay
// disabled across monitor ticks. Reproduces the bug where SetBalance's healthy-
// balance branch forced Enabled=true every 5 min, clobbering the operator's
// disable so the card reverted to enabled on the next poll.
func TestSetBalance_RespectsManualDisable(t *testing.T) {
	p := newTestPool([]AccountConfig{{Alias: "a", APIKey: "k1"}})
	acc := p.Get(0)

	acc.SetBalance(50, 1) // healthy → enabled
	if !acc.Enabled {
		t.Fatal("healthy account should be enabled")
	}

	p.ToggleByID(acc.ID) // operator disables
	if acc.Enabled {
		t.Fatal("account should be disabled after toggle")
	}

	acc.SetBalance(50, 1) // monitor tick, still healthy
	if acc.Enabled {
		t.Fatal("SetBalance re-enabled a manually disabled account (disable reverted)")
	}

	// Re-enabling clears the manual-disable guard; healthy ticks keep it enabled.
	p.ToggleByID(acc.ID)
	acc.SetBalance(50, 1)
	if !acc.Enabled {
		t.Fatal("re-enabled account should stay enabled on a healthy tick")
	}
}

// TestSetBalance_PersistedDisabledNotReEnabled: an account that loads from
// config already disabled (the persisted result of a prior toggle) must not be
// flipped back on by the first monitor tick.
func TestSetBalance_PersistedDisabledNotReEnabled(t *testing.T) {
	off := false
	p := New([]AccountConfig{{Alias: "a", APIKey: "k1", Enabled: &off}}, "affinity", 20.0, true, 8192, 1000000)
	acc := p.Get(0)
	if acc.Enabled {
		t.Fatal("account should load disabled")
	}
	acc.SetBalance(50, 1)
	if acc.Enabled {
		t.Fatal("persisted-disabled account re-enabled by SetBalance")
	}
}
