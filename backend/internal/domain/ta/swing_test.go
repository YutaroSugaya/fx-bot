package ta

import (
	"math"
	"testing"
	"time"

	"fx-bot/backend/internal/domain/market"
)

// candlesHL builds candles from parallel high/low slices. Open/Close are set
// inside [low,high]; swing detection only reads High/Low + slice order.
func candlesHL(highs, lows []float64) []market.Candle {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]market.Candle, len(highs))
	for i := range highs {
		out[i] = market.Candle{
			Symbol:   "USD_JPY",
			Interval: time.Minute,
			OpenTime: t0.Add(time.Duration(i) * time.Minute),
			High:     highs[i],
			Low:      lows[i],
			Open:     lows[i],
			Close:    highs[i],
		}
	}
	return out
}

const pip = 0.01

func TestSwingHighs_DetectsClearPeak(t *testing.T) {
	// Peak at index 2 (High 15 dominates ±1 neighbors).
	highs := []float64{10, 12, 15, 11, 9}
	lows := []float64{8, 10, 13, 9, 7}
	got := SwingHighs(candlesHL(highs, lows), 1, pip, 0)
	if len(got) != 1 {
		t.Fatalf("expected 1 swing high, got %d: %+v", len(got), got)
	}
	if got[0].Index != 2 || !approx(got[0].Price, 15) || got[0].Kind != SwingHigh {
		t.Fatalf("unexpected swing: %+v", got[0])
	}
}

func TestSwingLows_DetectsClearTrough(t *testing.T) {
	// Trough at index 2 (Low 5 dominates ±1 neighbors).
	highs := []float64{12, 10, 7, 9, 11}
	lows := []float64{10, 8, 5, 7, 9}
	got := SwingLows(candlesHL(highs, lows), 1, pip, 0)
	if len(got) != 1 {
		t.Fatalf("expected 1 swing low, got %d: %+v", len(got), got)
	}
	if got[0].Index != 2 || !approx(got[0].Price, 5) || got[0].Kind != SwingLow {
		t.Fatalf("unexpected swing: %+v", got[0])
	}
}

func TestSwingHighs_EdgesExcluded(t *testing.T) {
	// Highest bar is index 0 (a boundary): with n=1 it cannot be a swing
	// because it has no left neighbor.
	highs := []float64{20, 12, 10, 11, 9}
	lows := []float64{18, 10, 8, 9, 7}
	got := SwingHighs(candlesHL(highs, lows), 1, pip, 0)
	for _, s := range got {
		if s.Index == 0 {
			t.Fatalf("boundary index 0 must not be a swing: %+v", got)
		}
	}
}

func TestSwingHighs_ProminenceGateFiltersShallowPivot(t *testing.T) {
	// Pivot at index 1: High 10.05. Neighbor lows 9.99 (left) and 10.01 (right).
	// leftDrop=6 pips, rightDrop=4 pips → prominence = min = 4 pips.
	highs := []float64{10.00, 10.05, 10.02}
	lows := []float64{9.99, 10.04, 10.01}
	c := candlesHL(highs, lows)
	if got := SwingHighs(c, 1, pip, 2); len(got) != 1 {
		t.Fatalf("threshold 2 pips (< 4): expected pivot kept, got %d", len(got))
	}
	if got := SwingHighs(c, 1, pip, 5); len(got) != 0 {
		t.Fatalf("threshold 5 pips (> 4): expected pivot filtered, got %d: %+v", len(got), got)
	}
}

func TestSwingHighs_WiderNNeedsBroaderPeak(t *testing.T) {
	// Index 2 beats its ±1 neighbors but NOT index 0 (also 15). With n=2 it is
	// not a strict local max over the full window → rejected.
	highs := []float64{15, 12, 15, 11, 9}
	lows := []float64{13, 10, 13, 9, 7}
	if got := SwingHighs(candlesHL(highs, lows), 1, pip, 0); len(got) != 1 || got[0].Index != 2 {
		t.Fatalf("n=1: expected swing at 2, got %+v", got)
	}
	if got := SwingHighs(candlesHL(highs, lows), 2, pip, 0); len(got) != 0 {
		t.Fatalf("n=2: expected no swing (tie with index 0), got %+v", got)
	}
}

func TestSwingHighs_MultiplePeaksInIndexOrder(t *testing.T) {
	highs := []float64{9, 14, 10, 13, 8, 16, 7}
	lows := []float64{7, 12, 8, 11, 6, 14, 5}
	got := SwingHighs(candlesHL(highs, lows), 1, pip, 0)
	if len(got) != 3 {
		t.Fatalf("expected 3 peaks (idx 1,3,5), got %d: %+v", len(got), got)
	}
	for i, want := range []int{1, 3, 5} {
		if got[i].Index != want {
			t.Fatalf("peak %d: expected index %d, got %d", i, want, got[i].Index)
		}
	}
}

func TestSwings_DegenerateInputs(t *testing.T) {
	highs := []float64{10, 12, 11}
	lows := []float64{8, 10, 9}
	if got := SwingHighs(candlesHL(highs, lows), 0, pip, 0); got != nil {
		t.Fatalf("n=0: expected nil, got %+v", got)
	}
	// Too few candles for n=2 (need 2 each side) → nil.
	if got := SwingHighs(candlesHL(highs, lows), 2, pip, 0); got != nil {
		t.Fatalf("insufficient candles: expected nil, got %+v", got)
	}
	if got := SwingHighs(nil, 1, pip, 0); got != nil {
		t.Fatalf("nil candles: expected nil, got %+v", got)
	}
}

func TestSwingHighs_NonFiniteBarNotASwing(t *testing.T) {
	// A corrupt bar (High=NaN) must not become a swing, and must not let its
	// finite neighbours pass as strict extrema either (IEEE: every compare with
	// NaN is false). Same for +Inf.
	for _, bad := range []float64{math.NaN(), math.Inf(1)} {
		highs := []float64{10, 12, bad, 11, 9}
		lows := []float64{8, 10, 9, 9, 7}
		got := SwingHighs(candlesHL(highs, lows), 1, pip, 0)
		for _, s := range got {
			if math.IsNaN(s.Price) || math.IsInf(s.Price, 0) {
				t.Fatalf("bad=%v: non-finite swing emitted: %+v", bad, s)
			}
			if s.Index == 1 || s.Index == 2 || s.Index == 3 {
				t.Fatalf("bad=%v: bar in the corrupt window must not be a swing: %+v", bad, s)
			}
		}
	}
}

func TestSwingHighs_ProminenceGateRequiresPipSize(t *testing.T) {
	highs := []float64{10.00, 10.05, 10.02}
	lows := []float64{9.99, 10.04, 10.01}
	c := candlesHL(highs, lows)
	// Gate requested (minProminencePips>0) but no usable pip scale → refuse
	// rather than silently emit ungated swings (spec: avoid ザル化).
	if got := SwingHighs(c, 1, 0, 5); got != nil {
		t.Fatalf("pipSize=0 with gate requested: expected nil, got %+v", got)
	}
	if got := SwingHighs(c, 1, -0.01, 5); got != nil {
		t.Fatalf("negative pipSize with gate requested: expected nil, got %+v", got)
	}
	// No gate requested → pipSize is irrelevant, plain fractal detection runs.
	if got := SwingHighs(c, 1, 0, 0); len(got) != 1 {
		t.Fatalf("no gate, pipSize=0: expected 1 swing, got %+v", got)
	}
}
