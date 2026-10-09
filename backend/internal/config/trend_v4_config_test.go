package config

import (
	"os"
	"testing"
	"time"
)

// TestTrendV4Configs is the validator gate for the tracked trend_v4_*.yaml configs (trend_follow runner).
// It closes the hole where a malformed config could be SQL-inserted past the validator and break the
// live bot; seeding itself goes through scripts/seed_active_config.sh (cmd/config-check runs the same
// startup checks before the upsert).
func TestTrendV4Configs(t *testing.T) {
	limits, err := LoadHardLimits("../../../configs/hard_limits.yaml")
	if err != nil {
		t.Fatalf("load hard_limits: %v", err)
	}
	v := NewValidator(limits)
	// Must include trend_follow (the v4 strategy); mirrors the engine's RegisteredNames whitelist.
	allowed := []string{"momentum_pullback", "breakout_follow", "range_breakout_probe", "mtf_pullback",
		"ma_pullback", "ma_pullback_v2", "gotobi_fix", "london_breakout", "trend_follow", "daily_trend", "no_trade"}

	cases := []struct {
		file, symbol, cfgID string
	}{
		{"../../../configs/trend_v4_USD_JPY.yaml", "USD_JPY", "trend-v4-usdjpy"},
		{"../../../configs/trend_v4_EUR_JPY.yaml", "EUR_JPY", "trend-v4-eurjpy"},
		{"../../../configs/trend_v4_GBP_JPY.yaml", "GBP_JPY", "trend-v4-gbpjpy"},
		{"../../../configs/trend_v4_EUR_USD.yaml", "EUR_USD", "trend-v4-eurusd"},
		{"../../../configs/trend_v4_GBP_USD.yaml", "GBP_USD", "trend-v4-gbpusd"},
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
				t.Errorf("IsActive(now) = false — v4 config would NOT trade")
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
			if cfg.Strategy.Name != StrategyTrendFollow {
				t.Errorf("strategy = %q, want trend_follow", cfg.Strategy.Name)
			}
			// v4 kill-switch must be set (forward-test brake).
			if cfg.Risk.MaxLossInThisWindowJPY <= 0 {
				t.Errorf("max_loss_in_this_window_jpy = %d, want > 0 (kill-switch)", cfg.Risk.MaxLossInThisWindowJPY)
			}
		})
	}
}
