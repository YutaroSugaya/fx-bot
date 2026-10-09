package ta

import (
	"math"
	"testing"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
)

// rc builds a flat candle whose High/Low/Open/Close all equal v, so that the
// close-driven RSI and the High/Low-driven swing detector see the same path.
func rc(v float64) market.Candle {
	return market.Candle{Symbol: "USD_JPY", Open: v, High: v, Low: v, Close: v}
}

func closesFrom(vals ...float64) []float64 { return vals }

func TestRSI_MonotonicUp_Is100(t *testing.T) {
	if r, ok := RSI(closesFrom(1, 2, 3, 4, 5), 2); !ok || !nearF(r, 100, 1e-9) {
		t.Fatalf("monotonic-up RSI expected 100, got %v ok=%v", r, ok)
	}
}

func TestRSI_MonotonicDown_Is0(t *testing.T) {
	if r, ok := RSI(closesFrom(5, 4, 3, 2, 1), 2); !ok || !nearF(r, 0, 1e-9) {
		t.Fatalf("monotonic-down RSI expected 0, got %v ok=%v", r, ok)
	}
}

func TestRSI_FlatSeries_Undefined(t *testing.T) {
	if _, ok := RSI(closesFrom(5, 5, 5, 5), 2); ok {
		t.Fatalf("a flat series carries no momentum — RSI must be undefined")
	}
}

func TestRSI_TooShort_Undefined(t *testing.T) {
	if _, ok := RSI(closesFrom(1, 2), 3); ok {
		t.Fatalf("fewer than period+1 closes must be undefined")
	}
}

// TestRSI_Wilder_KnownValues pins the Wilder seeding + smoothing on a tiny
// period-2 series [10,11,10,11,12]: changes +1,-1,+1,+1.
//
//	seed @idx2: avgGain .5 avgLoss .5 -> RSI 50
//	@idx3:      avgGain .75 avgLoss .25 -> RSI 75
//	@idx4:      avgGain .875 avgLoss .125 -> RSI 87.5
func TestRSI_Wilder_KnownValues(t *testing.T) {
	series, validFrom, ok := RSISeries(closesFrom(10, 11, 10, 11, 12), 2)
	if !ok || validFrom != 2 {
		t.Fatalf("RSISeries ok=%v validFrom=%d (want ok,2)", ok, validFrom)
	}
	for idx, want := range map[int]float64{2: 50, 3: 75, 4: 87.5} {
		if !nearF(series[idx], want, 1e-6) {
			t.Errorf("rsi[%d]=%v want %v", idx, series[idx], want)
		}
	}
	if r, ok := RSI(closesFrom(10, 11, 10, 11, 12), 2); !ok || !nearF(r, 87.5, 1e-6) {
		t.Errorf("latest RSI=%v ok=%v want 87.5", r, ok)
	}
}

// bearish regular divergence: price prints a higher high (103 -> 104) while RSI
// makes a lower high (100 -> ~81.7).
func bearishDivergenceSeries() []market.Candle {
	vals := []float64{100, 100, 100, 101, 103, 101, 102, 103, 104, 103, 102}
	cs := make([]market.Candle, len(vals))
	for i, v := range vals {
		cs[i] = rc(v)
	}
	return cs
}

func TestRegularDivergence_BearishTrue(t *testing.T) {
	if !RegularDivergence(bearishDivergenceSeries(), order.SideSell, 3, 1, 0.01, 0) {
		t.Fatalf("higher high + lower RSI must be bearish divergence")
	}
}

func TestRegularDivergence_FalseOnLowerHigh(t *testing.T) {
	// Second swing high (103) is BELOW the first (104): not a higher high, so no
	// regular bearish divergence regardless of RSI.
	vals := []float64{100, 100, 100, 104, 101, 103, 101}
	cs := make([]market.Candle, len(vals))
	for i, v := range vals {
		cs[i] = rc(v)
	}
	if RegularDivergence(cs, order.SideSell, 3, 1, 0.01, 0) {
		t.Fatalf("a lower second high must NOT be bearish divergence")
	}
}

func TestRegularDivergence_BullishTrue(t *testing.T) {
	// Mirror of the bearish fixture: lower low (97 -> 96) with a higher RSI low.
	vals := []float64{100, 100, 100, 99, 97, 99, 98, 97, 96, 97, 98}
	cs := make([]market.Candle, len(vals))
	for i, v := range vals {
		cs[i] = rc(v)
	}
	if !RegularDivergence(cs, order.SideBuy, 3, 1, 0.01, 0) {
		t.Fatalf("lower low + higher RSI must be bullish divergence")
	}
}

func TestRegularDivergence_FalseWithOneSwing(t *testing.T) {
	// A single clean spike has only one swing high — nothing to diverge against.
	vals := []float64{100, 100, 100, 101, 102, 103, 102}
	cs := make([]market.Candle, len(vals))
	for i, v := range vals {
		cs[i] = rc(v)
	}
	if RegularDivergence(cs, order.SideSell, 3, 1, 0.01, 0) {
		t.Fatalf("fewer than two swings must return false")
	}
}

func nearF(a, b, tol float64) bool { return math.Abs(a-b) <= tol }
