package fallback

import "testing"

func TestPick(t *testing.T) {
	on := true
	off := false
	s := New([]Config{
		{ID: 1, Name: "exact", Models: []string{"qwen-plus", "gpt-4o"}},    // explicit whitelist
		{ID: 2, Name: "wild", Models: []string{"*"}},                       // wildcard
		{ID: 3, Name: "blank", Models: nil},                                // empty whitelist
		{ID: 4, Name: "off", Models: []string{"qwen-plus"}, Enabled: &off}, // disabled
		{ID: 5, Name: "defaulted-off-restored", Models: []string{"claude"}, Enabled: &on},
	})

	ids := func(cs []Config) []int {
		out := make([]int, len(cs))
		for i, c := range cs {
			out[i] = c.ID
		}
		return out
	}

	cases := []struct {
		name  string
		model string
		want  []int
	}{
		{"exact_hit", "qwen-plus", []int{1, 2}}, // exact(1) + wildcard(2); blank(3) excluded for real model
		{"exact_hit_other", "gpt-4o", []int{1, 2}},
		{"wildcard_catches_unlisted", "deepseek-r1", []int{2}},  // only wildcard lists it
		{"blank_excluded_for_real_model", "anything", []int{2}}, // blank(3) never matches a real model; wildcard(2) still catches
		{"disabled_excluded", "claude", []int{2, 5}},            // wildcard(2) + exact(5); disabled(4) excluded
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ids(s.Pick(tc.model))
			if len(got) != len(tc.want) {
				t.Fatalf("Pick(%q) = %v, want %v", tc.model, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Pick(%q)[%d] = %d, want %d (got %v)", tc.model, i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

func TestAddAssignsMonotonicID(t *testing.T) {
	s := New([]Config{{ID: 5, Name: "x"}})
	if id := s.Add(Config{Name: "y"}); id != 6 {
		t.Fatalf("Add after max ID 5 = %d, want 6", id)
	}
	if id := s.Add(Config{Name: "z"}); id != 7 {
		t.Fatalf("second Add = %d, want 7", id)
	}
}

// TestRecordThrottleCooldownSkipsPick: a 429 parks the channel so the very next
// Pick skips it (平滑切号 — no wasted 429 RTT).
func TestRecordThrottleCooldownSkipsPick(t *testing.T) {
	s := New([]Config{{ID: 1, Name: "ch", Models: []string{"m"}}})
	if len(s.Pick("m")) != 1 {
		t.Fatal("initial Pick should include the channel")
	}
	if streak, disabled := s.RecordThrottle(1); disabled || streak != 1 {
		t.Fatalf("first throttle: streak=%d disabled=%v want 1/false", streak, disabled)
	}
	if got := len(s.Pick("m")); got != 0 {
		t.Fatalf("Pick after 429 cooldown = %d, want 0 (channel parked)", got)
	}
}

// TestRecordFailureAutoDisablesAtThreshold: failThreshold consecutive failures
// flip the channel off with DisabledByErr set, and Pick drops it.
func TestRecordFailureAutoDisablesAtThreshold(t *testing.T) {
	s := New([]Config{{ID: 1, Name: "ch", Models: []string{"m"}}})
	for i := 1; i <= failThreshold; i++ {
		_, disabled := s.RecordFailure(1)
		if i < failThreshold && disabled {
			t.Fatalf("call %d disabled early (threshold=%d)", i, failThreshold)
		}
		if i == failThreshold && !disabled {
			t.Fatalf("call %d should have auto-disabled", i)
		}
	}
	v := s.All()[0]
	if v.Enabled || !v.DisabledByErr {
		t.Fatalf("after threshold: enabled=%v disabledByErr=%v want false/true", v.Enabled, v.DisabledByErr)
	}
	if len(s.Pick("m")) != 0 {
		t.Fatal("auto-disabled channel must be skipped by Pick")
	}
}

// TestRecordSuccessResetsStreak: a 200 clears the streak, so failures after it
// start counting from 0 and don't prematurely disable.
func TestRecordSuccessResetsStreak(t *testing.T) {
	s := New([]Config{{ID: 1, Name: "ch", Models: []string{"m"}}})
	for i := 0; i < failThreshold-1; i++ { // one shy of disable
		s.RecordFailure(1)
	}
	s.RecordSuccess(1)
	s.RecordFailure(1) // streak is now 1, not failThreshold
	if v := s.All()[0]; !v.Enabled {
		t.Fatal("single failure after a success must not disable (streak was reset)")
	}
}

// TestToggleReenableClearsAutoDisable: re-enabling an auto-disabled channel
// clears DisabledByErr + streak + cooldown, so it re-enters rotation fresh.
func TestToggleReenableClearsAutoDisable(t *testing.T) {
	s := New([]Config{{ID: 1, Name: "ch", Models: []string{"m"}}})
	for i := 0; i < failThreshold; i++ {
		s.RecordFailure(1)
	}
	if v := s.All()[0]; !v.DisabledByErr {
		t.Fatal("channel should be auto-disabled before toggle")
	}
	if !s.ToggleByID(1) {
		t.Fatal("ToggleByID returned false")
	}
	v := s.All()[0]
	if !v.Enabled || v.DisabledByErr {
		t.Fatalf("re-enable should clear: enabled=%v disabledByErr=%v", v.Enabled, v.DisabledByErr)
	}
	if len(s.Pick("m")) != 1 {
		t.Fatal("re-enabled channel should re-enter Pick")
	}
}
