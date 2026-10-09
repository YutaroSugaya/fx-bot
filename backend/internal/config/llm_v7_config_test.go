package config

import (
	"os"
	"testing"
	"time"
)

// TestLLMV7Configs is the validator gate for the tracked llm_v7_*.yaml configs (LLM loop).
// They are seeded with scripts/seed_active_config.sh, which runs the bot's startup checks
// (cmd/config-check); this test guards the tracked files themselves.
//
// llm_v7 configs = the trend_follow / exhaustion-fade configs with TWO deliberate policy choices:
//  1. max_loss_in_this_window_jpy = 0 (window kill-switch 無効)。valid_from 起点でリセットされない
//     累積カウンタは小さい lot だと「生涯数敗で永久凍結」になり、ペアが無言で凍結する。
//     残る防御 = 日次損失cap / per-trade cap / スプレッド / emergency_stop / broker OCO / ナンピン禁止。
//  2. entry.max_spread_pips = 3.0 (スプレッド床をコード側 3.0 に一本化し、admission 層まで貫通させる —
//     LLM サイクルが 3.0 で通した判断を admission がより狭い値で黙殺しないように)。
//
// それ以外(strategy ブロック・出口・quantity・hour partition)は元の trend_follow / exhaustion-fade config と同一。
func TestLLMV7Configs(t *testing.T) {
	limits, err := LoadHardLimits("../../../configs/hard_limits.yaml")
	if err != nil {
		t.Fatalf("load hard_limits: %v", err)
	}
	v := NewValidator(limits)
	allowed := []string{"momentum_pullback", "breakout_follow", "range_breakout_probe", "mtf_pullback",
		"ma_pullback", "ma_pullback_v2", "gotobi_fix", "london_breakout", "trend_follow", "daily_trend",
		"exhaustion_fade", "no_trade"}

	cases := []struct {
		file, symbol, cfgID string
		strategy            StrategyName
	}{
		{"../../../configs/llm_v7_USD_JPY.yaml", "USD_JPY", "llm-v7-usdjpy", StrategyExhaustionFade},
		{"../../../configs/llm_v7_EUR_JPY.yaml", "EUR_JPY", "llm-v7-eurjpy", StrategyTrendFollow},
		{"../../../configs/llm_v7_GBP_JPY.yaml", "GBP_JPY", "llm-v7-gbpjpy", StrategyTrendFollow},
		{"../../../configs/llm_v7_EUR_USD.yaml", "EUR_USD", "llm-v7-eurusd", StrategyTrendFollow},
		{"../../../configs/llm_v7_GBP_USD.yaml", "GBP_USD", "llm-v7-gbpusd", StrategyTrendFollow},
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
				t.Errorf("IsActive(now) = false — v7 config would NOT trade")
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
			if cfg.Strategy.Name != tc.strategy {
				t.Errorf("strategy = %q, want %q", cfg.Strategy.Name, tc.strategy)
			}

			// ★ Policy change 1: the window kill-switch is REMOVED (0 = gate disabled).
			// A >0 value here re-creates the invisible cumulative freeze — reject it.
			if cfg.Risk.MaxLossInThisWindowJPY != 0 {
				t.Errorf("max_loss_in_this_window_jpy = %d, want 0 (window kill-switch disabled)",
					cfg.Risk.MaxLossInThisWindowJPY)
			}
			if cfg.Risk.MaxTradesInThisWindow != 0 {
				t.Errorf("max_trades_in_this_window = %d, want 0 (disabled)", cfg.Risk.MaxTradesInThisWindow)
			}

			// ★ Policy change 2: spread cap unified at 3.0 through the admission layer
			// (a tighter 1.0-1.5 would silently kill LLM entries at 1.5-3.0).
			if cfg.Entry.MaxSpreadPips != 3.0 {
				t.Errorf("entry.max_spread_pips = %v, want 3.0 (unified spread floor)", cfg.Entry.MaxSpreadPips)
			}

			// Unchanged live semantics: sizing and single-position cap.
			if cfg.Risk.Quantity != 1000 {
				t.Errorf("risk.quantity = %d, want 1000 (unchanged; LLM path forces its own qty)", cfg.Risk.Quantity)
			}
			if cfg.Risk.MaxOpenPositions != 1 {
				t.Errorf("risk.max_open_positions = %d, want 1", cfg.Risk.MaxOpenPositions)
			}

			// USD_JPY keeps the deterministic exhaustion_fade hour partition (JST 4/10/11)
			// in lockstep with bot_config llm_decision.exclude_hours_jst.
			if tc.symbol == "USD_JPY" {
				want := []int{4, 10, 11}
				got := cfg.Entry.AllowedHoursJST
				if len(got) != len(want) {
					t.Fatalf("allowed_hours_jst = %v, want %v", got, want)
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("allowed_hours_jst = %v, want %v", got, want)
					}
				}
			}
		})
	}
}
