package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validHardLimitsYAML = `
hard_limits:
  allowed_symbols:
    - USD_JPY
  quantity:
    min: 100
    max: 100
  take_profit_pips:
    min: 1.0
    max: 8.0
  stop_loss_pips:
    min: 1.0
    max: 5.0
  max_hold_minutes:
    min: 1
    max: 30
  max_trades_in_this_window:
    min: 0
    max: 5
  max_loss_in_this_window_jpy:
    min: 0
    max: 500
  max_spread_pips:
    min: 0.1
    max: 0.5
  config_ttl_minutes:
    min: 30
    max: 90
`

func writeHardLimitsYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hard_limits.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp yaml: %v", err)
	}
	return path
}

func TestLoadHardLimits_OK(t *testing.T) {
	path := writeHardLimitsYAML(t, validHardLimitsYAML)
	h, err := LoadHardLimits(path)
	if err != nil {
		t.Fatalf("LoadHardLimits: %v", err)
	}
	if len(h.AllowedSymbols) != 1 || h.AllowedSymbols[0] != "USD_JPY" {
		t.Errorf("allowed_symbols: %v", h.AllowedSymbols)
	}
	if h.Quantity.Min != 100 || h.Quantity.Max != 100 {
		t.Errorf("quantity range: %+v", h.Quantity)
	}
	if h.TakeProfitPips.Min != 1.0 || h.TakeProfitPips.Max != 8.0 {
		t.Errorf("take_profit_pips: %+v", h.TakeProfitPips)
	}
	if h.ConfigTTLMinutes.Min != 30 || h.ConfigTTLMinutes.Max != 90 {
		t.Errorf("config_ttl_minutes: %+v", h.ConfigTTLMinutes)
	}
}

func TestLoadHardLimits_EmptyAllowedSymbols(t *testing.T) {
	bad := strings.Replace(validHardLimitsYAML, "  allowed_symbols:\n    - USD_JPY", "  allowed_symbols: []", 1)
	path := writeHardLimitsYAML(t, bad)
	_, err := LoadHardLimits(path)
	if err == nil || !strings.Contains(err.Error(), "allowed_symbols") {
		t.Fatalf("expected allowed_symbols error, got %v", err)
	}
}

func TestLoadHardLimits_MinGreaterThanMax(t *testing.T) {
	bad := strings.Replace(validHardLimitsYAML, "  take_profit_pips:\n    min: 1.0\n    max: 8.0", "  take_profit_pips:\n    min: 9.0\n    max: 1.0", 1)
	path := writeHardLimitsYAML(t, bad)
	_, err := LoadHardLimits(path)
	if err == nil || !strings.Contains(err.Error(), "take_profit_pips") {
		t.Fatalf("expected min>max error, got %v", err)
	}
}

func TestLoadHardLimits_NegativeMin(t *testing.T) {
	bad := strings.Replace(validHardLimitsYAML, "  stop_loss_pips:\n    min: 1.0\n    max: 5.0", "  stop_loss_pips:\n    min: -1.0\n    max: 5.0", 1)
	path := writeHardLimitsYAML(t, bad)
	_, err := LoadHardLimits(path)
	if err == nil || !strings.Contains(err.Error(), "stop_loss_pips") {
		t.Fatalf("expected negative-min error, got %v", err)
	}
}

func TestLoadHardLimits_BadYAML(t *testing.T) {
	path := writeHardLimitsYAML(t, "hard_limits:\n  allowed_symbols:\n  - USD_JPY\n bad indent\n")
	_, err := LoadHardLimits(path)
	if err == nil || !strings.Contains(err.Error(), "parse hard_limits") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestLoadHardLimits_FileNotFound(t *testing.T) {
	_, err := LoadHardLimits("/tmp/this-does-not-exist-fx-bot-hl.yaml")
	if err == nil || !strings.Contains(err.Error(), "read hard_limits") {
		t.Fatalf("expected read error, got %v", err)
	}
}

func TestFloatRange_Contains(t *testing.T) {
	r := FloatRange{Min: 1.0, Max: 5.0}
	cases := []struct {
		v    float64
		want bool
	}{
		{1.0, true},
		{5.0, true},
		{3.0, true},
		{0.99, false},
		{5.01, false},
		{-1, false},
	}
	for _, c := range cases {
		if got := r.Contains(c.v); got != c.want {
			t.Errorf("Contains(%v) = %v, want %v", c.v, got, c.want)
		}
	}
}

func TestIntRange_Contains(t *testing.T) {
	r := IntRange{Min: 0, Max: 5}
	cases := []struct {
		v    int
		want bool
	}{
		{0, true},
		{5, true},
		{3, true},
		{-1, false},
		{6, false},
	}
	for _, c := range cases {
		if got := r.Contains(c.v); got != c.want {
			t.Errorf("Contains(%d) = %v, want %v", c.v, got, c.want)
		}
	}
}

// strategy_limits の YAML パース + 整合性検証 ----------------------------

const hardLimitsWithStrategyLimitsYAML = `
hard_limits:
  allowed_symbols:
    - USD_JPY
  quantity:
    min: 100
    max: 100
  take_profit_pips:
    min: 3.0
    max: 8.0
  stop_loss_pips:
    min: 2.0
    max: 3.0
  max_hold_minutes:
    min: 1
    max: 30
  max_trades_in_this_window:
    min: 0
    max: 5
  max_loss_in_this_window_jpy:
    min: 0
    max: 500
  max_spread_pips:
    min: 0.1
    max: 0.5
  config_ttl_minutes:
    min: 30
    max: 90
  strategy_limits:
    momentum_pullback:
      take_profit_pips:
        min: 2.0
        max: 4.0
      stop_loss_pips:
        min: 2.0
        max: 3.0
    breakout_follow:
      take_profit_pips:
        min: 5.0
        max: 10.0
      stop_loss_pips:
        min: 3.0
        max: 4.0
    range_breakout_probe:
      take_profit_pips:
        min: 3.0
        max: 6.0
      stop_loss_pips:
        min: 2.0
        max: 4.0
`

func TestLoadHardLimits_StrategyLimits_Parsed(t *testing.T) {
	path := writeHardLimitsYAML(t, hardLimitsWithStrategyLimitsYAML)
	h, err := LoadHardLimits(path)
	if err != nil {
		t.Fatalf("LoadHardLimits: %v", err)
	}
	if h.StrategyLimits == nil || len(h.StrategyLimits) != 3 {
		t.Fatalf("expected 3 strategy_limits, got %v", h.StrategyLimits)
	}
	rr, ok := h.StrategyLimits["momentum_pullback"]
	if !ok {
		t.Fatalf("momentum_pullback missing from strategy_limits: %v", h.StrategyLimits)
	}
	if rr.TakeProfitPips.Min != 2.0 || rr.TakeProfitPips.Max != 4.0 {
		t.Errorf("momentum_pullback TP: %+v", rr.TakeProfitPips)
	}
	bf := h.StrategyLimits["breakout_follow"]
	if bf.TakeProfitPips.Max != 10.0 {
		t.Errorf("breakout_follow TP max: %v", bf.TakeProfitPips.Max)
	}
	probe := h.StrategyLimits["range_breakout_probe"]
	if probe.TakeProfitPips.Min != 3.0 || probe.StopLossPips.Max != 4.0 {
		t.Errorf("range_breakout_probe limits: %+v", probe)
	}
}

func TestLoadHardLimits_StrategyLimits_LimitsFor(t *testing.T) {
	path := writeHardLimitsYAML(t, hardLimitsWithStrategyLimitsYAML)
	h, _ := LoadHardLimits(path)
	if h.LimitsFor("momentum_pullback") == nil {
		t.Errorf("expected non-nil for momentum_pullback")
	}
	if h.LimitsFor("range_breakout_probe") == nil {
		t.Errorf("expected non-nil for range_breakout_probe")
	}
	if h.LimitsFor("range_reversion") != nil {
		t.Errorf("expected nil for range_reversion (not in strategy_limits)")
	}
	if h.LimitsFor("") != nil {
		t.Errorf("expected nil for empty strategy name")
	}
	// nil-receiver は panic しない
	var nilH *HardLimits
	if nilH.LimitsFor("momentum_pullback") != nil {
		t.Errorf("nil receiver should return nil")
	}
}

func TestLoadHardLimits_StrategyLimits_MinGreaterThanMax(t *testing.T) {
	bad := strings.Replace(hardLimitsWithStrategyLimitsYAML,
		"      take_profit_pips:\n        min: 2.0\n        max: 4.0",
		"      take_profit_pips:\n        min: 5.0\n        max: 4.0", 1)
	path := writeHardLimitsYAML(t, bad)
	_, err := LoadHardLimits(path)
	if err == nil || !strings.Contains(err.Error(), "strategy_limits[momentum_pullback]") {
		t.Fatalf("expected strategy_limits inconsistency error, got %v", err)
	}
}

func TestLoadHardLimits_StrategyLimits_NegativeMin(t *testing.T) {
	bad := strings.Replace(hardLimitsWithStrategyLimitsYAML,
		"      stop_loss_pips:\n        min: 2.0\n        max: 3.0",
		"      stop_loss_pips:\n        min: -1.0\n        max: 3.0", 1)
	path := writeHardLimitsYAML(t, bad)
	_, err := LoadHardLimits(path)
	if err == nil || !strings.Contains(err.Error(), "must be >= 0") {
		t.Fatalf("expected negative min error, got %v", err)
	}
}

// PaperConfig parsing + validation tests ----------------------------

const hardLimitsWithPaperYAML = `
hard_limits:
  allowed_symbols:
    - USD_JPY
  quantity:
    min: 100
    max: 100
  take_profit_pips:
    min: 1.0
    max: 8.0
  stop_loss_pips:
    min: 1.0
    max: 5.0
  max_hold_minutes:
    min: 1
    max: 30
  max_trades_in_this_window:
    min: 0
    max: 5
  max_loss_in_this_window_jpy:
    min: 0
    max: 500
  max_spread_pips:
    min: 0.1
    max: 0.5
  config_ttl_minutes:
    min: 30
    max: 90
  paper:
    simulated_slippage_pips: 0.5
    api_fee_jpy_per_trade: 10.0
`

func TestLoadHardLimits_PaperSection_Parsed(t *testing.T) {
	path := writeHardLimitsYAML(t, hardLimitsWithPaperYAML)
	h, err := LoadHardLimits(path)
	if err != nil {
		t.Fatalf("LoadHardLimits: %v", err)
	}
	if h.Paper == nil {
		t.Fatal("Paper should be non-nil when paper section is present")
	}
	if h.Paper.SimulatedSlippagePips != 0.5 {
		t.Errorf("SimulatedSlippagePips: got %v want 0.5", h.Paper.SimulatedSlippagePips)
	}
	if h.Paper.APIFeeJPYPerTrade != 10.0 {
		t.Errorf("APIFeeJPYPerTrade: got %v want 10.0", h.Paper.APIFeeJPYPerTrade)
	}
}

func TestLoadHardLimits_PaperSection_Optional(t *testing.T) {
	// validHardLimitsYAML から paper セクションが無いケース。Paper は nil でOK。
	path := writeHardLimitsYAML(t, validHardLimitsYAML)
	h, err := LoadHardLimits(path)
	if err != nil {
		t.Fatalf("LoadHardLimits: %v", err)
	}
	if h.Paper != nil {
		t.Errorf("Paper should be nil when paper section absent; got %+v", h.Paper)
	}
}

func TestLoadHardLimits_PaperSection_NegativeRejected(t *testing.T) {
	bad := strings.Replace(hardLimitsWithPaperYAML,
		"simulated_slippage_pips: 0.5", "simulated_slippage_pips: -0.1", 1)
	path := writeHardLimitsYAML(t, bad)
	_, err := LoadHardLimits(path)
	if err == nil || !strings.Contains(err.Error(), "paper") {
		t.Fatalf("expected paper negative-value error, got %v", err)
	}
}

// CooldownConfig parsing + validation ---------------------------

const hardLimitsWithCooldownYAML = `
hard_limits:
  allowed_symbols:
    - USD_JPY
  quantity:
    min: 100
    max: 100
  take_profit_pips:
    min: 1.0
    max: 8.0
  stop_loss_pips:
    min: 1.0
    max: 5.0
  max_hold_minutes:
    min: 1
    max: 30
  max_trades_in_this_window:
    min: 0
    max: 5
  max_loss_in_this_window_jpy:
    min: 0
    max: 500
  max_spread_pips:
    min: 0.1
    max: 0.5
  config_ttl_minutes:
    min: 30
    max: 90
  cooldown:
    after_entry_seconds: 60
    after_loss_seconds: 300
    after_take_profit_seconds: 120
`

func TestLoadHardLimits_CooldownSection_Parsed(t *testing.T) {
	path := writeHardLimitsYAML(t, hardLimitsWithCooldownYAML)
	h, err := LoadHardLimits(path)
	if err != nil {
		t.Fatalf("LoadHardLimits: %v", err)
	}
	if h.Cooldown == nil {
		t.Fatal("Cooldown should be non-nil when cooldown section is present")
	}
	if h.Cooldown.AfterEntrySeconds != 60 {
		t.Errorf("AfterEntrySeconds: got %d want 60", h.Cooldown.AfterEntrySeconds)
	}
	if h.Cooldown.AfterLossSeconds != 300 {
		t.Errorf("AfterLossSeconds: got %d want 300", h.Cooldown.AfterLossSeconds)
	}
	if h.Cooldown.AfterTakeProfitSeconds != 120 {
		t.Errorf("AfterTakeProfitSeconds: got %d want 120", h.Cooldown.AfterTakeProfitSeconds)
	}
}

func TestLoadHardLimits_CooldownSection_Optional(t *testing.T) {
	path := writeHardLimitsYAML(t, validHardLimitsYAML)
	h, err := LoadHardLimits(path)
	if err != nil {
		t.Fatalf("LoadHardLimits: %v", err)
	}
	if h.Cooldown != nil {
		t.Errorf("Cooldown should be nil when absent; got %+v", h.Cooldown)
	}
}

func TestLoadHardLimits_CooldownSection_NegativeRejected(t *testing.T) {
	bad := strings.Replace(hardLimitsWithCooldownYAML,
		"after_entry_seconds: 60", "after_entry_seconds: -1", 1)
	path := writeHardLimitsYAML(t, bad)
	_, err := LoadHardLimits(path)
	if err == nil || !strings.Contains(err.Error(), "cooldown") {
		t.Fatalf("expected cooldown negative-value error, got %v", err)
	}
}

func TestSymbolAllowed(t *testing.T) {
	h := &HardLimits{AllowedSymbols: []string{"USD_JPY", "EUR_JPY"}}
	if !h.SymbolAllowed("USD_JPY") {
		t.Errorf("USD_JPY should be allowed")
	}
	if !h.SymbolAllowed("EUR_JPY") {
		t.Errorf("EUR_JPY should be allowed")
	}
	if h.SymbolAllowed("BTC_JPY") {
		t.Errorf("BTC_JPY should not be allowed")
	}
	if h.SymbolAllowed("") {
		t.Errorf("empty should not be allowed")
	}
}
