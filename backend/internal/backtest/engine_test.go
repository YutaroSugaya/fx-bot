package backtest

import (
	"context"
	"math"
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
)

// alwaysBuyStrategy is a deterministic test fixture that ignores market
// conditions and emits a BUY signal on every Evaluate call. The Engine's
// max_open_positions cap (= 1 in tests) prevents repeated entries while a
// position is open, so this is safe to register without flooding trades.
type alwaysBuyStrategy struct{}

func (alwaysBuyStrategy) Name() config.StrategyName { return "test_always_buy" }
func (alwaysBuyStrategy) Evaluate(in strategy.EvalInput) strategy.Signal {
	entry := 0.0
	if in.Summary != nil {
		entry = in.Summary.CurrentRate.Ask
	}
	return strategy.Signal{
		Decision:       strategy.DecisionEnter,
		Side:           order.SideBuy,
		EntryPrice:     entry,
		TakeProfitPips: in.Config.Exit.TakeProfitPips,
		StopLossPips:   in.Config.Exit.StopLossPips,
		MaxHoldMinutes: in.Config.Exit.MaxHoldMinutes,
		ConfigID:       in.Config.ConfigID,
		StrategyName:   "test_always_buy",
		Reason:         "test always buy",
		CreatedAt:      in.Now,
	}
}

// buyConfig returns a config that authorises BUY-only trades with the given
// TP/SL/MaxHold. Strategy.Name is "test_always_buy" so the engine dispatches
// to alwaysBuyStrategy after we register it.
func buyConfig(now time.Time, tpPips, slPips float64, maxHold int) *config.StrategyConfig {
	return &config.StrategyConfig{
		ConfigID:   "test-buy",
		Symbol:     "USD_JPY",
		Enabled:    true,
		ValidFrom:  now.Add(-time.Hour),
		ValidUntil: now.Add(24 * time.Hour),
		Strategy: config.StrategySection{
			Name: "test_always_buy",
		},
		Entry: config.EntrySection{
			Direction:     config.DirectionBuyOnly,
			MaxSpreadPips: 5.0,
		},
		Exit: config.ExitSection{
			TakeProfitPips: tpPips,
			StopLossPips:   slPips,
			MaxHoldMinutes: maxHold,
		},
		Risk: config.ConfigRiskSection{
			Quantity:         100,
			MaxOpenPositions: 1,
		},
	}
}

// noTradeConfig returns a StrategyConfig that the engine will treat as
// "do not trade for any reason" — IsActive() returns false so engine.Evaluate
// short-circuits with DecisionNoTrade.
func noTradeConfig(now time.Time) *config.StrategyConfig {
	return &config.StrategyConfig{
		ConfigID:   "test-no-trade",
		Symbol:     "USD_JPY",
		Enabled:    false,
		ValidFrom:  now.Add(-time.Hour),
		ValidUntil: now.Add(time.Hour),
		Strategy: config.StrategySection{
			Name: config.StrategyNoTrade,
		},
		Entry:   config.EntrySection{Direction: config.DirectionNone},
		Exit:    config.ExitSection{},
		Risk:    config.ConfigRiskSection{Quantity: 0, MaxOpenPositions: 1},
		NoTrade: config.NoTradeSection{Enabled: true, Reason: "backtest no-trade fixture"},
	}
}

// flatCandles generates `n` 1-minute candles that all sit at the same price,
// with OpenTime striding by 1 minute starting at start.
func flatCandles(n int, price float64, start time.Time) []market.Candle {
	out := make([]market.Candle, n)
	for i := 0; i < n; i++ {
		out[i] = market.Candle{
			Symbol:   "USD_JPY",
			Interval: time.Minute,
			OpenTime: start.Add(time.Duration(i) * time.Minute),
			Open:     price,
			High:     price,
			Low:      price,
			Close:    price,
		}
	}
	return out
}

// TestEngine_Replay_NoSignal_ProducesEmptyResult is the smallest possible
// drive-loop test: feed flat candles to a no_trade config and verify the
// engine completes without opening any position.
func TestEngine_Replay_NoSignal_ProducesEmptyResult(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	candles := flatCandles(30, 150.00, start)

	e := NewEngine(EngineConfig{
		Symbol:         "USD_JPY",
		StrategyConfig: noTradeConfig(start),
	})
	result, err := e.Replay(ctx, candles)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(result.Trades) != 0 {
		t.Errorf("expected 0 trades, got %d: %+v", len(result.Trades), result.Trades)
	}
	if result.Metrics.SampleSize != 0 {
		t.Errorf("Metrics.SampleSize: %d want 0", result.Metrics.SampleSize)
	}
}

// TestEngine_Replay_SingleTPHit_RecordsWinningTrade is the happy-path
// integration test: BUY at bar 0, TP hits bar 2's High, position closes,
// metrics show 1 winner.
//
// Scenario (USD/JPY, 1 pip = 0.01):
//
//	Bar 0: open=150.00 high=150.05 low=149.98 close=150.02 → BUY @ ~150.02
//	Bar 1: open=150.02 high=150.10 low=150.00 close=150.08 (drifting up)
//	Bar 2: open=150.08 high=150.40 low=150.06 close=150.30 (High touches TP 150.32)
//
// TP_pips=30 → entry 150.02 + 0.30 = 150.32; Bar 2 High 150.40 > TP → fire.
func TestEngine_Replay_SingleTPHit_RecordsWinningTrade(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	cfg := buyConfig(start, 30 /*TP*/, 20 /*SL*/, 60 /*MaxHold*/)

	candles := []market.Candle{
		{Symbol: "USD_JPY", Interval: time.Minute, OpenTime: start.Add(0 * time.Minute), Open: 150.00, High: 150.05, Low: 149.98, Close: 150.02},
		{Symbol: "USD_JPY", Interval: time.Minute, OpenTime: start.Add(1 * time.Minute), Open: 150.02, High: 150.10, Low: 150.00, Close: 150.08},
		{Symbol: "USD_JPY", Interval: time.Minute, OpenTime: start.Add(2 * time.Minute), Open: 150.08, High: 150.40, Low: 150.06, Close: 150.30},
	}

	e := NewEngine(EngineConfig{Symbol: "USD_JPY", StrategyConfig: cfg})
	e.Strategies.Register(alwaysBuyStrategy{})

	result, err := e.Replay(ctx, candles)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(result.Trades) != 1 {
		t.Fatalf("expected 1 trade, got %d: %+v", len(result.Trades), result.Trades)
	}
	tr := result.Trades[0]
	if tr.CloseReason != "take_profit" {
		t.Errorf("CloseReason: got %q want take_profit", tr.CloseReason)
	}
	if tr.Side != string(order.SideBuy) {
		t.Errorf("Side: got %q want BUY", tr.Side)
	}
	// Entry should be bar 0 close (~150.02), exit at TP level (~150.32).
	// Allow some tolerance for rounding / spread / slippage (0 by default but be lenient).
	wantPips := 30.0
	if math.Abs(tr.ProfitLossPips-wantPips) > 1.0 {
		t.Errorf("ProfitLossPips: got %v want ~%v", tr.ProfitLossPips, wantPips)
	}
	if tr.ProfitLossJPY <= 0 {
		t.Errorf("ProfitLossJPY: got %v, expected positive", tr.ProfitLossJPY)
	}
	if !tr.ClosedAt.After(tr.OpenedAt) {
		t.Errorf("ClosedAt %v not after OpenedAt %v", tr.ClosedAt, tr.OpenedAt)
	}
	if result.Metrics.SampleSize != 1 || result.Metrics.WinRate != 1.0 {
		t.Errorf("Metrics: got %+v", result.Metrics)
	}
}

// TestEngine_Replay_SingleSLHit_RecordsLosingTrade is symmetric to TP hit:
// BUY at bar 0, then bar 1 Low touches SL → losing trade.
//
// SL_pips=20 → entry 150.02 - 0.20 = 149.82; Bar 1 Low 149.80 < SL → fire.
func TestEngine_Replay_SingleSLHit_RecordsLosingTrade(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	cfg := buyConfig(start, 30 /*TP*/, 20 /*SL*/, 60 /*MaxHold*/)

	candles := []market.Candle{
		{Symbol: "USD_JPY", Interval: time.Minute, OpenTime: start.Add(0 * time.Minute), Open: 150.00, High: 150.05, Low: 149.98, Close: 150.02},
		{Symbol: "USD_JPY", Interval: time.Minute, OpenTime: start.Add(1 * time.Minute), Open: 150.02, High: 150.03, Low: 149.80, Close: 149.85},
	}

	e := NewEngine(EngineConfig{Symbol: "USD_JPY", StrategyConfig: cfg})
	e.Strategies.Register(alwaysBuyStrategy{})

	result, err := e.Replay(ctx, candles)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(result.Trades) != 1 {
		t.Fatalf("expected 1 trade, got %d", len(result.Trades))
	}
	tr := result.Trades[0]
	if tr.CloseReason != "stop_loss" {
		t.Errorf("CloseReason: got %q want stop_loss", tr.CloseReason)
	}
	if tr.ProfitLossJPY >= 0 {
		t.Errorf("ProfitLossJPY: got %v, expected negative", tr.ProfitLossJPY)
	}
	wantPips := -20.0
	if math.Abs(tr.ProfitLossPips-wantPips) > 1.0 {
		t.Errorf("ProfitLossPips: got %v want ~%v", tr.ProfitLossPips, wantPips)
	}
	if result.Metrics.WinRate != 0 {
		t.Errorf("WinRate: got %v want 0", result.Metrics.WinRate)
	}
}

// TestEngine_Replay_BarHitsBothTPandSL_AppliesPessimisticPolicy is the
// conflict-resolution test: a single bar's High touches TP and Low touches SL.
// Default PessimisticSLFirst → SL fires; AmbiguousBars increments.
func TestEngine_Replay_BarHitsBothTPandSL_AppliesPessimisticPolicy(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	cfg := buyConfig(start, 30 /*TP*/, 20 /*SL*/, 60 /*MaxHold*/)

	// Bar 0: entry @ 150.02. Bar 1: huge range — High 150.40 (>= TP 150.32)
	// AND Low 149.80 (<= SL 149.82). Default policy resolves to SL.
	candles := []market.Candle{
		{Symbol: "USD_JPY", Interval: time.Minute, OpenTime: start.Add(0 * time.Minute), Open: 150.00, High: 150.05, Low: 149.98, Close: 150.02},
		{Symbol: "USD_JPY", Interval: time.Minute, OpenTime: start.Add(1 * time.Minute), Open: 150.02, High: 150.40, Low: 149.80, Close: 150.10},
	}

	t.Run("default_pessimistic", func(t *testing.T) {
		e := NewEngine(EngineConfig{Symbol: "USD_JPY", StrategyConfig: cfg})
		e.Strategies.Register(alwaysBuyStrategy{})
		result, _ := e.Replay(ctx, candles)
		if len(result.Trades) != 1 || result.Trades[0].CloseReason != "stop_loss" {
			t.Fatalf("expected SL exit by default, got: %+v", result.Trades)
		}
		if result.AmbiguousBars != 1 {
			t.Errorf("AmbiguousBars: got %d want 1", result.AmbiguousBars)
		}
	})

	t.Run("optimistic_tp_first", func(t *testing.T) {
		e := NewEngine(EngineConfig{
			Symbol: "USD_JPY", StrategyConfig: cfg,
			ConflictPolicy: OptimisticTPFirst,
		})
		e.Strategies.Register(alwaysBuyStrategy{})
		result, _ := e.Replay(ctx, candles)
		if len(result.Trades) != 1 || result.Trades[0].CloseReason != "take_profit" {
			t.Fatalf("expected TP exit under optimistic policy, got: %+v", result.Trades)
		}
	})

	t.Run("skip_ambiguous_defers_to_next_bar", func(t *testing.T) {
		// Add a clean TP bar at bar 2 so the position eventually exits.
		extended := append([]market.Candle{}, candles...)
		extended = append(extended, market.Candle{
			Symbol: "USD_JPY", Interval: time.Minute,
			OpenTime: start.Add(2 * time.Minute),
			Open:     150.10, High: 150.40, Low: 150.10, Close: 150.35,
		})
		e := NewEngine(EngineConfig{
			Symbol: "USD_JPY", StrategyConfig: cfg,
			ConflictPolicy: SkipAmbiguous,
		})
		e.Strategies.Register(alwaysBuyStrategy{})
		result, _ := e.Replay(ctx, extended)
		if len(result.Trades) != 1 || result.Trades[0].CloseReason != "take_profit" {
			t.Fatalf("expected deferred TP exit, got: %+v", result.Trades)
		}
		if result.AmbiguousBars != 1 {
			t.Errorf("AmbiguousBars: got %d want 1", result.AmbiguousBars)
		}
	})
}

// TestEngine_Replay_MaxHoldExceeded_ExitsAtBarClose: entry at bar 0, neither
// TP nor SL ever hit, position closes at bar N's close once now >= openedAt +
// MaxHold. CloseReason is "max_hold" and exit price equals that bar's Close.
func TestEngine_Replay_MaxHoldExceeded_ExitsAtBarClose(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	cfg := buyConfig(start, 30 /*TP*/, 20 /*SL*/, 2 /*MaxHold=2min*/)

	// Bar 0: entry @ close 150.02. Bar 1: tight range, no TP/SL. Bar 2: same.
	// At bar 2 close (= start+3min) we're past the MaxHold deadline
	// (entry now = start+1min, deadline = start+3min). Should exit at 150.07.
	candles := []market.Candle{
		{Symbol: "USD_JPY", Interval: time.Minute, OpenTime: start.Add(0 * time.Minute), Open: 150.00, High: 150.05, Low: 149.98, Close: 150.02},
		{Symbol: "USD_JPY", Interval: time.Minute, OpenTime: start.Add(1 * time.Minute), Open: 150.02, High: 150.06, Low: 149.95, Close: 150.04},
		{Symbol: "USD_JPY", Interval: time.Minute, OpenTime: start.Add(2 * time.Minute), Open: 150.04, High: 150.08, Low: 150.00, Close: 150.07},
	}

	e := NewEngine(EngineConfig{Symbol: "USD_JPY", StrategyConfig: cfg})
	e.Strategies.Register(alwaysBuyStrategy{})

	result, err := e.Replay(ctx, candles)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(result.Trades) != 1 {
		t.Fatalf("expected 1 trade, got %d: %+v", len(result.Trades), result.Trades)
	}
	tr := result.Trades[0]
	if tr.CloseReason != "max_hold" {
		t.Errorf("CloseReason: got %q want max_hold", tr.CloseReason)
	}
	if math.Abs(tr.ExitPrice-150.07) > 1e-9 {
		t.Errorf("ExitPrice: got %v want 150.07 (bar close)", tr.ExitPrice)
	}
	// PnL: (150.07 - 150.02) / 0.01 = 5 pips, 5 * 0.01 * 100 = 5 JPY profit.
	if math.Abs(tr.ProfitLossPips-5.0) > 1.0 {
		t.Errorf("ProfitLossPips: got %v want ~5", tr.ProfitLossPips)
	}
}

// CostModel が entry/exit すべてに adverse 適用されること ----------

// TestEngine_Replay_Costs_SlippageAppliedToTPExit: TP hit でも exit price から
// slippage を引く (BUY なら exit -= slip)。これが効かないと backtest は
// 楽観的 PnL を出してしまう。
func TestEngine_Replay_Costs_SlippageAppliedToTPExit(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	cfg := buyConfig(start, 30 /*TP*/, 20 /*SL*/, 60 /*MaxHold*/)

	candles := []market.Candle{
		{Symbol: "USD_JPY", Interval: time.Minute, OpenTime: start.Add(0 * time.Minute), Open: 150.00, High: 150.05, Low: 149.98, Close: 150.02},
		{Symbol: "USD_JPY", Interval: time.Minute, OpenTime: start.Add(1 * time.Minute), Open: 150.02, High: 150.40, Low: 150.00, Close: 150.30},
	}

	// No-cost baseline: entry 150.02, TP 150.32 → +30 pips.
	eBase := NewEngine(EngineConfig{Symbol: "USD_JPY", StrategyConfig: cfg})
	eBase.Strategies.Register(alwaysBuyStrategy{})
	resBase, _ := eBase.Replay(ctx, candles)
	if len(resBase.Trades) != 1 || math.Abs(resBase.Trades[0].ProfitLossPips-30.0) > 0.01 {
		t.Fatalf("baseline expected ~30 pips, got %+v", resBase.Trades)
	}

	// With 2-pip slippage:
	//   entry = 150.02 + 0.02 = 150.04 (slipped fill — like Live MARKET entry)
	//   tpPrice = entry + 30 pips = 150.34 (TP anchored to actual fill, mirrors GMO behaviour)
	//   exit slipped = 150.34 - 0.02 = 150.32 (adverse on the close leg)
	//   net pips = (150.32 - 150.04) / 0.01 = 28
	// = baseline 30 minus 2 pips (one exit slip; entry slip is absorbed into the
	// anchor). This matches MARKET+OCO realised PnL in Live.
	eCost := NewEngine(EngineConfig{
		Symbol: "USD_JPY", StrategyConfig: cfg,
		Costs: CostModel{SlippagePips: 2.0},
	})
	eCost.Strategies.Register(alwaysBuyStrategy{})
	resCost, _ := eCost.Replay(ctx, candles)
	if len(resCost.Trades) != 1 {
		t.Fatalf("expected 1 trade, got %d", len(resCost.Trades))
	}
	wantPips := 28.0
	if math.Abs(resCost.Trades[0].ProfitLossPips-wantPips) > 0.01 {
		t.Errorf("ProfitLossPips with slip: got %v want %v (entry-anchored TP, exit slippage applied)",
			resCost.Trades[0].ProfitLossPips, wantPips)
	}
}

func TestEngine_Replay_Costs_SlippageAppliedToSLExit(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	cfg := buyConfig(start, 30, 20, 60)

	candles := []market.Candle{
		{Symbol: "USD_JPY", Interval: time.Minute, OpenTime: start.Add(0 * time.Minute), Open: 150.00, High: 150.05, Low: 149.98, Close: 150.02},
		{Symbol: "USD_JPY", Interval: time.Minute, OpenTime: start.Add(1 * time.Minute), Open: 150.02, High: 150.03, Low: 149.80, Close: 149.85},
	}

	// With 2-pip slip:
	//   entry = 150.02 + 0.02 = 150.04 (slipped fill)
	//   slPrice = entry - 20 pips = 149.84 (anchored to fill)
	//   exit slipped = 149.84 - 0.02 = 149.82 (adverse close)
	//   net pips = (149.82 - 150.04) / 0.01 = -22
	// = baseline -20 minus 2 pips (one exit slip).
	e := NewEngine(EngineConfig{
		Symbol: "USD_JPY", StrategyConfig: cfg,
		Costs: CostModel{SlippagePips: 2.0},
	})
	e.Strategies.Register(alwaysBuyStrategy{})
	res, _ := e.Replay(ctx, candles)
	if len(res.Trades) != 1 {
		t.Fatalf("expected 1 trade, got %d", len(res.Trades))
	}
	wantPips := -22.0
	if math.Abs(res.Trades[0].ProfitLossPips-wantPips) > 0.01 {
		t.Errorf("SL with slip: got %v want %v", res.Trades[0].ProfitLossPips, wantPips)
	}
}

func TestEngine_Replay_Costs_FeeReducesPnL(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	cfg := buyConfig(start, 30, 20, 60)

	candles := []market.Candle{
		{Symbol: "USD_JPY", Interval: time.Minute, OpenTime: start.Add(0 * time.Minute), Open: 150.00, High: 150.05, Low: 149.98, Close: 150.02},
		{Symbol: "USD_JPY", Interval: time.Minute, OpenTime: start.Add(1 * time.Minute), Open: 150.02, High: 150.40, Low: 150.00, Close: 150.30},
	}

	// TP @ +30 pips × 100 units = 30 JPY gross. Fee 10 JPY → net 20 JPY.
	e := NewEngine(EngineConfig{
		Symbol: "USD_JPY", StrategyConfig: cfg,
		Costs: CostModel{FeeJPYPerTrade: 10.0},
	})
	e.Strategies.Register(alwaysBuyStrategy{})
	res, _ := e.Replay(ctx, candles)
	if len(res.Trades) != 1 {
		t.Fatalf("expected 1 trade, got %d", len(res.Trades))
	}
	want := 20.0
	if math.Abs(res.Trades[0].ProfitLossJPY-want) > 0.01 {
		t.Errorf("ProfitLossJPY with fee: got %v want %v", res.Trades[0].ProfitLossJPY, want)
	}
}

// Backtest Mode A real-data wiring:
// production strategies (momentum_pullback,
// breakout_follow) read MarketSummary windows. With the stub summary they
// never trigger entries. The Engine must rebuild Summary from candle history
// at each bar so real backtests reflect actual signal generation.
//
// This test uses breakout_follow because it deterministically fires when
// current Ask > Summary6h.High + cushion — easier to construct than
// momentum_pullback's pullback detection.
func TestEngine_Replay_RebuildsSummaryForProductionStrategy(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)

	// Build 90 bars of a calm range 150.00-150.10, then 1 bar that breaks
	// strongly above the range high. Summary6h.High should be ~150.10 by then,
	// and the breakout bar's Close 150.20 exceeds it + cushion → entry.
	var candles []market.Candle
	for i := 0; i < 90; i++ {
		t := start.Add(time.Duration(i) * time.Minute)
		mid := 150.05 + float64(i%5)*0.005 // small oscillation, never above 150.075
		candles = append(candles, market.Candle{
			Symbol:   "USD_JPY",
			Interval: time.Minute,
			OpenTime: t,
			Open:     mid, High: mid + 0.02, Low: mid - 0.02, Close: mid,
		})
	}
	// Breakout bar.
	candles = append(candles, market.Candle{
		Symbol:   "USD_JPY",
		Interval: time.Minute,
		OpenTime: start.Add(90 * time.Minute),
		Open:     150.10, High: 150.25, Low: 150.10, Close: 150.20,
	})
	// Cool-off bars so the trade has room to TP later.
	for i := 91; i < 120; i++ {
		t := start.Add(time.Duration(i) * time.Minute)
		candles = append(candles, market.Candle{
			Symbol:   "USD_JPY",
			Interval: time.Minute,
			OpenTime: t,
			Open:     150.20, High: 150.60, Low: 150.20, Close: 150.55,
		})
	}

	cfg := buyConfig(start, 30 /*TP*/, 20 /*SL*/, 60 /*MaxHold*/)
	cfg.Strategy.Name = "breakout_follow"
	cfg.Entry.RequireBreakout = true

	e := NewEngine(EngineConfig{Symbol: "USD_JPY", StrategyConfig: cfg})
	// NB: do NOT register alwaysBuyStrategy — we want the production
	// breakout_follow to be invoked. NewEngine already registers it.
	res, err := e.Replay(ctx, candles)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(res.Trades) == 0 {
		t.Fatalf("expected at least 1 trade from breakout_follow + summary rebuild; got 0. " +
			"This means MarketSummary windows are still stubbed.")
	}
}
