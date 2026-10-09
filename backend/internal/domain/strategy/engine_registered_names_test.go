package strategy

import "testing"

// The production AllowedStrategies whitelist must be
// DERIVED from the engine registry so it can never drift from what the engine can
// actually run. A registered strategy missing from a hand-maintained whitelist would
// make a re-enabled advisor reject/replace an active config on its first cycle.
// RegisteredNames is the single source.
func TestEngine_RegisteredNames_IncludesEveryLiveStrategy(t *testing.T) {
	got := NewEngine().RegisteredNames()
	set := map[string]bool{}
	for _, n := range got {
		set[string(n)] = true
	}
	want := []string{
		"no_trade", "momentum_pullback", "breakout_follow",
		"range_breakout_probe", "mtf_pullback", "ma_pullback", "ma_pullback_v2",
		"gotobi_fix", "london_breakout", "trend_follow", "daily_trend",
		"exhaustion_fade",
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("RegisteredNames() missing %q; got %v", w, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("RegisteredNames() returned %d names %v; want %d", len(got), got, len(want))
	}
	// Determinism: two calls must return identical, sorted order.
	again := NewEngine().RegisteredNames()
	for i := range got {
		if got[i] != again[i] {
			t.Errorf("RegisteredNames() not deterministic at %d: %q vs %q", i, got[i], again[i])
		}
	}
}
