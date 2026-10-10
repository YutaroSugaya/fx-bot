package config

import (
	"os"
	"testing"
	"time"
)

// TestPaperV83Config is the validator gate for the PAPER active config (USD_JPY).
// It is seeded with scripts/seed_active_config.sh --mode paper_config, which runs the
// bot's startup checks (cmd/config-check) before upserting.
//
// なぜ必要か:
//
//	bot.mode: paper_config で動かすには「paper_config の active strategy_configs 行」が要る。
//	それが無い / スタブのまま (raw_yaml が空に近い・valid_until 失効) だと起動時 validation に落ちる:
//	  active_config_db_load_failed_starting_without_active (paper は warn して続行)
//	→ ActiveConfigHolder が空 → LLM が go を出しても
//	   llm_decision_cycle.go の cfgID=="" 分岐で stage=no_active_config (「設定未解決でスキップ」)。
//	つまり全判断が無音で消える。
//
// admission に効くのは entry.max_spread_pips / risk.* だけで、strategy ブロック
// (exhaustion_fade・allowed_hours_jst) は llm_decision.exclude_hours_jst[USD_JPY]=[] により
// per-tick engine が USD_JPY を一切持たないため実行されない (EngineOwnedHoursJST=空)。
func TestPaperV83Config(t *testing.T) {
	limits, err := LoadHardLimits("../../../configs/hard_limits.yaml")
	if err != nil {
		t.Fatalf("load hard_limits: %v", err)
	}
	v := NewValidator(limits)
	allowed := []string{"momentum_pullback", "breakout_follow", "range_breakout_probe", "mtf_pullback",
		"ma_pullback", "ma_pullback_v2", "gotobi_fix", "london_breakout", "trend_follow", "daily_trend",
		"exhaustion_fade", "no_trade"}

	const (
		file  = "../../../configs/paper_v83_USD_JPY.yaml"
		cfgID = "paper-v83-usdjpy"
	)

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	cfg, err := ParseStrategyConfig(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	// 起動時に走る検証と同じ経路 (Promoter.LoadActiveFromDB → Validator.ValidateStatic)。
	// ここが緑でなければ seed しても holder は空のまま = no_active_config が続く。
	if res := v.ValidateStatic(cfg, allowed...); !res.OK() {
		for _, e := range res.Errors {
			// 意図的な遠未来 valid_until (凍結 config) だけは hard_limit TTL 外で許容。
			if e.Field == "valid_until" && e.Type == ValidationHardLimit {
				continue
			}
			t.Errorf("static validation error: %s", e.Error())
		}
	}

	if !cfg.IsActive(time.Now()) {
		t.Errorf("IsActive(now) = false — paper config would NOT trade")
	}
	if cfg.ConfigID != cfgID {
		t.Errorf("config_id = %q, want %q", cfg.ConfigID, cfgID)
	}
	if cfg.Symbol != "USD_JPY" {
		t.Errorf("symbol = %q, want USD_JPY", cfg.Symbol)
	}
	if !cfg.Enabled {
		t.Errorf("enabled = false, want true")
	}
	if cfg.IsNoTradeDecision() {
		t.Errorf("config resolves to no_trade; want an active trading config")
	}

	// admission 面のパラメータ (live に移すときも同じ値で比較できるよう固定する)。
	if cfg.Entry.MaxSpreadPips != 3.0 {
		t.Errorf("entry.max_spread_pips = %v, want 3.0 (unified spread floor)", cfg.Entry.MaxSpreadPips)
	}
	if cfg.Risk.Quantity != 1000 {
		t.Errorf("risk.quantity = %d, want 1000", cfg.Risk.Quantity)
	}
	if cfg.Risk.MaxOpenPositions != 1 {
		t.Errorf("risk.max_open_positions = %d, want 1", cfg.Risk.MaxOpenPositions)
	}
	if cfg.Risk.MaxLossInThisWindowJPY != 0 {
		t.Errorf("max_loss_in_this_window_jpy = %d, want 0 (window kill-switch 無効)",
			cfg.Risk.MaxLossInThisWindowJPY)
	}
	if cfg.Risk.MaxTradesInThisWindow != 0 {
		t.Errorf("max_trades_in_this_window = %d, want 0 (disabled)", cfg.Risk.MaxTradesInThisWindow)
	}
}
