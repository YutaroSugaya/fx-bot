package strategy

import (
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
)

func baseConfig(name config.StrategyName, dir config.Direction, now time.Time) *config.StrategyConfig {
	c := &config.StrategyConfig{
		ConfigID:    "test-cfg",
		GeneratedAt: now,
		ValidFrom:   now,
		ValidUntil:  now.Add(60 * time.Minute),
		Symbol:      "USD_JPY",
		Enabled:     true,
		Strategy:    config.StrategySection{Name: name},
		Entry: config.EntrySection{
			MaxSpreadPips: 5.0, // generous for tests
			Direction:     dir,
		},
		Exit: config.ExitSection{
			TakeProfitPips: 2.0, StopLossPips: 2.5, MaxHoldMinutes: 20,
		},
		Risk: config.ConfigRiskSection{
			Quantity: 100, MaxOpenPositions: 1, MaxTradesInThisWindow: 3, MaxLossInThisWindowJPY: 300,
		},
	}
	if name == config.StrategyNoTrade {
		c.Enabled = false
		c.Entry.Direction = config.DirectionNone
		c.Exit = config.ExitSection{}
		c.Risk.Quantity = 0
		c.NoTrade = config.NoTradeSection{Enabled: true, Reason: "test"}
	}
	return c
}

func mkSummary(bid, ask, hi, lo float64, trend string) *market.MarketSummary {
	w := market.WindowSummary{
		High: hi, Low: lo, RangePips: (hi - lo) / 0.01, TrendDirection: trend, NumCandles: 10,
		Support: lo, Resistance: hi,
	}
	return &market.MarketSummary{
		Symbol:      "USD_JPY",
		CurrentRate: market.CurrentRate{Bid: bid, Ask: ask, SpreadPips: (ask - bid) / 0.01},
		// Day-trading strategies read Summary6h as the trend axis; populate both so
		// existing tests (which historically only set 1h) keep working.
		Summary1h: w,
		Summary6h: w,
	}
}

func TestEngine_Dispatch_NoTrade(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyNoTrade, config.DirectionNone, now)
	sig := e.Evaluate(EvalInput{Now: now, Config: cfg, Summary: mkSummary(150.10, 150.13, 150.20, 150.05, "flat")})
	if sig.Decision != DecisionNoTrade {
		t.Errorf("expected NO_TRADE, got %s reason=%s", sig.Decision, sig.Reason)
	}
}

func TestEngine_NilConfig(t *testing.T) {
	e := NewEngine()
	sig := e.Evaluate(EvalInput{Now: time.Now()})
	if sig.Decision != DecisionNone || sig.Reason != "no_active_config" {
		t.Errorf("got %s, %s", sig.Decision, sig.Reason)
	}
}

func TestEngine_ConfigExpired_ReturnsNoTrade(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyMomentumPullback, config.DirectionBoth, now)
	later := cfg.ValidUntil.Add(time.Minute)
	sig := e.Evaluate(EvalInput{Now: later, Config: cfg, Summary: mkSummary(150.10, 150.13, 150.20, 150.05, "flat")})
	if sig.Decision != DecisionNoTrade {
		t.Errorf("expected NO_TRADE for expired, got %s reason=%s", sig.Decision, sig.Reason)
	}
}

func TestEngine_AllowedHoursJST_BlocksOutsideAndAllowsInside(t *testing.T) {
	e := NewEngine()
	// 18:00 JST = 09:00 UTC (May 15 weekday, FX active)
	jst18 := time.Date(2026, 5, 15, 9, 0, 0, 0, time.UTC)
	// 13:00 JST = 04:00 UTC (Tokyo afternoon — outside the allowed hours)
	jst13 := time.Date(2026, 5, 15, 4, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		now       time.Time
		allowed   []int
		wantBlock bool
	}{
		{"empty whitelist = all hours allowed", jst13, nil, false},
		{"hour 18 inside [17,18,19]", jst18, []int{17, 18, 19}, false},
		{"hour 13 outside [17,18,19] = blocked", jst13, []int{17, 18, 19}, true},
		{"hour 13 inside [13] = allowed", jst13, []int{13}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig(config.StrategyMomentumPullback, config.DirectionBoth, tc.now)
			cfg.ValidFrom = tc.now.Add(-time.Hour)
			cfg.ValidUntil = tc.now.Add(time.Hour)
			cfg.Entry.AllowedHoursJST = tc.allowed
			sig := e.Evaluate(EvalInput{
				Now: tc.now, Config: cfg,
				Summary: mkSummary(150.10, 150.13, 150.20, 150.05, "flat"),
			})
			gotBlocked := sig.Decision == DecisionNoTrade && sig.Reason == "outside_allowed_hours_jst"
			if gotBlocked != tc.wantBlock {
				t.Errorf("got Decision=%s Reason=%q; wantBlock=%v", sig.Decision, sig.Reason, tc.wantBlock)
			}
		})
	}
}

// momentum_pullback ---------------------------------------------------------

func TestMomentumPullback_TrendUpWithBearishLastCandle_Enters(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyMomentumPullback, config.DirectionBoth, now)
	candles := []market.Candle{
		{Open: 150.10, Close: 150.15, OpenTime: now.Add(-15 * time.Minute)},
		{Open: 150.15, Close: 150.20, OpenTime: now.Add(-10 * time.Minute)},
		{Open: 150.20, Close: 150.18, OpenTime: now.Add(-5 * time.Minute)}, // pullback
	}
	sig := e.Evaluate(EvalInput{
		Now: now, Config: cfg, Candles5m: candles,
		Summary: mkSummary(150.18, 150.19, 150.25, 150.10, "up"),
	})
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Errorf("expected BUY entry on uptrend pullback, got %+v", sig)
	}
}

func TestMomentumPullback_TrendUpWithoutPullback_NoEntry(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyMomentumPullback, config.DirectionBoth, now)
	candles := []market.Candle{
		{Open: 150.10, Close: 150.15, OpenTime: now.Add(-15 * time.Minute)},
		{Open: 150.15, Close: 150.20, OpenTime: now.Add(-10 * time.Minute)},
		{Open: 150.20, Close: 150.22, OpenTime: now.Add(-5 * time.Minute)}, // still up
	}
	sig := e.Evaluate(EvalInput{
		Now: now, Config: cfg, Candles5m: candles,
		Summary: mkSummary(150.22, 150.23, 150.25, 150.10, "up"),
	})
	if sig.IsEntry() {
		t.Errorf("expected no entry without pullback, got %+v", sig)
	}
}

func TestMomentumPullback_FlatTrend_NoEntry(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyMomentumPullback, config.DirectionBoth, now)
	sig := e.Evaluate(EvalInput{
		Now: now, Config: cfg,
		Candles5m: []market.Candle{
			{Open: 150.10, Close: 150.11, OpenTime: now.Add(-10 * time.Minute)},
			{Open: 150.11, Close: 150.10, OpenTime: now.Add(-5 * time.Minute)},
		},
		Summary: mkSummary(150.10, 150.11, 150.12, 150.09, "flat"),
	})
	if sig.IsEntry() {
		t.Errorf("expected no entry on flat trend, got %+v", sig)
	}
}

// 追いかけ (chasing) 防止フィルタ -------------------------------------------
//
// momentum_pullback で「もう走り切った後」= 直近のベースから離れすぎた所での
// 順張りを抑止する(大きく走った後の天井買いを防ぐ)。MaxChasePips / ChaseLookbackCandles が両方 > 0 のときだけ有効。

func TestMomentumPullback_ChasingExtended_NoEntry(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyMomentumPullback, config.DirectionBoth, now)
	cfg.Entry.MaxChasePips = 12
	cfg.Entry.ChaseLookbackCandles = 3
	// 上昇で +19pips 走った後の浅い押し目。entry(ask)=150.19, 直近 3 本の
	// 最安値=150.00 → extension=19pips > 12 → 追いかけと判定して見送り。
	candles := []market.Candle{
		{Open: 150.00, Close: 150.10, Low: 150.00, High: 150.10, OpenTime: now.Add(-15 * time.Minute)},
		{Open: 150.10, Close: 150.20, Low: 150.10, High: 150.20, OpenTime: now.Add(-10 * time.Minute)},
		{Open: 150.20, Close: 150.18, Low: 150.17, High: 150.21, OpenTime: now.Add(-5 * time.Minute)}, // pullback
	}
	sig := e.Evaluate(EvalInput{
		Now: now, Config: cfg, Candles5m: candles,
		Summary: mkSummary(150.18, 150.19, 150.25, 150.00, "up"),
	})
	if sig.IsEntry() {
		t.Errorf("expected no entry (chasing_extended), got %+v", sig)
	}
	if sig.Reason != "chasing_extended" {
		t.Errorf("expected reason chasing_extended, got %q", sig.Reason)
	}
}

func TestMomentumPullback_ShallowEntry_StillEnters(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyMomentumPullback, config.DirectionBoth, now)
	cfg.Entry.MaxChasePips = 12
	cfg.Entry.ChaseLookbackCandles = 3
	// ベース付近での浅いエントリー。entry(ask)=150.19, 直近最安値=150.14 →
	// extension=5pips < 12 → フィルタを通過して通常通り BUY。
	candles := []market.Candle{
		{Open: 150.15, Close: 150.17, Low: 150.14, High: 150.18, OpenTime: now.Add(-15 * time.Minute)},
		{Open: 150.17, Close: 150.19, Low: 150.16, High: 150.20, OpenTime: now.Add(-10 * time.Minute)},
		{Open: 150.19, Close: 150.18, Low: 150.175, High: 150.195, OpenTime: now.Add(-5 * time.Minute)}, // pullback
	}
	sig := e.Evaluate(EvalInput{
		Now: now, Config: cfg, Candles5m: candles,
		Summary: mkSummary(150.18, 150.19, 150.25, 150.14, "up"),
	})
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Errorf("expected BUY entry (shallow, not chasing), got %+v reason=%q", sig, sig.Reason)
	}
}

// breakout_follow -----------------------------------------------------------

func TestBreakoutFollow_AboveRange_BUY(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyBreakoutFollow, config.DirectionBoth, now)
	cfg.Entry.RequireBreakout = true
	// ask 150.35 > hi 150.30 + cushion 0.03 → BUY breakout
	sig := e.Evaluate(EvalInput{Now: now, Config: cfg, Summary: mkSummary(150.34, 150.35, 150.30, 150.05, "up")})
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Errorf("expected BUY breakout, got %+v", sig)
	}
}

func TestBreakoutFollow_BelowRange_SELL(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyBreakoutFollow, config.DirectionBoth, now)
	cfg.Entry.RequireBreakout = true
	// bid 150.01 < lo 150.05 - cushion 0.03 → SELL breakout
	sig := e.Evaluate(EvalInput{Now: now, Config: cfg, Summary: mkSummary(150.01, 150.02, 150.30, 150.05, "down")})
	if !sig.IsEntry() || sig.Side != order.SideSell {
		t.Errorf("expected SELL breakout, got %+v", sig)
	}
}

func TestBreakoutFollow_InsideRange_NoEntry(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyBreakoutFollow, config.DirectionBoth, now)
	cfg.Entry.RequireBreakout = true
	sig := e.Evaluate(EvalInput{Now: now, Config: cfg, Summary: mkSummary(150.15, 150.16, 150.30, 150.05, "flat")})
	if sig.IsEntry() {
		t.Errorf("expected no entry inside range, got %+v", sig)
	}
}

func TestBreakoutFollow_RequireBreakoutFalse_NoEntry(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyBreakoutFollow, config.DirectionBoth, now)
	cfg.Entry.RequireBreakout = false
	sig := e.Evaluate(EvalInput{Now: now, Config: cfg, Summary: mkSummary(150.34, 150.35, 150.30, 150.05, "up")})
	if sig.IsEntry() {
		t.Errorf("expected no entry when require_breakout=false, got %+v", sig)
	}
}

// range_breakout_probe ------------------------------------------------------

func TestRangeBreakoutProbe_NearResistance_BUY(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyRangeBreakoutProbe, config.DirectionBoth, now)
	cfg.Entry.RequireBreakout = false
	sig := e.Evaluate(EvalInput{Now: now, Config: cfg, Summary: mkSummary(150.285, 150.290, 150.30, 150.15, "flat")})
	if !sig.IsEntry() || sig.Side != order.SideBuy {
		t.Errorf("expected BUY probe near resistance, got %+v", sig)
	}
}

func TestRangeBreakoutProbe_NearSupport_SELL(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyRangeBreakoutProbe, config.DirectionBoth, now)
	cfg.Entry.RequireBreakout = false
	sig := e.Evaluate(EvalInput{Now: now, Config: cfg, Summary: mkSummary(150.160, 150.165, 150.30, 150.15, "flat")})
	if !sig.IsEntry() || sig.Side != order.SideSell {
		t.Errorf("expected SELL probe near support, got %+v", sig)
	}
}

func TestRangeBreakoutProbe_MiddleOfRange_NoEntry(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyRangeBreakoutProbe, config.DirectionBoth, now)
	cfg.Entry.RequireBreakout = false
	sig := e.Evaluate(EvalInput{Now: now, Config: cfg, Summary: mkSummary(150.220, 150.225, 150.30, 150.15, "flat")})
	if sig.IsEntry() || sig.Reason != "inside_range_not_near_edge" {
		t.Errorf("expected no probe in middle of range, got %+v", sig)
	}
}

func TestRangeBreakoutProbe_AlreadyBrokenOut_NoEntry(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyRangeBreakoutProbe, config.DirectionBoth, now)
	cfg.Entry.RequireBreakout = false
	sig := e.Evaluate(EvalInput{Now: now, Config: cfg, Summary: mkSummary(150.315, 150.320, 150.30, 150.15, "flat")})
	if sig.IsEntry() || sig.Reason != "already_broken_out" {
		t.Errorf("expected no probe after breakout, got %+v", sig)
	}
}

func TestRangeBreakoutProbe_RequireBreakoutTrue_NoEntry(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyRangeBreakoutProbe, config.DirectionBoth, now)
	cfg.Entry.RequireBreakout = true
	sig := e.Evaluate(EvalInput{Now: now, Config: cfg, Summary: mkSummary(150.285, 150.290, 150.30, 150.15, "flat")})
	if sig.IsEntry() || sig.Reason != "require_breakout=true" {
		t.Errorf("expected no entry when require_breakout=true, got %+v", sig)
	}
}

// quantity propagation -------------------------------------------------------
//
// Entry signals must carry the order size from the active config's
// risk.quantity. ExecuteOrder.defaultQty() reads sig.Quantity directly
// (no fallback) so a strategy that forgets to populate it would emit a
// zero-quantity signal — which OnSignal then refuses. These tests pin the
// strategy → signal step.

func TestMomentumPullback_PropagatesQuantityFromConfig(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyMomentumPullback, config.DirectionBoth, now)
	cfg.Risk.Quantity = 7000 // not 100 — pin that the strategy uses *this* value
	candles := []market.Candle{
		{Open: 150.10, Close: 150.15, OpenTime: now.Add(-15 * time.Minute)},
		{Open: 150.15, Close: 150.20, OpenTime: now.Add(-10 * time.Minute)},
		{Open: 150.20, Close: 150.18, OpenTime: now.Add(-5 * time.Minute)},
	}
	sig := e.Evaluate(EvalInput{
		Now: now, Config: cfg, Candles5m: candles,
		Summary: mkSummary(150.18, 150.19, 150.25, 150.10, "up"),
	})
	if !sig.IsEntry() {
		t.Fatalf("expected entry, got %+v", sig)
	}
	if sig.Quantity != 7000 {
		t.Errorf("expected Quantity=7000 from config.Risk.Quantity, got %d", sig.Quantity)
	}
}

func TestBreakoutFollow_PropagatesQuantityFromConfig(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyBreakoutFollow, config.DirectionBoth, now)
	cfg.Entry.RequireBreakout = true
	cfg.Risk.Quantity = 3500
	sig := e.Evaluate(EvalInput{Now: now, Config: cfg, Summary: mkSummary(150.34, 150.35, 150.30, 150.05, "up")})
	if !sig.IsEntry() {
		t.Fatalf("expected entry, got %+v", sig)
	}
	if sig.Quantity != 3500 {
		t.Errorf("expected Quantity=3500 from config.Risk.Quantity, got %d", sig.Quantity)
	}
}

func TestRangeBreakoutProbe_PropagatesQuantityFromConfig(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyRangeBreakoutProbe, config.DirectionBoth, now)
	cfg.Risk.Quantity = 2500
	sig := e.Evaluate(EvalInput{Now: now, Config: cfg, Summary: mkSummary(150.285, 150.290, 150.30, 150.15, "flat")})
	if !sig.IsEntry() {
		t.Fatalf("expected entry, got %+v", sig)
	}
	if sig.Quantity != 2500 {
		t.Errorf("expected Quantity=2500 from config.Risk.Quantity, got %d", sig.Quantity)
	}
}

// Early-exit window propagation. Both strategies must copy
// EarlyExitWindowMinutes / EarlyExitTargetPips from config.Exit so the
// signal carries them all the way to ExecuteOrder → PositionRecord →
// evaluateExit. Without this propagation, evaluateExit's early-exit
// branch can never fire (fields are 0 = feature off).

func TestMomentumPullback_PropagatesEarlyExitFromConfig(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyMomentumPullback, config.DirectionBoth, now)
	cfg.Exit.EarlyExitWindowMinutes = 30
	cfg.Exit.EarlyExitTargetPips = -2.0
	candles := []market.Candle{
		{Open: 150.10, Close: 150.15, OpenTime: now.Add(-15 * time.Minute)},
		{Open: 150.15, Close: 150.20, OpenTime: now.Add(-10 * time.Minute)},
		{Open: 150.20, Close: 150.18, OpenTime: now.Add(-5 * time.Minute)},
	}
	sig := e.Evaluate(EvalInput{
		Now: now, Config: cfg, Candles5m: candles,
		Summary: mkSummary(150.18, 150.19, 150.25, 150.10, "up"),
	})
	if !sig.IsEntry() {
		t.Fatalf("expected entry, got %+v", sig)
	}
	if sig.EarlyExitWindowMinutes != 30 {
		t.Errorf("expected EarlyExitWindowMinutes=30, got %d", sig.EarlyExitWindowMinutes)
	}
	if sig.EarlyExitTargetPips != -2.0 {
		t.Errorf("expected EarlyExitTargetPips=-2.0, got %v", sig.EarlyExitTargetPips)
	}
}

func TestBreakoutFollow_PropagatesEarlyExitFromConfig(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := baseConfig(config.StrategyBreakoutFollow, config.DirectionBoth, now)
	cfg.Entry.RequireBreakout = true
	cfg.Exit.EarlyExitWindowMinutes = 45
	cfg.Exit.EarlyExitTargetPips = -3.5
	sig := e.Evaluate(EvalInput{Now: now, Config: cfg, Summary: mkSummary(150.34, 150.35, 150.30, 150.05, "up")})
	if !sig.IsEntry() {
		t.Fatalf("expected entry, got %+v", sig)
	}
	if sig.EarlyExitWindowMinutes != 45 {
		t.Errorf("expected EarlyExitWindowMinutes=45, got %d", sig.EarlyExitWindowMinutes)
	}
	if sig.EarlyExitTargetPips != -3.5 {
		t.Errorf("expected EarlyExitTargetPips=-3.5, got %v", sig.EarlyExitTargetPips)
	}
}
