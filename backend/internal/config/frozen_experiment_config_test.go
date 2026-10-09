package config

import (
	"os"
	"testing"
	"time"
)

// TestFrozenExperimentConfigs is the pre-flight check for the
// frozen-strategy experiment. The hand-authored configs/frozen_experiment_*.yaml
// are seeded directly into the LIVE strategy_configs table (advisor disabled),
// so they never pass through the advisor's promotion-time validation. A broken
// config on a live bot = real money risk. This test proves, before seeding, that
// each frozen config is RUNTIME-valid and holds the invariants the experiment
// design depends on.
//
// Note on valid_until: the frozen config uses a far-future valid_until (2030) so
// it never expires at runtime. That intentionally exceeds the promotion-time TTL
// hard_limit (config_ttl_minutes 60–120). Since the advisor is disabled and the
// config is seeded directly, that promotion-time guard never runs — so the test
// asserts the config is clean on every pass EXCEPT a single, expected
// hard_limit error on valid_until, and that IsActive(now) (the real runtime
// gate the engine uses) returns true.
func TestFrozenExperimentConfigs(t *testing.T) {
	limits, err := LoadHardLimits("../../../configs/hard_limits.yaml")
	if err != nil {
		t.Fatalf("load hard_limits: %v", err)
	}
	v := NewValidator(limits)
	allowed := []string{"momentum_pullback", "breakout_follow", "range_breakout_probe", "no_trade"}

	cases := []struct {
		file   string
		symbol string
		cfgID  string
	}{
		{"../../../configs/frozen_experiment_USD_JPY.yaml", "USD_JPY", "frozen-mompull-usdjpy-v1"},
		{"../../../configs/frozen_experiment_EUR_JPY.yaml", "EUR_JPY", "frozen-mompull-eurjpy-v1"},
		{"../../../configs/frozen_experiment_GBP_JPY.yaml", "GBP_JPY", "frozen-mompull-gbpjpy-v1"},
		{"../../../configs/frozen_experiment_EUR_USD.yaml", "EUR_USD", "frozen-mompull-eurusd-v1"},
		{"../../../configs/frozen_experiment_GBP_USD.yaml", "GBP_USD", "frozen-mompull-gbpusd-v1"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.symbol, func(t *testing.T) {
			raw, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatalf("read %s: %v", tc.file, err)
			}
			cfg, err := ParseStrategyConfig(raw)
			if err != nil {
				t.Fatalf("parse %s: %v", tc.file, err)
			}

			// Passes that must be fully clean at runtime.
			if res := v.ValidateSchema(cfg); !res.OK() {
				t.Fatalf("schema invalid:\n%s", res.Summary())
			}
			if res := v.ValidateSemantic(cfg, allowed); !res.OK() {
				t.Fatalf("semantic invalid:\n%s", res.Summary())
			}
			if res := v.ValidateRisk(cfg, AccountState{}); !res.OK() {
				t.Fatalf("risk invalid:\n%s", res.Summary())
			}
			// Hard-limit pass: clean EXCEPT the deliberate far-future valid_until.
			for _, e := range v.ValidateHardLimit(cfg).Errors {
				if e.Field != "valid_until" {
					t.Errorf("unexpected hard_limit error: %s", e.Error())
				}
			}

			// The real runtime gate the engine uses each tick. This is what
			// actually decides whether the frozen config trades.
			if !cfg.IsActive(time.Now()) {
				t.Errorf("IsActive(now) = false — frozen config would NOT trade")
			}

			// --- invariants the experiment design depends on ---
			if cfg.ConfigID != tc.cfgID {
				t.Errorf("config_id = %q, want %q", cfg.ConfigID, tc.cfgID)
			}
			if cfg.Symbol != tc.symbol {
				t.Errorf("symbol = %q, want %q", cfg.Symbol, tc.symbol)
			}
			if !cfg.Enabled {
				t.Errorf("enabled = false, want true (frozen config must trade)")
			}
			if cfg.IsNoTradeDecision() {
				t.Errorf("config resolves to no_trade; want an active trading config")
			}
			if cfg.Strategy.Name != StrategyMomentumPullback {
				t.Errorf("strategy = %q, want momentum_pullback", cfg.Strategy.Name)
			}
			// direction=both → side は相場 (6h/24h トレンド) が決める。盲目ロング偏重を防ぐ核心。
			if cfg.Entry.Direction != DirectionBoth {
				t.Errorf("direction = %q, want both", cfg.Entry.Direction)
			}
			// 損小利大: SL < TP。
			if cfg.Exit.StopLossPips >= cfg.Exit.TakeProfitPips {
				t.Errorf("SL %.1f >= TP %.1f — geometry is not 損小利大",
					cfg.Exit.StopLossPips, cfg.Exit.TakeProfitPips)
			}
			// ratchet RR floor: locked = arm - giveback >= TP * ratchetMinLockedFracOfTP.
			locked := cfg.Exit.RatchetArmPips - cfg.Exit.RatchetGivebackPips
			if floor := cfg.Exit.TakeProfitPips * ratchetMinLockedFracOfTP; locked < floor {
				t.Errorf("ratchet locked %.1f < floor %.1f (TP %.1f × %.2f)",
					locked, floor, cfg.Exit.TakeProfitPips, ratchetMinLockedFracOfTP)
			}
			// 固定 config は窓が永続するため lifetime cap にしない (0 = 無制限)。
			if cfg.Risk.MaxTradesInThisWindow != 0 {
				t.Errorf("max_trades_in_this_window = %d, want 0 (unlimited)", cfg.Risk.MaxTradesInThisWindow)
			}
		})
	}
}
