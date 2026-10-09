package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseStrategyConfig_OK(t *testing.T) {
	data, err := os.ReadFile("../../../configs/strategy_config.active.example.yaml")
	if err != nil {
		t.Fatalf("read example yaml: %v", err)
	}
	cfg, err := ParseStrategyConfig(data)
	if err != nil {
		t.Fatalf("ParseStrategyConfig: %v", err)
	}
	if cfg.Symbol != "USD_JPY" {
		t.Errorf("symbol: %q", cfg.Symbol)
	}
	if !cfg.Enabled {
		t.Errorf("enabled should be true in example")
	}
	if cfg.Strategy.Name != StrategyMomentumPullback {
		t.Errorf("strategy.name: %q", cfg.Strategy.Name)
	}
	if cfg.Exit.TakeProfitPips != 12.0 {
		t.Errorf("take_profit_pips: %v", cfg.Exit.TakeProfitPips)
	}
	if cfg.Risk.Quantity != 1000 {
		t.Errorf("risk.quantity: %v", cfg.Risk.Quantity)
	}
}

// Early-exit fields: YAML parsing must populate
// EarlyExitWindowMinutes / EarlyExitTargetPips on the Exit section so the
// advisor can write configs that shorten the worst-case MaxHold force-close.
func TestParseStrategyConfig_EarlyExitFields(t *testing.T) {
	yaml := `
config_id: "test-early-exit"
generated_at: "2026-05-23T00:00:00Z"
valid_from: "2026-05-23T00:00:00Z"
valid_until: "2026-05-23T01:00:00Z"
symbol: USD_JPY
enabled: true
market_regime: { type: "trend_up", confidence: 0.6, reason: "test" }
strategy: { name: "momentum_pullback", timeframe: "5m", trend_timeframe: "1h" }
entry:
  max_spread_pips: 0.5
  min_volatility_pips_5m: 1.5
  max_volatility_pips_5m: 6.0
  require_breakout: false
  direction: both
exit:
  take_profit_pips: 30.0
  stop_loss_pips: 20.0
  max_hold_minutes: 240
  early_exit_window_minutes: 30
  early_exit_target_pips: -2.0
risk: { quantity: 1000, max_open_positions: 1, max_trades_in_this_window: 2, max_loss_in_this_window_jpy: 1500 }
no_trade: { enabled: false, reason: "" }
`
	cfg, err := ParseStrategyConfig([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseStrategyConfig: %v", err)
	}
	if cfg.Exit.EarlyExitWindowMinutes != 30 {
		t.Errorf("early_exit_window_minutes: got %d want 30", cfg.Exit.EarlyExitWindowMinutes)
	}
	if cfg.Exit.EarlyExitTargetPips != -2.0 {
		t.Errorf("early_exit_target_pips: got %v want -2.0", cfg.Exit.EarlyExitTargetPips)
	}
}

// Omitting the early-exit fields must leave them as zero (feature disabled).
// Ensures back-compat: existing configs without the new fields keep working.
// 旧 config (early_exit_* フィールド無し) は parser が default 0 にフォールバック
// すること。enabled config の 0 は validator が reject するが (ratchet OFF 時)、
// パーサー自体は引き続き後方互換でゼロを返す (validator は別レイヤ)。
func TestParseStrategyConfig_EarlyExitOmitted_DefaultsZero(t *testing.T) {
	yaml := `
config_id: "test-no-early-exit"
generated_at: "2026-05-25T00:00:00Z"
valid_from: "2026-05-25T00:00:00Z"
valid_until: "2026-05-25T01:00:00Z"
symbol: USD_JPY
enabled: true
market_regime: { type: "trend_up", confidence: 0.6, reason: "test" }
strategy: { name: "momentum_pullback", timeframe: "5m", trend_timeframe: "1h" }
entry: { max_spread_pips: 0.5, min_volatility_pips_5m: 1.5, max_volatility_pips_5m: 6.0, require_breakout: false, direction: both }
exit:
  take_profit_pips: 30.0
  stop_loss_pips: 20.0
  max_hold_minutes: 240
risk: { quantity: 1000, max_open_positions: 1, max_trades_in_this_window: 2, max_loss_in_this_window_jpy: 1500 }
no_trade: { enabled: false, reason: "" }
`
	cfg, err := ParseStrategyConfig([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseStrategyConfig: %v", err)
	}
	if cfg.Exit.EarlyExitWindowMinutes != 0 {
		t.Errorf("expected default 0, got %d", cfg.Exit.EarlyExitWindowMinutes)
	}
	if cfg.Exit.EarlyExitTargetPips != 0 {
		t.Errorf("expected default 0, got %v", cfg.Exit.EarlyExitTargetPips)
	}
}

// RatchetArmPips / RatchetGivebackPips は trailing take-profit。peak が arm
// に達して以降、peak から giveback 戻ったら paper/live ともに OnTick で
// MARKET close (Live は既存の cancel→MARKET フローを再利用)。0 / 0 = OFF。
func TestParseStrategyConfig_RatchetFields(t *testing.T) {
	yaml := `
config_id: "test-ratchet"
generated_at: "2026-05-25T00:00:00Z"
valid_from: "2026-05-25T00:00:00Z"
valid_until: "2026-05-25T01:00:00Z"
symbol: USD_JPY
enabled: true
market_regime: { type: "trend_up", confidence: 0.6, reason: "test" }
strategy: { name: "momentum_pullback", timeframe: "5m", trend_timeframe: "1h" }
entry:
  max_spread_pips: 0.5
  min_volatility_pips_5m: 1.5
  max_volatility_pips_5m: 6.0
  require_breakout: false
  direction: both
exit:
  take_profit_pips: 30.0
  stop_loss_pips: 20.0
  max_hold_minutes: 240
  ratchet_arm_pips: 5.0
  ratchet_giveback_pips: 3.0
risk: { quantity: 1000, max_open_positions: 1, max_trades_in_this_window: 2, max_loss_in_this_window_jpy: 1500 }
no_trade: { enabled: false, reason: "" }
`
	cfg, err := ParseStrategyConfig([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseStrategyConfig: %v", err)
	}
	if cfg.Exit.RatchetArmPips != 5.0 {
		t.Errorf("ratchet_arm_pips: got %v want 5.0", cfg.Exit.RatchetArmPips)
	}
	if cfg.Exit.RatchetGivebackPips != 3.0 {
		t.Errorf("ratchet_giveback_pips: got %v want 3.0", cfg.Exit.RatchetGivebackPips)
	}
}

// 既存 config (ratchet フィールド無し) は 0 / 0 にフォールバックして feature
// disable 扱いになること。後方互換ガード (validator は別レイヤで reject)。
func TestParseStrategyConfig_RatchetOmitted_DefaultsZero(t *testing.T) {
	yaml := `
config_id: "test-no-ratchet"
generated_at: "2026-05-25T00:00:00Z"
valid_from: "2026-05-25T00:00:00Z"
valid_until: "2026-05-25T01:00:00Z"
symbol: USD_JPY
enabled: true
market_regime: { type: "trend_up", confidence: 0.6, reason: "test" }
strategy: { name: "momentum_pullback", timeframe: "5m", trend_timeframe: "1h" }
entry: { max_spread_pips: 0.5, min_volatility_pips_5m: 1.5, max_volatility_pips_5m: 6.0, require_breakout: false, direction: both }
exit:
  take_profit_pips: 30.0
  stop_loss_pips: 20.0
  max_hold_minutes: 240
risk: { quantity: 1000, max_open_positions: 1, max_trades_in_this_window: 2, max_loss_in_this_window_jpy: 1500 }
no_trade: { enabled: false, reason: "" }
`
	cfg, err := ParseStrategyConfig([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseStrategyConfig: %v", err)
	}
	if cfg.Exit.RatchetArmPips != 0 {
		t.Errorf("expected default 0, got %v", cfg.Exit.RatchetArmPips)
	}
	if cfg.Exit.RatchetGivebackPips != 0 {
		t.Errorf("expected default 0, got %v", cfg.Exit.RatchetGivebackPips)
	}
}

func TestParseStrategyConfig_BadYAML(t *testing.T) {
	_, err := ParseStrategyConfig([]byte("not: yaml\n  bad indent: [unclosed"))
	if err == nil || !strings.Contains(err.Error(), "strategy_config yaml") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func mustParseTime(t *testing.T, layout, value string) time.Time {
	t.Helper()
	tm, err := time.Parse(layout, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return tm
}

func TestStrategyConfig_IsActive(t *testing.T) {
	from := mustParseTime(t, time.RFC3339, "2026-05-15T10:00:00+09:00")
	until := mustParseTime(t, time.RFC3339, "2026-05-15T11:00:00+09:00")
	inside := mustParseTime(t, time.RFC3339, "2026-05-15T10:30:00+09:00")
	before := mustParseTime(t, time.RFC3339, "2026-05-15T09:30:00+09:00")
	after := mustParseTime(t, time.RFC3339, "2026-05-15T11:30:00+09:00")

	base := func() *StrategyConfig {
		return &StrategyConfig{
			ConfigID:   "x",
			Symbol:     "USD_JPY",
			Enabled:    true,
			ValidFrom:  from,
			ValidUntil: until,
			Strategy:   StrategySection{Name: StrategyMomentumPullback},
		}
	}

	t.Run("active in window", func(t *testing.T) {
		c := base()
		if !c.IsActive(inside) {
			t.Errorf("should be active inside window")
		}
	})
	t.Run("before valid_from", func(t *testing.T) {
		c := base()
		if c.IsActive(before) {
			t.Errorf("should not be active before valid_from")
		}
	})
	t.Run("at valid_until is inactive (right-open)", func(t *testing.T) {
		c := base()
		if c.IsActive(until) {
			t.Errorf("valid_until should be exclusive")
		}
	})
	t.Run("after valid_until", func(t *testing.T) {
		c := base()
		if c.IsActive(after) {
			t.Errorf("should not be active after valid_until")
		}
	})
	t.Run("enabled false", func(t *testing.T) {
		c := base()
		c.Enabled = false
		if c.IsActive(inside) {
			t.Errorf("disabled config should not be active")
		}
	})
	t.Run("no_trade.enabled true", func(t *testing.T) {
		c := base()
		c.NoTrade.Enabled = true
		if c.IsActive(inside) {
			t.Errorf("no_trade.enabled should disable")
		}
	})
	t.Run("strategy.name = no_trade", func(t *testing.T) {
		c := base()
		c.Strategy.Name = StrategyNoTrade
		if c.IsActive(inside) {
			t.Errorf("strategy.name=no_trade should disable")
		}
	})
}

func TestEntrySection_IsHourAllowed(t *testing.T) {
	// 09:00 UTC = 18:00 JST. 00:00 UTC = 09:00 JST.
	jstHour18 := time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC)
	jstHour9 := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	jstHour13 := time.Date(2026, 5, 19, 4, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		allowed []int
		t       time.Time
		want    bool
	}{
		{"empty list = all allowed (back-compat)", nil, jstHour18, true},
		{"empty slice = all allowed", []int{}, jstHour13, true},
		{"hour 18 in [17,18,19]", []int{17, 18, 19}, jstHour18, true},
		{"hour 13 not in [17,18,19]", []int{17, 18, 19}, jstHour13, false},
		{"hour 9 in [0,2,8,9]", []int{0, 2, 8, 9}, jstHour9, true},
		{"single-element match", []int{18}, jstHour18, true},
		{"single-element miss", []int{18}, jstHour13, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := EntrySection{AllowedHoursJST: tc.allowed}
			if got := e.IsHourAllowed(tc.t); got != tc.want {
				t.Errorf("IsHourAllowed(%v) with allowed=%v: got %v want %v",
					tc.t, tc.allowed, got, tc.want)
			}
		})
	}
}

func TestStrategyName_Valid(t *testing.T) {
	cases := map[StrategyName]bool{
		StrategyMomentumPullback:   true,
		StrategyBreakoutFollow:     true,
		StrategyRangeBreakoutProbe: true,
		StrategyMTFPullback:        true,
		StrategyMAPullback:         true,
		StrategyNoTrade:            true,
		"range_reversion":          false, // removed in day-trading migration
		"":                         false,
		"random":                   false,
	}
	for s, want := range cases {
		if got := s.Valid(); got != want {
			t.Errorf("StrategyName(%q).Valid() = %v, want %v", s, got, want)
		}
	}
}

func TestDirection_Valid(t *testing.T) {
	cases := map[Direction]bool{
		DirectionBuyOnly:  true,
		DirectionSellOnly: true,
		DirectionBoth:     true,
		DirectionNone:     true,
		"":                false,
		"bidirectional":   false,
	}
	for d, want := range cases {
		if got := d.Valid(); got != want {
			t.Errorf("Direction(%q).Valid() = %v, want %v", d, got, want)
		}
	}
}

func TestMarketRegimeType_Valid(t *testing.T) {
	cases := map[MarketRegimeType]bool{
		RegimeRange:     true,
		RegimeTrendUp:   true,
		RegimeTrendDown: true,
		RegimeVolatile:  true,
		RegimeUnclear:   true,
		"":              false,
		"bullish":       false,
	}
	for r, want := range cases {
		if got := r.Valid(); got != want {
			t.Errorf("MarketRegimeType(%q).Valid() = %v, want %v", r, got, want)
		}
	}
}
