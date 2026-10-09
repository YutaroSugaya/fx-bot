package ta

import (
	"math"
	"testing"

	"fx-bot/backend/internal/domain/market"
)

// risingChannelCandles is a clean rising zigzag (n=1) whose swing highs lie on
// y=0.5x+9.5 (idx 2,4,6 → 10.5,11.5,12.5) and swing lows on y=0.5x+8.5
// (idx 1,3,5,7 → 9.0,10.0,11.0,12.0): a perfectly parallel channel of width 1.0.
func risingChannelCandles() []market.Candle {
	highs := []float64{9.8, 9.6, 10.5, 10.3, 11.5, 11.3, 12.5, 12.3, 12.4}
	lows := []float64{9.4, 9.0, 10.2, 10.0, 11.2, 11.0, 12.2, 12.0, 12.1}
	return candlesHL(highs, lows)
}

func TestParallelChannel_RisingParallel(t *testing.T) {
	ch, ok := ParallelChannel(risingChannelCandles(), 1, pip, 0)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !approx(ch.Upper.Slope, 0.5) || !approx(ch.Lower.Slope, 0.5) {
		t.Fatalf("expected both slopes 0.5, got upper=%v lower=%v", ch.Upper.Slope, ch.Lower.Slope)
	}
	if !approx(ch.Upper.Intercept, 9.5) || !approx(ch.Lower.Intercept, 8.5) {
		t.Fatalf("expected intercepts 9.5/8.5, got %v/%v", ch.Upper.Intercept, ch.Lower.Intercept)
	}
}

func TestParallelChannel_MidIsBetween(t *testing.T) {
	ch, ok := ParallelChannel(risingChannelCandles(), 1, pip, 0)
	if !ok {
		t.Fatal("expected ok=true")
	}
	mid := ch.Mid()
	if !approx(mid.Slope, 0.5) || !approx(mid.Intercept, 9.0) {
		t.Fatalf("mid expected slope=0.5 intercept=9.0, got %+v", mid)
	}
	// At x=10: upper=14.5, lower=13.5, mid=14.0.
	if !approx(ch.Upper.At(10), 14.5) || !approx(ch.Lower.At(10), 13.5) || !approx(mid.At(10), 14.0) {
		t.Fatalf("at x=10: upper=%v lower=%v mid=%v", ch.Upper.At(10), ch.Lower.At(10), mid.At(10))
	}
}

func TestParallelChannel_Width(t *testing.T) {
	ch, _ := ParallelChannel(risingChannelCandles(), 1, pip, 0)
	if !approx(ch.Width(), 1.0) {
		t.Fatalf("expected width 1.0, got %v", ch.Width())
	}
}

func TestParallelChannel_InsufficientSwings(t *testing.T) {
	// Monotonic series → no interior pivots → cannot form a channel.
	highs := []float64{1, 2, 3, 4, 5}
	lows := []float64{0, 1, 2, 3, 4}
	if _, ok := ParallelChannel(candlesHL(highs, lows), 1, pip, 0); ok {
		t.Fatal("monotonic data: expected ok=false (no swings)")
	}
}

// nonCollinearChannelCandles is risingChannelCandles with the idx-4 swing high
// bumped 10.5→11.7 so the highs are NOT collinear (they were on y=0.5x+9.5).
// A centroid line would run through the middle of the highs; a true envelope
// must sit on top of them.
func nonCollinearChannelCandles() []market.Candle {
	highs := []float64{9.8, 9.6, 10.5, 10.3, 11.7, 11.3, 12.5, 12.3, 12.4}
	lows := []float64{9.4, 9.0, 10.2, 10.0, 11.2, 11.0, 12.2, 12.0, 12.1}
	return candlesHL(highs, lows)
}

func TestParallelChannel_EnvelopeContainsAllSwings(t *testing.T) {
	c := nonCollinearChannelCandles()
	ch, ok := ParallelChannel(c, 1, pip, 0)
	if !ok {
		t.Fatal("expected ok=true")
	}
	// Envelope contract: every swing high at-or-below Upper, every swing low
	// at-or-above Lower (price stays inside the channel).
	for _, s := range SwingHighs(c, 1, pip, 0) {
		if s.Price > ch.Upper.At(float64(s.Index))+1e-9 {
			t.Fatalf("swing high %v pokes above Upper %v at idx %d", s.Price, ch.Upper.At(float64(s.Index)), s.Index)
		}
	}
	for _, s := range SwingLows(c, 1, pip, 0) {
		if s.Price < ch.Lower.At(float64(s.Index))-1e-9 {
			t.Fatalf("swing low %v pokes below Lower %v at idx %d", s.Price, ch.Lower.At(float64(s.Index)), s.Index)
		}
	}
	// idx-4 high (11.7) is the binding top → Upper is tangent there.
	if !approx(ch.Upper.At(4), 11.7) {
		t.Fatalf("Upper.At(4) expected 11.7 (envelope tangent), got %v", ch.Upper.At(4))
	}
	// width = upper intercept 9.7 − lower intercept 8.5 = 1.2 (not the centroid 1.067).
	if !approx(ch.Width(), 1.2) {
		t.Fatalf("expected envelope width 1.2, got %v", ch.Width())
	}
}

func TestParallelChannel_NonFiniteRejected(t *testing.T) {
	highs := []float64{9, 14, math.Inf(1), 13, 8, 16, 7}
	lows := []float64{7, 12, 8, 11, 6, 14, 5}
	if _, ok := ParallelChannel(candlesHL(highs, lows), 1, pip, 0); ok {
		t.Fatal("Inf candle: expected ok=false")
	}
}
