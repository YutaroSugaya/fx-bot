package strategy

import (
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
)

// daily_trend runs on DAILY bars supplied via in.Candles1m. 200-day SMA slope sign = trend,
// Donchian-55 break = entry, ATR-scaled SL/ratchet, MaxHold 90d. Parameters are pre-registered (fixed before the backtest).

func mkDailySeries(n int, start, stepPips, pip float64) []market.Candle {
	out := make([]market.Candle, n)
	base := time.Date(2016, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		c := start + float64(i)*stepPips*pip
		out[i] = market.Candle{
			Symbol: "EUR_USD", OpenTime: base.AddDate(0, 0, i),
			Open: c, High: c + 20*pip, Low: c - 20*pip, Close: c,
		}
	}
	return out
}

func TestDailyTrend_Evaluate(t *testing.T) {
	pip := 0.0001
	cfg := &config.StrategyConfig{
		ConfigID: "test-daily", Symbol: "EUR_USD",
		Entry: config.EntrySection{Direction: config.DirectionBoth, MaxSpreadPips: 5.0},
		Risk:  config.ConfigRiskSection{Quantity: 1000},
	}
	eval := func(bars []market.Candle, mid float64) Signal {
		sum := &market.MarketSummary{CurrentRate: market.CurrentRate{Bid: mid, Ask: mid, SpreadPips: 0}}
		return DailyTrend{}.Evaluate(EvalInput{
			Now:    time.Date(2017, 6, 1, 0, 0, 0, 0, time.UTC),
			Config: cfg, Summary: sum, Candles1m: bars, // daily bars carried in Candles1m
		})
	}

	// Rising 260-day series (uptrend). Last high defines the Donchian-55 ceiling.
	up := mkDailySeries(260, 1.1000, 5, pip)
	lastHigh := up[len(up)-1].High

	sig := eval(up, lastHigh+0.0050) // price breaks above the 55-day high in an uptrend
	if sig.Decision != DecisionEnter || sig.Side != order.SideBuy {
		t.Fatalf("uptrend + 55d breakout must ENTER buy; got %v %v (%s)", sig.Decision, sig.Side, sig.Reason)
	}
	if sig.StopLossPips <= 0 || sig.RatchetArmPips <= sig.RatchetGivebackPips {
		t.Errorf("need positive SL and arm>giveback; SL=%.1f arm=%.1f give=%.1f", sig.StopLossPips, sig.RatchetArmPips, sig.RatchetGivebackPips)
	}
	if sig.MaxHoldMinutes != dtMaxHoldMin {
		t.Errorf("MaxHold=%d want %d (90d)", sig.MaxHoldMinutes, dtMaxHoldMin)
	}

	// Uptrend but price below the Donchian high → no fresh breakout → no entry.
	if s := eval(up, 1.1000); s.Decision == DecisionEnter {
		t.Errorf("no breakout must not enter; got %v (%s)", s.Decision, s.Reason)
	}

	// Downtrend + break below 55-day low → SELL.
	down := mkDailySeries(260, 1.3000, -5, pip)
	if s := eval(down, down[len(down)-1].Low-0.0050); s.Decision != DecisionEnter || s.Side != order.SideSell {
		t.Errorf("downtrend + breakdown must ENTER sell; got %v %v (%s)", s.Decision, s.Side, s.Reason)
	}

	// Flat series → no trend → no entry.
	if s := eval(mkDailySeries(260, 1.1000, 0, pip), 1.1050); s.Decision == DecisionEnter {
		t.Errorf("flat must not enter; got %v", s.Decision)
	}
	// Insufficient daily history → no entry (no panic).
	if s := eval(mkDailySeries(100, 1.1000, 5, pip), 2.0); s.Decision == DecisionEnter {
		t.Errorf("insufficient history must not enter; got %v", s.Decision)
	}
}
