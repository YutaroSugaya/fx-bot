package config

import (
	"strings"
	"testing"
)

// Per-symbol order size: a main pair can run at a larger size (here USD_JPY at 10,000
// units) while the other pairs stay at a smaller global size as a selective probe tier.
// quantity_by_symbol overrides llm_decision.quantity per pair; unset pair falls back
// to the global quantity (same per-pair-map shape as max_range_position_24h_buy).
func TestLoadBotConfig_ParsesQuantityBySymbol(t *testing.T) {
	body := validBotConfigYAML + `
llm_decision:
  enabled: true
  symbols: [USD_JPY, EUR_JPY]
  quantity: 2000
  quantity_by_symbol:
    USD_JPY: 10000
`
	cfg, err := LoadBotConfig(writeTempYAML(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	lc := cfg.LLMDecision
	if got := lc.QuantityFor("USD_JPY"); got != 10000 {
		t.Errorf("QuantityFor(USD_JPY) = %d, want 10000 (per-symbol override)", got)
	}
	if got := lc.QuantityFor("EUR_JPY"); got != 2000 {
		t.Errorf("QuantityFor(EUR_JPY) = %d, want 2000 (fallback to global quantity)", got)
	}
}

// A 0 in the map is an explicit "use the global quantity" (0 = off convention);
// an omitted map keeps every pair on the global quantity (full back-compat).
func TestLLMDecisionSection_QuantityForFallbacks(t *testing.T) {
	s := LLMDecisionSection{Quantity: 2000, QuantityBySymbol: map[string]int{"USD_JPY": 0}}
	if got := s.QuantityFor("USD_JPY"); got != 2000 {
		t.Errorf("explicit 0 must fall back to global quantity, got %d", got)
	}
	none := LLMDecisionSection{Quantity: 2000}
	if got := none.QuantityFor("GBP_JPY"); got != 2000 {
		t.Errorf("omitted map must fall back to global quantity, got %d", got)
	}
	// Global quantity 0 too → 0: the wiring layer then applies hard_limits.quantity.min
	// (1,000) exactly as it does today for a zero global quantity.
	zero := LLMDecisionSection{}
	if got := zero.QuantityFor("USD_JPY"); got != 0 {
		t.Errorf("all-zero config must yield 0 (wiring applies the hard-limits floor), got %d", got)
	}
}

// A negative size is always a typo and must fail LOUDLY at boot, not flow toward the
// order path (the risk Gate would reject it anyway, but per the loud-fail rule for
// every other llm_decision knob, a broken value must not boot).
func TestLoadBotConfig_RejectsNegativeQuantityBySymbol(t *testing.T) {
	body := validBotConfigYAML + `
llm_decision:
  enabled: true
  symbols: [USD_JPY]
  quantity_by_symbol:
    USD_JPY: -1000
`
	_, err := LoadBotConfig(writeTempYAML(t, body))
	if err == nil || !strings.Contains(err.Error(), "quantity_by_symbol") {
		t.Fatalf("want load error mentioning quantity_by_symbol, got %v", err)
	}
}
