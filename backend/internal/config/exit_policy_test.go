package config

import (
	"testing"
	"time"
)

// exit_policy=strategy_computed marks configs whose
// real TP/SL/MaxHold/ratchet come from strategy constants (ma_pullback /
// mtf_pullback), not the config. The config's tp/sl are validator placeholders.
// Such configs are long-lived (frozen experiment, valid_until 2030) so they are
// exempt from the [60,120] TTL window, exempt from TP/SL range and RR-floor
// checks, and — being permanent-window — MUST have max_trades_in_this_window=0
// (a positive cap over a multi-year window would halt trading after N trades:
// a halt footgun).

func testHardLimits(t *testing.T) *HardLimits {
	t.Helper()
	limits, err := LoadHardLimits("../../../configs/hard_limits.yaml")
	if err != nil {
		t.Fatalf("load hard_limits: %v", err)
	}
	return limits
}

func strategyComputedCfg() *StrategyConfig {
	return &StrategyConfig{
		ConfigID:     "frozen-mapb-usdjpy-v3-1",
		ValidFrom:    time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
		ValidUntil:   time.Date(2030, 12, 31, 23, 59, 59, 0, time.UTC),
		Symbol:       "USD_JPY",
		Enabled:      true,
		MarketRegime: MarketRegime{Type: RegimeUnclear, Confidence: 0.7},
		Strategy:     StrategySection{Name: StrategyMAPullback},
		Entry:        EntrySection{MaxSpreadPips: 1.0, RequireBreakout: false, Direction: DirectionBoth},
		Exit: ExitSection{
			TakeProfitPips: 14, StopLossPips: 10, MaxHoldMinutes: 480,
			RatchetArmPips: 16, RatchetGivebackPips: 8,
		},
		Risk: ConfigRiskSection{Quantity: 1000, MaxOpenPositions: 1, MaxTradesInThisWindow: 0},
	}
}

func TestIsStrategyComputedExit_DerivesFromStrategy(t *testing.T) {
	cases := []struct {
		name     string
		strat    StrategyName
		explicit ExitPolicy
		want     bool
	}{
		{"ma_pullback default", StrategyMAPullback, "", true},
		{"mtf_pullback default", StrategyMTFPullback, "", true},
		{"momentum default", StrategyMomentumPullback, "", false},
		{"breakout default", StrategyBreakoutFollow, "", false},
		{"explicit strategy_computed overrides", StrategyMomentumPullback, ExitPolicyStrategyComputed, true},
		{"explicit config_driven overrides", StrategyMAPullback, ExitPolicyConfigDriven, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &StrategyConfig{Strategy: StrategySection{Name: tc.strat}, Exit: ExitSection{ExitPolicy: tc.explicit}}
			if got := c.IsStrategyComputedExit(); got != tc.want {
				t.Errorf("IsStrategyComputedExit() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateHardLimit_StrategyComputed_TTLExempt(t *testing.T) {
	limits := testHardLimits(t)
	v := NewValidator(limits)
	cfg := strategyComputedCfg() // valid_until = 2030 (way outside [60,120] TTL)
	for _, e := range v.ValidateHardLimit(cfg).Errors {
		t.Errorf("strategy_computed config must be TTL-exempt; got %s", e.Error())
	}
}

func TestValidateHardLimit_StrategyComputed_RejectsPositiveMaxTrades(t *testing.T) {
	limits := testHardLimits(t)
	v := NewValidator(limits)
	cfg := strategyComputedCfg()
	cfg.Risk.MaxTradesInThisWindow = 5 // halt-footgun on a permanent window
	found := false
	for _, e := range v.ValidateHardLimit(cfg).Errors {
		if e.Field == "risk.max_trades_in_this_window" {
			found = true
		}
	}
	if !found {
		t.Error("strategy_computed (permanent-window) config with positive max_trades must be rejected (halt footgun)")
	}
}

func TestValidateHardLimit_StrategyComputed_SkipsTPSLRange(t *testing.T) {
	limits := testHardLimits(t)
	v := NewValidator(limits)
	cfg := strategyComputedCfg()
	cfg.Exit.TakeProfitPips = 75 // honest runner TP, far outside the placeholder range
	cfg.Exit.StopLossPips = 18
	for _, e := range v.ValidateHardLimit(cfg).Errors {
		if e.Field == "exit.take_profit_pips" || e.Field == "exit.stop_loss_pips" {
			t.Errorf("strategy_computed TP/SL are placeholders and must be range-exempt; got %s", e.Error())
		}
	}
}

func TestValidateSchema_StrategyComputed_SkipsRRFloor(t *testing.T) {
	v := NewValidator(testHardLimits(t))
	cfg := strategyComputedCfg()
	cfg.Exit.TakeProfitPips = 30 // honest TP; locked=arm-give=8 < TP*0.5=15 would fail RR-floor
	for _, e := range v.ValidateSchema(cfg).Errors {
		if e.Field == "exit.ratchet" {
			t.Errorf("strategy_computed must skip the RR-floor (honest TP needs no fake placeholder); got %s", e.Error())
		}
	}
}

func TestValidateHardLimit_ConfigDriven_StillEnforcesTTLAndRanges(t *testing.T) {
	// Regression: a normal advisor (config_driven) config must STILL get TTL +
	// TP/SL range enforcement — the exemptions are strategy_computed-only.
	v := NewValidator(testHardLimits(t))
	cfg := strategyComputedCfg()
	cfg.Strategy.Name = StrategyMomentumPullback
	cfg.Exit.ExitPolicy = ExitPolicyConfigDriven
	cfg.Exit.RatchetArmPips = 0
	cfg.Exit.RatchetGivebackPips = 0
	errs := v.ValidateHardLimit(cfg)
	hasTTL := false
	for _, e := range errs.Errors {
		if e.Field == "valid_until" {
			hasTTL = true
		}
	}
	if !hasTTL {
		t.Error("config_driven config with 2030 valid_until must still fail the TTL window check")
	}
}
