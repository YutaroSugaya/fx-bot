package strategy

import (
	"math"
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
)

// asianRange is the pure core: the high/low of the [00:00,07:00) UTC Tokyo
// session, which the London-open breakout trades against.
func TestAsianRange(t *testing.T) {
	utc := time.UTC
	bar := func(h, min int, hi, lo float64) market.Candle {
		return market.Candle{
			Symbol: "GBP_JPY", OpenTime: time.Date(2024, 3, 5, h, min, 0, 0, utc),
			High: hi, Low: lo, Open: (hi + lo) / 2, Close: (hi + lo) / 2,
		}
	}
	candles := []market.Candle{
		bar(1, 0, 190.30, 190.10),
		bar(3, 0, 190.50, 190.20), // session high = 190.50
		bar(6, 0, 190.25, 190.00), // session low = 190.00
		bar(8, 0, 191.00, 189.50), // hour 8 = OUTSIDE [0,7); must be ignored
	}
	hi, lo, ok := asianRange(candles, time.Date(2024, 3, 5, 7, 30, 0, 0, utc))
	if !ok || hi != 190.50 || lo != 190.00 {
		t.Fatalf("asianRange = (%.2f, %.2f, %v); want (190.50, 190.00, true)", hi, lo, ok)
	}
}

func TestLondonBreakout_Evaluate(t *testing.T) {
	utc := time.UTC
	bar := func(h, min int, hi, lo, c float64) market.Candle {
		return market.Candle{
			Symbol: "GBP_JPY", OpenTime: time.Date(2024, 3, 5, h, min, 0, 0, utc),
			High: hi, Low: lo, Open: (hi + lo) / 2, Close: c,
		}
	}
	// Asian session [00:00,07:00): range [190.00, 190.50] = 50 pips wide.
	asian := []market.Candle{bar(1, 0, 190.30, 190.10, 190.20), bar(3, 0, 190.50, 190.20, 190.40), bar(6, 0, 190.25, 190.00, 190.10)}
	cfg := &config.StrategyConfig{
		ConfigID: "test-london", Symbol: "GBP_JPY",
		Entry: config.EntrySection{Direction: config.DirectionBoth, MaxSpreadPips: 2.0},
		Risk:  config.ConfigRiskSection{Quantity: 1000},
	}
	eval := func(now time.Time, price float64, candles []market.Candle, c *config.StrategyConfig) Signal {
		sum := &market.MarketSummary{CurrentRate: market.CurrentRate{Bid: price, Ask: price, SpreadPips: 0}}
		return LondonBreakout{}.Evaluate(EvalInput{Now: now, Config: c, Summary: sum, Candles1m: candles})
	}
	inWindow := time.Date(2024, 3, 5, 7, 30, 0, 0, utc)

	// Close above the Asian high inside the entry window → BUY, structural SL/TP from range width.
	sig := eval(inWindow, 190.60, asian, cfg)
	if sig.Decision != DecisionEnter || sig.Side != order.SideBuy {
		t.Fatalf("break above 190.50 must ENTER buy; got %v %v (%s)", sig.Decision, sig.Side, sig.Reason)
	}
	if math.Abs(sig.StopLossPips-lbSLRangeFrac*50) > 0.1 || math.Abs(sig.TakeProfitPips-lbTPRangeFrac*50) > 0.1 {
		t.Errorf("SL/TP = %.2f/%.2f; want ~%.0f/%.0f (range 50pips × %.1f/%.1f)",
			sig.StopLossPips, sig.TakeProfitPips, lbSLRangeFrac*50, lbTPRangeFrac*50, lbSLRangeFrac, lbTPRangeFrac)
	}

	// Close below the Asian low → SELL.
	if s := eval(inWindow, 189.90, asian, cfg); s.Decision != DecisionEnter || s.Side != order.SideSell {
		t.Errorf("break below 190.00 must ENTER sell; got %v %v", s.Decision, s.Side)
	}
	// Inside the range → no entry.
	if s := eval(inWindow, 190.25, asian, cfg); s.Decision == DecisionEnter {
		t.Errorf("price inside range must not enter; got %v", s.Decision)
	}
	// Outside the entry window (11:00 UTC) → no entry even when broken.
	if s := eval(time.Date(2024, 3, 5, 11, 0, 0, 0, utc), 190.60, asian, cfg); s.Decision == DecisionEnter {
		t.Errorf("outside entry window must not enter; got %v", s.Decision)
	}
	// One trade per day: a prior entry-window bar already closed beyond the range → skip.
	withPrior := append(append([]market.Candle{}, asian...), bar(7, 10, 190.65, 190.40, 190.60)) // 07:10 close above hi
	if s := eval(inWindow, 190.70, withPrior, cfg); s.Decision == DecisionEnter {
		t.Errorf("second break of the day must be suppressed; got %v (%s)", s.Decision, s.Reason)
	}
}
