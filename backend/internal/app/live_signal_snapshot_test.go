package app

import (
	"strings"
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/usecase/command"
)

// flatBars builds n flat candles at `dur` spacing (all 150.00) — enough bars to
// satisfy a strategy's lookback without implying any trend.
func flatBars(t0 time.Time, n int, dur time.Duration) []market.Candle {
	out := make([]market.Candle, n)
	for i := 0; i < n; i++ {
		out[i] = market.Candle{
			Symbol: "USD_JPY", Interval: dur, OpenTime: t0.Add(time.Duration(i) * dur),
			Open: 150.0, High: 150.0, Low: 150.0, Close: 150.0,
		}
	}
	return out
}

// Regression (display bug): the ma_pullback gate funnel must be computed
// with the SAME Candles1h the engine used. Recomputing Gates() with only 5m candles
// makes the trend gate falsely show "1H足の蓄積待ち (0/220)" while
// the bot is really evaluating the 1h trend (reason=not_in_ma_zone / no_trend). With
// enough 1h bars supplied, the trend gate must reflect the 1h slope, not "蓄積待ち".
func TestBuildLiveSignalSnapshot_GatesUse1hCandles(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := &config.StrategyConfig{Symbol: "USD_JPY"}
	cfg.Strategy.Name = config.StrategyMAPullback
	cfg.Entry.MaxSpreadPips = 1.0
	res := command.TradingCycleResult{
		Decision: strategy.DecisionNone,
		Signal:   strategy.Signal{StrategyName: config.StrategyMAPullback, Reason: "not_in_ma_zone"},
	}
	c5m := flatBars(now, 220, 5*time.Minute)
	c1h := flatBars(now, 230, time.Hour) // >= maPB1hMinBars (220)
	s := BuildLiveSignalSnapshot("USD_JPY", now, res, sampleSummary(), cfg, c5m, c1h)

	found := false
	for _, g := range s.Gates {
		if g.Key == "trend" {
			found = true
			if strings.Contains(g.Detail, "蓄積待ち") {
				t.Errorf("trend gate recomputed WITHOUT Candles1h: %q (must reflect the 1h slope)", g.Detail)
			}
		}
	}
	if !found {
		t.Fatal("no trend gate in ma_pullback snapshot")
	}
}

func sampleSummary() *market.MarketSummary {
	return &market.MarketSummary{
		CurrentRate: market.CurrentRate{SpreadPips: 0.4},
		Summary6h:   market.WindowSummary{TrendDirection: "down", ATRPips: 5.2},
		Summary24h:  market.WindowSummary{TrendDirection: "up"},
	}
}

func sampleConfig() *config.StrategyConfig {
	c := &config.StrategyConfig{}
	c.Strategy.Name = config.StrategyMomentumPullback
	c.Entry.MaxSpreadPips = 1.0
	return c
}

// 戦略が「押し目待ち」で見送ったケース: decision=none, reason 反映, gate は無関係。
func TestBuildLiveSignalSnapshot_StrategySkip(t *testing.T) {
	now := time.Date(2026, 6, 4, 7, 0, 0, 0, time.UTC)
	res := command.TradingCycleResult{
		Signal: strategy.Signal{
			Decision:     strategy.DecisionNone,
			StrategyName: config.StrategyMomentumPullback,
			Reason:       "no_pullback",
		},
		Decision:   strategy.DecisionNone,
		GateReason: "no_pullback",
	}
	s := BuildLiveSignalSnapshot("USD_JPY", now, res, sampleSummary(), sampleConfig(), nil, nil)

	if s.Decision != "none" {
		t.Errorf("decision: got %q want none", s.Decision)
	}
	if s.Reason != "no_pullback" {
		t.Errorf("reason: got %q", s.Reason)
	}
	if s.GateBlocked {
		t.Error("gate should not be blocked when strategy itself skipped")
	}
	if s.Executed {
		t.Error("executed should be false")
	}
	if s.Trend6h != "down" || s.Trend24h != "up" {
		t.Errorf("trend: 6h=%q 24h=%q", s.Trend6h, s.Trend24h)
	}
	if s.SpreadPips != 0.4 || s.MaxSpreadPips != 1.0 {
		t.Errorf("spread: %v / %v", s.SpreadPips, s.MaxSpreadPips)
	}
	if s.Atr6hPips != 5.2 {
		t.Errorf("atr: %v", s.Atr6hPips)
	}
}

// 戦略は ENTER だが executor まで到達 → executed=true, gate_blocked=false。
func TestBuildLiveSignalSnapshot_Executed(t *testing.T) {
	now := time.Date(2026, 6, 4, 7, 0, 0, 0, time.UTC)
	res := command.TradingCycleResult{
		Signal: strategy.Signal{
			Decision:       strategy.DecisionEnter,
			Side:           order.SideSell,
			StrategyName:   config.StrategyMomentumPullback,
			EntryPrice:     159.2,
			TakeProfitPips: 12,
			StopLossPips:   6,
		},
		Decision: strategy.DecisionEnter,
		Executed: true,
	}
	s := BuildLiveSignalSnapshot("USD_JPY", now, res, sampleSummary(), sampleConfig(), nil, nil)

	if s.Decision != "enter" {
		t.Errorf("decision: got %q want enter", s.Decision)
	}
	if s.Side != "SELL" {
		t.Errorf("side: got %q want SELL", s.Side)
	}
	if !s.Executed {
		t.Error("executed should be true")
	}
	if s.GateBlocked {
		t.Error("gate_blocked should be false when executed")
	}
	if s.EntryPrice != 159.2 || s.TakeProfitPips != 12 || s.StopLossPips != 6 {
		t.Errorf("order fields: %v %v %v", s.EntryPrice, s.TakeProfitPips, s.StopLossPips)
	}
}

// 戦略は ENTER だがリスクゲートでブロック → gate_blocked=true, gate_reason 反映。
func TestBuildLiveSignalSnapshot_GateBlocked(t *testing.T) {
	now := time.Date(2026, 6, 4, 7, 0, 0, 0, time.UTC)
	res := command.TradingCycleResult{
		Signal: strategy.Signal{
			Decision:     strategy.DecisionEnter,
			Side:         order.SideSell,
			StrategyName: config.StrategyMomentumPullback,
			Reason:       "trend=down pullback",
		},
		Decision:   strategy.DecisionEnter,
		GateReason: "cooldown loss until 07:30:00",
		Executed:   false,
	}
	s := BuildLiveSignalSnapshot("USD_JPY", now, res, sampleSummary(), sampleConfig(), nil, nil)

	if s.Decision != "enter" {
		t.Errorf("decision: got %q", s.Decision)
	}
	if !s.GateBlocked {
		t.Error("gate_blocked should be true (strategy enter but no order)")
	}
	if s.GateReason != "cooldown loss until 07:30:00" {
		t.Errorf("gate_reason: got %q", s.GateReason)
	}
	if s.Executed {
		t.Error("executed should be false")
	}
}
