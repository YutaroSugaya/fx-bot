package command

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/risk"
	"fx-bot/backend/internal/domain/strategy"
)

// Classical-school: real Evaluator + real Executor + real PaperBroker +
// InMemoryPositionRepo. Only the engine's strategy is swapped to a fixture
// that always-enters BUY when its mode field is set.

type alwaysBuy struct{}

func (alwaysBuy) Name() config.StrategyName { return config.StrategyMomentumPullback }
func (alwaysBuy) Evaluate(in strategy.EvalInput) strategy.Signal {
	return strategy.Signal{
		Decision: strategy.DecisionEnter, Side: order.SideBuy,
		EntryPrice: 100.10, TakeProfitPips: 10, StopLossPips: 10, MaxHoldMinutes: 5,
		Quantity:     in.Config.Risk.Quantity,
		StrategyName: config.StrategyMomentumPullback,
		ConfigID:     in.Config.ConfigID,
		CreatedAt:    in.Now,
	}
}

func mkInput(now time.Time) TradingCycleInput {
	cfg := &config.StrategyConfig{
		ConfigID:  "test-cfg",
		Symbol:    "USD_JPY",
		Enabled:   true,
		ValidFrom: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour),
		Strategy: config.StrategySection{Name: config.StrategyMomentumPullback},
		Entry:    config.EntrySection{Direction: config.DirectionBoth, MaxSpreadPips: 5},
		Exit:     config.ExitSection{TakeProfitPips: 10, StopLossPips: 10, MaxHoldMinutes: 5},
		Risk:     config.ConfigRiskSection{Quantity: 100, MaxOpenPositions: 1, MaxTradesInThisWindow: 99, MaxLossInThisWindowJPY: 99999},
	}
	return TradingCycleInput{
		Now:          now,
		Ticker:       &market.Ticker{Symbol: "USD_JPY", Bid: 100.10, Ask: 100.11, Timestamp: now},
		ActiveConfig: cfg,
		Summary: &market.MarketSummary{
			Symbol:      "USD_JPY",
			CurrentRate: market.CurrentRate{Bid: 100.10, Ask: 100.11, SpreadPips: 1},
			Summary6h:   market.WindowSummary{High: 100.30, Low: 100.00, RangePips: 30, TrendDirection: "up", NumCandles: 360},
			Summary24h:  market.WindowSummary{High: 100.40, Low: 99.80, RangePips: 60, TrendDirection: "up", NumCandles: 1440},
		},
		Candles1m: []market.Candle{{Open: 100.0, High: 100.2, Low: 99.9, Close: 100.1, OpenTime: now.Add(-time.Minute)}},
		Candles5m: []market.Candle{{Open: 100.0, High: 100.2, Low: 99.9, Close: 100.1, OpenTime: now.Add(-5 * time.Minute)}},
		AccountSnapshot: risk.AccountSnapshot{
			OpenPositions: 0, MaxConsecutiveLosses: 99,
		},
	}
}

func TestTradingCycle_Execute_HappyPath_PlacesOrder(t *testing.T) {
	now := time.Now()
	pos := backtest.NewInMemoryPositionRepo()
	eng := strategy.NewEngine()
	eng.Register(alwaysBuy{})
	evaluator := &EvaluateEntry{Engine: eng}
	executor := NewExecuteOrder(&fakeBroker{}, pos, config.ModePaperConfig, "USD_JPY", silentLogger())
	tc := &TradingCycle{Evaluator: evaluator, Executor: executor}

	res, err := tc.Execute(context.Background(), mkInput(now))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Executed {
		t.Fatalf("expected Executed=true; got %+v", res)
	}
	open, _ := pos.ListOpenOrClosing(context.Background(), "USD_JPY")
	if len(open) != 1 {
		t.Errorf("expected 1 open position; got %d", len(open))
	}
}

func TestTradingCycle_Execute_NoActiveConfig_NoOp(t *testing.T) {
	now := time.Now()
	in := mkInput(now)
	in.ActiveConfig = nil
	eng := strategy.NewEngine()
	eng.Register(alwaysBuy{})
	evaluator := &EvaluateEntry{Engine: eng}
	executor := NewExecuteOrder(&fakeBroker{}, backtest.NewInMemoryPositionRepo(), config.ModePaperConfig, "USD_JPY", silentLogger())
	tc := &TradingCycle{Evaluator: evaluator, Executor: executor}

	res, _ := tc.Execute(context.Background(), in)
	if res.Executed {
		t.Errorf("nil active should not execute; got %+v", res)
	}
}

// Hour partition: when llm_decision is enabled EntriesDisabled is globally true,
// but the per-tick engine must still run during the JST hours the LLM cedes to a
// deterministic strategy (exhaustion_fade owns USD_JPY in JST {4,10,11}).
func TestTradingCycle_Execute_EntriesDisabled_EngineOwnedHours(t *testing.T) {
	mk := func() *TradingCycle {
		pos := backtest.NewInMemoryPositionRepo()
		eng := strategy.NewEngine()
		eng.Register(alwaysBuy{})
		return &TradingCycle{
			Evaluator:           &EvaluateEntry{Engine: eng},
			Executor:            NewExecuteOrder(&fakeBroker{}, pos, config.ModePaperConfig, "USD_JPY", silentLogger()),
			EntriesDisabled:     true,
			EngineOwnedHoursJST: []int{10},
		}
	}
	// 01:30 UTC = 10:30 JST → engine owns the hour → executes despite EntriesDisabled.
	if res, _ := mk().Execute(context.Background(), mkInput(time.Date(2026, 6, 24, 1, 30, 0, 0, time.UTC))); !res.Executed {
		t.Fatalf("engine-owned hour must bypass EntriesDisabled; got %+v", res)
	}
	// 00:30 UTC = 09:30 JST → not owned → stays disabled.
	if res, _ := mk().Execute(context.Background(), mkInput(time.Date(2026, 6, 24, 0, 30, 0, 0, time.UTC))); res.Executed || res.GateReason != "engine_entries_disabled_v2_exclusive" {
		t.Fatalf("non-owned hour must stay disabled; got %+v", res)
	}
}

func TestTradingCycle_Execute_NilEvaluator_ReturnsZero(t *testing.T) {
	tc := &TradingCycle{}
	res, err := tc.Execute(context.Background(), TradingCycleInput{})
	if err != nil || res.Executed {
		t.Errorf("nil evaluator should be a no-op; got res=%+v err=%v", res, err)
	}
}
