package ta

import (
	"testing"
	"time"

	"fx-bot/backend/internal/domain/market"
)

// ohlc builds a candle from explicit OHLC (volume/time irrelevant for these
// pure feature tests). Index spacing is 1 minute so any time-based helper is
// well-formed.
func ohlc(i int, open, high, low, close float64) market.Candle {
	t0 := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	return market.Candle{
		Symbol:   "USD_JPY",
		Interval: time.Minute,
		OpenTime: t0.Add(time.Duration(i) * time.Minute),
		Open:     open, High: high, Low: low, Close: close,
	}
}

// flatC is a candle with all OHLC equal to v (used where only Close matters).
func flatC(i int, v float64) market.Candle { return ohlc(i, v, v, v, v) }

func TestNetMovePips(t *testing.T) {
	cs := []market.Candle{
		flatC(0, 150.00), flatC(1, 150.05), flatC(2, 150.10),
		flatC(3, 150.20), flatC(4, 150.35), flatC(5, 150.50),
	}
	if got, ok := NetMovePips(cs, 5, 0.01); !ok || !approx(got, 50) {
		t.Fatalf("net over 5 = (150.50-150.00)=50pips, got %v/%v", got, ok)
	}
	if got, ok := NetMovePips(cs, 2, 0.01); !ok || !approx(got, 30) {
		t.Fatalf("net over 2 = (150.50-150.20)=30pips, got %v/%v", got, ok)
	}
	// A down move is negative.
	down := []market.Candle{flatC(0, 150.50), flatC(1, 150.30), flatC(2, 150.10)}
	if got, ok := NetMovePips(down, 2, 0.01); !ok || !approx(got, -40) {
		t.Fatalf("down net = -40pips, got %v/%v", got, ok)
	}
	if _, ok := NetMovePips(cs, 6, 0.01); ok {
		t.Fatal("need n+1 candles: expected ok=false")
	}
	if _, ok := NetMovePips(cs, 0, 0.01); ok {
		t.Fatal("n<1: expected ok=false")
	}
	if _, ok := NetMovePips(cs, 2, 0); ok {
		t.Fatal("pipSize<=0: expected ok=false")
	}
}

func TestDeviationATR(t *testing.T) {
	// price 0.30 above MA = 30 pips; ATR 10 pips → 3 ATR stretched up.
	if got, ok := DeviationATR(150.50, 150.20, 10, 0.01); !ok || !approx(got, 3.0) {
		t.Fatalf("expected +3.0 ATR, got %v/%v", got, ok)
	}
	// Below the MA is negative.
	if got, ok := DeviationATR(150.05, 150.20, 10, 0.01); !ok || !approx(got, -1.5) {
		t.Fatalf("expected -1.5 ATR, got %v/%v", got, ok)
	}
	if _, ok := DeviationATR(150.50, 150.20, 0, 0.01); ok {
		t.Fatal("atr<=0: expected ok=false")
	}
	if _, ok := DeviationATR(150.50, 150.20, 10, 0); ok {
		t.Fatal("pipSize<=0: expected ok=false")
	}
}

func TestShapeOf(t *testing.T) {
	// Bullish body 30p, upper wick 10p, lower wick 5p, range 45p.
	s, ok := ShapeOf(ohlc(0, 150.10, 150.50, 150.05, 150.40), 0.01)
	if !ok {
		t.Fatal("expected ok")
	}
	if !s.Bullish {
		t.Error("close>open → bullish")
	}
	if !approx(s.BodyPips, 30) || !approx(s.UpperWickPips, 10) || !approx(s.LowerWickPips, 5) || !approx(s.RangePips, 45) {
		t.Errorf("shape pips: body=%v upper=%v lower=%v range=%v", s.BodyPips, s.UpperWickPips, s.LowerWickPips, s.RangePips)
	}
	if !approx(s.UpperWickToBody, 10.0/30.0) || !approx(s.LowerWickToBody, 5.0/30.0) {
		t.Errorf("ratios: upper=%v lower=%v", s.UpperWickToBody, s.LowerWickToBody)
	}
	// Shooting-star pin bar: tiny bearish body 3p, long upper wick 15p → rejection.
	pin, _ := ShapeOf(ohlc(1, 150.45, 150.60, 150.41, 150.42), 0.01)
	if pin.Bullish {
		t.Error("close<open → bearish")
	}
	if !approx(pin.UpperWickToBody, 15.0/3.0) {
		t.Errorf("pin upper/body expected 5.0, got %v", pin.UpperWickToBody)
	}
	// Doji: zero body → ratios are 0 (no divide-by-zero blow-up), still ok.
	doji, ok := ShapeOf(ohlc(2, 150.20, 150.30, 150.10, 150.20), 0.01)
	if !ok || doji.UpperWickToBody != 0 || doji.LowerWickToBody != 0 {
		t.Errorf("doji ratios must be 0, got upper=%v lower=%v ok=%v", doji.UpperWickToBody, doji.LowerWickToBody, ok)
	}
	if _, ok := ShapeOf(ohlc(3, 1, 1, 1, 1), 0); ok {
		t.Fatal("pipSize<=0: expected ok=false")
	}
}

func TestConsecutiveBars(t *testing.T) {
	// last three bullish, preceded by a bearish.
	up := []market.Candle{
		ohlc(0, 150.20, 150.21, 150.10, 150.11), // bearish
		ohlc(1, 150.11, 150.20, 150.10, 150.18), // bull
		ohlc(2, 150.18, 150.28, 150.17, 150.26), // bull
		ohlc(3, 150.26, 150.35, 150.25, 150.33), // bull
	}
	if n, dirUp := ConsecutiveBars(up); n != 3 || !dirUp {
		t.Fatalf("expected 3/up, got %d/%v", n, dirUp)
	}
	down := []market.Candle{
		ohlc(0, 150.10, 150.20, 150.09, 150.18), // bull
		ohlc(1, 150.18, 150.19, 150.10, 150.11), // bear
		ohlc(2, 150.11, 150.12, 150.00, 150.02), // bear
	}
	if n, dirUp := ConsecutiveBars(down); n != 2 || dirUp {
		t.Fatalf("expected 2/down, got %d/%v", n, dirUp)
	}
	// Doji last bar → no streak.
	doji := []market.Candle{ohlc(0, 150.10, 150.20, 150.05, 150.20), ohlc(1, 150.20, 150.25, 150.15, 150.20)}
	if n, _ := ConsecutiveBars(doji); n != 0 {
		t.Fatalf("doji last → streak 0, got %d", n)
	}
	if n, _ := ConsecutiveBars(nil); n != 0 {
		t.Fatalf("empty → 0, got %d", n)
	}
}

func TestMakesNewHighLow(t *testing.T) {
	// Last high 150.35 does NOT exceed the prior-2 max (150.40) = momentum stalled.
	stalled := []market.Candle{
		ohlc(0, 0, 150.30, 0, 0), ohlc(1, 0, 150.40, 0, 0), ohlc(2, 0, 150.35, 0, 0),
	}
	if MakesNewHigh(stalled, 2) {
		t.Error("150.35 < prior max 150.40 → not a new high")
	}
	// Last high 150.55 exceeds prior-2 max → still pushing up.
	pushing := []market.Candle{
		ohlc(0, 0, 150.30, 0, 0), ohlc(1, 0, 150.40, 0, 0), ohlc(2, 0, 150.55, 0, 0),
	}
	if !MakesNewHigh(pushing, 2) {
		t.Error("150.55 > prior max → new high")
	}
	// Mirror for lows.
	lowStalled := []market.Candle{
		ohlc(0, 0, 0, 150.10, 0), ohlc(1, 0, 0, 150.00, 0), ohlc(2, 0, 0, 150.05, 0),
	}
	if MakesNewLow(lowStalled, 2) {
		t.Error("150.05 > prior min 150.00 → not a new low")
	}
	if MakesNewHigh(pushing, 5) {
		t.Error("not enough prior candles → false")
	}
}

func TestLastSwing(t *testing.T) {
	// A clear pivot high at index 2 (price 150.50), pivot low at index 2 mirror.
	cs := []market.Candle{
		ohlc(0, 150.10, 150.12, 150.08, 150.11),
		ohlc(1, 150.11, 150.22, 150.10, 150.20),
		ohlc(2, 150.20, 150.50, 150.18, 150.40), // pivot high
		ohlc(3, 150.40, 150.25, 150.20, 150.22),
		ohlc(4, 150.22, 150.18, 150.12, 150.14),
	}
	if p, ok := LastSwingHigh(cs, 2, 0.01, 0); !ok || !approx(p, 150.50) {
		t.Fatalf("last swing high expected 150.50, got %v/%v", p, ok)
	}
	low := []market.Candle{
		ohlc(0, 150.40, 150.42, 150.38, 150.40),
		ohlc(1, 150.40, 150.41, 150.30, 150.31),
		ohlc(2, 150.31, 150.33, 150.00, 150.10), // pivot low
		ohlc(3, 150.10, 150.25, 150.09, 150.22),
		ohlc(4, 150.22, 150.40, 150.20, 150.38),
	}
	if p, ok := LastSwingLow(low, 2, 0.01, 0); !ok || !approx(p, 150.00) {
		t.Fatalf("last swing low expected 150.00, got %v/%v", p, ok)
	}
	// Too few candles → ok=false.
	if _, ok := LastSwingHigh(cs[:2], 2, 0.01, 0); ok {
		t.Fatal("too few candles: expected ok=false")
	}
}

func TestRoundNumberDistancePips(t *testing.T) {
	// .50 grid catches both figures and half-figures.
	if d, near, ok := RoundNumberDistancePips(150.04, 0.50, 0.01); !ok || !approx(d, 4) || !approx(near, 150.00) {
		t.Fatalf("150.04 → 4p from 150.00, got d=%v near=%v ok=%v", d, near, ok)
	}
	if d, near, ok := RoundNumberDistancePips(150.48, 0.50, 0.01); !ok || !approx(d, 2) || !approx(near, 150.50) {
		t.Fatalf("150.48 → 2p from 150.50, got d=%v near=%v ok=%v", d, near, ok)
	}
	// Figure grid (1.00).
	if d, near, ok := RoundNumberDistancePips(150.97, 1.00, 0.01); !ok || !approx(d, 3) || !approx(near, 151.00) {
		t.Fatalf("150.97 → 3p from 151.00, got d=%v near=%v ok=%v", d, near, ok)
	}
	if _, _, ok := RoundNumberDistancePips(150.0, 0, 0.01); ok {
		t.Fatal("step<=0: expected ok=false")
	}
	if _, _, ok := RoundNumberDistancePips(150.0, 0.50, 0); ok {
		t.Fatal("pipSize<=0: expected ok=false")
	}
}

func TestEfficiencyRatio(t *testing.T) {
	// Monotone rise → perfectly efficient (1.0).
	rise := []float64{1, 2, 3, 4, 5, 6}
	if er, ok := EfficiencyRatio(rise, 5); !ok || !approx(er, 1.0) {
		t.Fatalf("monotone rise ER=1.0, got %v/%v", er, ok)
	}
	// Zigzag round-trip → low efficiency (net 1 / path 5 = 0.2).
	zig := []float64{1, 2, 1, 2, 1, 2}
	if er, ok := EfficiencyRatio(zig, 5); !ok || !approx(er, 0.2) {
		t.Fatalf("zigzag ER=0.2, got %v/%v", er, ok)
	}
	if _, ok := EfficiencyRatio(rise, 6); ok {
		t.Fatal("need n+1 closes: expected ok=false")
	}
	if _, ok := EfficiencyRatio([]float64{5, 5, 5}, 2); ok {
		t.Fatal("zero path (flat): expected ok=false")
	}
}
