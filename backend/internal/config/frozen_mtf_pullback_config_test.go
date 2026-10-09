package config

import (
	"os"
	"testing"
	"time"
)

// TestFrozenMTFPullbackConfigs is the pre-flight check for the v2 frozen
// experiment (mtf_pullback). Like TestFrozenExperimentConfigs (v1), the
// hand-authored configs/frozen_mtf_pullback_*.yaml are meant to be seeded
// directly into the LIVE strategy_configs table (advisor disabled), bypassing
// promotion-time validation — so this proves they are RUNTIME-valid before any
// seed. It does NOT seed or switch the live config; it only guards the files.
func TestFrozenMTFPullbackConfigs(t *testing.T) {
	limits, err := LoadHardLimits("../../../configs/hard_limits.yaml")
	if err != nil {
		t.Fatalf("load hard_limits: %v", err)
	}
	v := NewValidator(limits)
	allowed := []string{"momentum_pullback", "breakout_follow", "range_breakout_probe", "mtf_pullback", "no_trade"}

	cases := []struct {
		file   string
		symbol string
		cfgID  string
	}{
		{"../../../configs/frozen_mtf_pullback_USD_JPY.yaml", "USD_JPY", "frozen-mtfpb-usdjpy-v2"},
		{"../../../configs/frozen_mtf_pullback_EUR_JPY.yaml", "EUR_JPY", "frozen-mtfpb-eurjpy-v2"},
		{"../../../configs/frozen_mtf_pullback_GBP_JPY.yaml", "GBP_JPY", "frozen-mtfpb-gbpjpy-v2"},
		// EUR_USD / GBP_USD (pip=0.0001) work because mtf_pullback sizes everything
		// via market.PipSize — unlike v1 momentum_pullback's hardcoded 0.01.
		{"../../../configs/frozen_mtf_pullback_EUR_USD.yaml", "EUR_USD", "frozen-mtfpb-eurusd-v2"},
		{"../../../configs/frozen_mtf_pullback_GBP_USD.yaml", "GBP_USD", "frozen-mtfpb-gbpusd-v2"},
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

			if !cfg.IsActive(time.Now()) {
				t.Errorf("IsActive(now) = false — frozen config would NOT trade")
			}

			if cfg.ConfigID != tc.cfgID {
				t.Errorf("config_id = %q, want %q", cfg.ConfigID, tc.cfgID)
			}
			if cfg.Symbol != tc.symbol {
				t.Errorf("symbol = %q, want %q", cfg.Symbol, tc.symbol)
			}
			if !cfg.Enabled {
				t.Errorf("enabled = false, want true")
			}
			if cfg.IsNoTradeDecision() {
				t.Errorf("config resolves to no_trade; want an active trading config")
			}
			if cfg.Strategy.Name != StrategyMTFPullback {
				t.Errorf("strategy = %q, want mtf_pullback", cfg.Strategy.Name)
			}
			if cfg.Entry.Direction != DirectionBoth {
				t.Errorf("direction = %q, want both", cfg.Entry.Direction)
			}
			if cfg.Entry.RequireBreakout {
				t.Errorf("require_breakout = true; mtf_pullback requires false")
			}
			if cfg.Exit.StopLossPips >= cfg.Exit.TakeProfitPips {
				t.Errorf("SL %.1f >= TP %.1f — not 損小利大", cfg.Exit.StopLossPips, cfg.Exit.TakeProfitPips)
			}
			// mtf_pullback v2 drops ratchet (公開されている裁量手法に無い): the strategy
			// computes structural TP/SL at entry and zeroes ratchet in the Signal,
			// so the frozen config must have ratchet OFF — not the v1 trailing setup.
			if cfg.Exit.RatchetArmPips != 0 || cfg.Exit.RatchetGivebackPips != 0 {
				t.Errorf("ratchet must be OFF for mtf_pullback v2, got arm=%.1f give=%.1f",
					cfg.Exit.RatchetArmPips, cfg.Exit.RatchetGivebackPips)
			}
			if cfg.Risk.MaxTradesInThisWindow != 0 {
				t.Errorf("max_trades_in_this_window = %d, want 0", cfg.Risk.MaxTradesInThisWindow)
			}
		})
	}
}
