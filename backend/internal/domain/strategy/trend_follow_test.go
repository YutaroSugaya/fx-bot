package strategy

import (
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
)

// trend_follow probe: 1h 200SMA slope = trend; a Donchian-48 (1h) continuation break in the
// trend direction = entry; wide ATR-scaled ratchet lets the trend run. Distinct mechanism from
// the pullback-to-MA entry — here per-trade edge is a whole trend leg, >> the cost floor.
// Pre-registered, FIXED params, NOT swept.

func mk1hSeries(n int, startPrice, stepPips, pip float64) []market.Candle {
	out := make([]market.Candle, n)
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		c := startPrice + float64(i)*stepPips*pip
		out[i] = market.Candle{
			Symbol: "GBP_JPY", OpenTime: base.Add(time.Duration(i) * time.Hour),
			Open: c, High: c + 5*pip, Low: c - 5*pip, Close: c,
		}
	}
	return out
}

func TestTrendFollow_Evaluate(t *testing.T) {
	pip := 0.01
	cfg := &config.StrategyConfig{
		ConfigID: "test-trend", Symbol: "GBP_JPY",
		Entry: config.EntrySection{Direction: config.DirectionBoth, MaxSpreadPips: 2.0},
		Risk:  config.ConfigRiskSection{Quantity: 1000},
	}
	eval := func(candles1h []market.Candle, mid float64) Signal {
		sum := &market.MarketSummary{CurrentRate: market.CurrentRate{Bid: mid, Ask: mid, SpreadPips: 0}}
		return TrendFollow{}.Evaluate(EvalInput{
			Now:    time.Date(2020, 1, 12, 12, 0, 0, 0, time.UTC),
			Config: cfg, Summary: sum, Candles1h: candles1h,
		})
	}

	// Strong uptrend (260×1h rising 2pips/bar). Last close = 150 + 259*0.02 = 155.18, high 155.23.
	up := mk1hSeries(260, 150.00, 2, pip)
	lastHigh := up[len(up)-1].High // 155.23

	// Price breaks above the Donchian-48 high in an uptrend → ENTER buy with structural SL + ratchet.
	sig := eval(up, lastHigh+0.30)
	if sig.Decision != DecisionEnter || sig.Side != order.SideBuy {
		t.Fatalf("uptrend + breakout must ENTER buy; got %v %v (%s)", sig.Decision, sig.Side, sig.Reason)
	}
	if sig.StopLossPips <= 0 {
		t.Errorf("SL must be a positive structural stop; got %.2f", sig.StopLossPips)
	}
	if sig.RatchetArmPips <= 0 || sig.RatchetGivebackPips <= 0 {
		t.Errorf("trend follower must trail via ratchet; got arm=%.2f give=%.2f", sig.RatchetArmPips, sig.RatchetGivebackPips)
	}
	if sig.RatchetArmPips <= sig.RatchetGivebackPips {
		t.Errorf("arm (%.2f) must exceed giveback (%.2f) so winners run before trailing", sig.RatchetArmPips, sig.RatchetGivebackPips)
	}
	if sig.MaxHoldMinutes != tfMaxHoldMin {
		t.Errorf("MaxHold = %d; want %d (long, let trends run)", sig.MaxHoldMinutes, tfMaxHoldMin)
	}

	// Uptrend but price NOT beyond the Donchian high → no continuation trigger → no entry.
	if s := eval(up, 154.00); s.Decision == DecisionEnter {
		t.Errorf("no fresh breakout must not enter; got %v (%s)", s.Decision, s.Reason)
	}

	// Strong downtrend + break below Donchian low → ENTER sell.
	down := mk1hSeries(260, 160.00, -2, pip)
	lastLow := down[len(down)-1].Low
	if s := eval(down, lastLow-0.30); s.Decision != DecisionEnter || s.Side != order.SideSell {
		t.Errorf("downtrend + breakdown must ENTER sell; got %v %v (%s)", s.Decision, s.Side, s.Reason)
	}

	// Flat market (no slope) → no trend → no entry.
	flat := mk1hSeries(260, 150.00, 0, pip)
	if s := eval(flat, 150.50); s.Decision == DecisionEnter {
		t.Errorf("flat market must not enter (no trend); got %v (%s)", s.Decision, s.Reason)
	}

	// Not enough 1h history → no entry (no panic).
	if s := eval(mk1hSeries(50, 150.00, 2, pip), 200.00); s.Decision == DecisionEnter {
		t.Errorf("insufficient 1h history must not enter; got %v", s.Decision)
	}
}

// Volatility-floor entry gate (MinEntryATRPips): an OFFLINE-ONLY, default-OFF filter to test the
// "only trade when 1h ATR ≥ floor" hypothesis. 0 = OFF (live untouched);
// when set, a would-be entry whose 1h ATR is below the floor is suppressed. NOT a live default — it
// is swept on in-sample and validated FIXED on OOS in the backtest harness, never tuned on live.
func TestTrendFollow_VolatilityFloorGate(t *testing.T) {
	pip := 0.01
	up := mk1hSeries(260, 150.00, 2, pip) // ~10pip 1h ATR (High/Low = close ± 5pip)
	lastHigh := up[len(up)-1].High
	mk := func(floor float64) Signal {
		cfg := &config.StrategyConfig{
			ConfigID: "test-trend-volgate", Symbol: "GBP_JPY",
			Entry: config.EntrySection{Direction: config.DirectionBoth, MaxSpreadPips: 2.0, MinEntryATRPips: floor},
			Risk:  config.ConfigRiskSection{Quantity: 1000},
		}
		sum := &market.MarketSummary{CurrentRate: market.CurrentRate{Bid: lastHigh + 0.30, Ask: lastHigh + 0.30, SpreadPips: 0}}
		return TrendFollow{}.Evaluate(EvalInput{Now: time.Date(2020, 1, 12, 12, 0, 0, 0, time.UTC), Config: cfg, Summary: sum, Candles1h: up})
	}

	// floor 0 = OFF → the breakout still ENTERs (back-compat with live).
	if s := mk(0); s.Decision != DecisionEnter {
		t.Fatalf("floor=0 (OFF) must keep entering; got %v (%s)", s.Decision, s.Reason)
	}
	// floor below the bar's ~10pip ATR → entry survives the gate.
	if s := mk(5); s.Decision != DecisionEnter {
		t.Errorf("floor below ATR must still enter; got %v (%s)", s.Decision, s.Reason)
	}
	// floor above the bar's ~10pip ATR → entry suppressed (low-volatility regime skipped).
	if s := mk(20); s.Decision == DecisionEnter {
		t.Errorf("floor above ATR must suppress the entry (low-vol skip); got %v (%s)", s.Decision, s.Reason)
	}
}
