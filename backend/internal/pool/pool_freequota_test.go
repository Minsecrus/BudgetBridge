package pool

import (
	"fmt"
	"testing"
)

func TestFreeQuotaAccounting(t *testing.T) {
	acc := &Account{}
	// Pristine model reads full quota.
	if got := acc.freeRemaining(1_000_000, "glm-5.2"); got != 1_000_000 {
		t.Fatalf("pristine: want 1000000, got %d", got)
	}
	// Partial consume reduces only that model.
	acc.ConsumeFree("glm-5.2", 400_000)
	if got := acc.freeRemaining(1_000_000, "glm-5.2"); got != 600_000 {
		t.Fatalf("after consume: want 600000, got %d", got)
	}
	// A DIFFERENT model is independent — the whole point of per-model.
	if got := acc.freeRemaining(1_000_000, "deepseek-v4-pro"); got != 1_000_000 {
		t.Fatalf("other model must be independent: want 1000000, got %d", got)
	}
	// Drain glm-5.2 to zero.
	acc.ConsumeFree("glm-5.2", 600_000)
	if got := acc.freeRemaining(1_000_000, "glm-5.2"); got != 0 {
		t.Fatalf("drained: want 0, got %d", got)
	}
	// Disabled quota (the backward-compat gate).
	if got := acc.freeRemaining(0, "glm-5.2"); got != 0 {
		t.Fatalf("disabled quota: want 0, got %d", got)
	}
	// Empty model / non-positive tokens are no-ops.
	acc.ConsumeFree("", 100)
	acc.ConsumeFree("glm-5.2", 0)
}

func TestPickColdPrefersAccountWithFreeQuotaForModel(t *testing.T) {
	// A: low-balance, glm-5.2 quota drained. B: healthy, glm-5.2 quota pristine.
	// Cold glm-5.2 traffic must prefer B (still has free quota) over A.
	cfgs := []AccountConfig{{ID: 1, Alias: "A"}, {ID: 2, Alias: "B"}}
	p := New(cfgs, "affinity", 20, true, 8192, 1_000_000)
	p.ByID(1).SetBalance(5, 1)                  // low, still alive (> drainFloor=3)
	p.ByID(2).SetBalance(200, 1)                // healthy
	p.ByID(1).ConsumeFree("glm-5.2", 1_000_000) // A's glm-5.2 quota gone; B pristine

	aCount, bCount := 0, 0
	for i := 0; i < 200; i++ {
		acc := p.Pick(fmt.Sprintf("k-%d", i), true, "glm-5.2")
		if acc == nil {
			t.Fatal("nil pick")
		}
		switch acc.ID {
		case 1:
			aCount++
		case 2:
			bCount++
		}
	}
	if aCount >= bCount {
		t.Fatalf("cold glm-5.2 should prefer free-quota account B: A=%d B=%d", aCount, bCount)
	}
}

func TestPickColdRecoversOtherModelQuotaOnLowBalanceAccount(t *testing.T) {
	// The headline scenario: A is bled dry on glm-5.2 (low balance + glm-5.2
	// quota exhausted) but deepseek-v4-pro quota is pristine. A cold
	// deepseek-v4-pro request should prefer A, not treat it as empty.
	cfgs := []AccountConfig{{ID: 1, Alias: "A"}, {ID: 2, Alias: "B"}}
	p := New(cfgs, "affinity", 20, true, 8192, 1_000_000)
	p.ByID(1).SetBalance(5, 1)
	p.ByID(2).SetBalance(5, 1)                          // equal low balance → equal drain weight
	p.ByID(1).ConsumeFree("glm-5.2", 1_000_000)         // A exhausted on glm-5.2 only
	p.ByID(2).ConsumeFree("deepseek-v4-pro", 1_000_000) // B exhausted on the requested model; A pristine

	// A has the requested model's free quota (coldScore 2.0); B doesn't and
	// falls back to drainScore (1.0 at this balance). Weighted HRW gives A ~2/3
	// of distinct cold keys — assert A takes the majority (not a fixed ratio,
	// which would be flaky under hash variance).
	aCount, bCount := 0, 0
	for i := 0; i < 300; i++ {
		switch p.Pick(fmt.Sprintf("k-%d", i), true, "deepseek-v4-pro").ID {
		case 1:
			aCount++
		case 2:
			bCount++
		}
	}
	if aCount <= bCount {
		t.Fatalf("cold deepseek-v4-pro should prefer A (pristine quota): A=%d B=%d", aCount, bCount)
	}
}

func TestPickColdFreeQuotaDisabledFallsBackToDrain(t *testing.T) {
	// freeQuotaPerModel=0 must reproduce the original balance-drain behavior.
	cfgs := []AccountConfig{{ID: 1, Alias: "A"}, {ID: 2, Alias: "B"}}
	p := New(cfgs, "affinity", 20, true, 8192, 0) // disabled
	p.ByID(1).SetBalance(5, 1)
	p.ByID(2).SetBalance(5, 1)
	p.ByID(1).ConsumeFree("glm-5.2", 1_000_000)

	aCount, bCount := 0, 0
	for i := 0; i < 200; i++ {
		acc := p.Pick(fmt.Sprintf("k-%d", i), true, "glm-5.2")
		switch acc.ID {
		case 1:
			aCount++
		case 2:
			bCount++
		}
	}
	// Equal drain weight → roughly even split; A is NOT penalized for lacking
	// quota (awareness off).
	if aCount == 0 || bCount == 0 {
		t.Fatalf("disabled awareness should split roughly evenly: A=%d B=%d", aCount, bCount)
	}
}
