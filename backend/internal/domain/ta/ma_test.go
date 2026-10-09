package ta

import (
	"math"
	"testing"
)

func TestSMA_TooFewValues(t *testing.T) {
	if _, ok := SMA([]float64{1, 2}, 3); ok {
		t.Fatal("len<period: expected ok=false")
	}
	if _, ok := SMA(nil, 1); ok {
		t.Fatal("nil values: expected ok=false")
	}
}

func TestSMA_BadPeriod(t *testing.T) {
	if _, ok := SMA([]float64{1, 2, 3}, 0); ok {
		t.Fatal("period 0: expected ok=false")
	}
	if _, ok := SMA([]float64{1, 2, 3}, -1); ok {
		t.Fatal("period -1: expected ok=false")
	}
}

func TestSMA_Constants(t *testing.T) {
	v, ok := SMA([]float64{5, 5, 5, 5, 5}, 3)
	if !ok || !approx(v, 5) {
		t.Fatalf("constant series: expected 5/true, got %v/%v", v, ok)
	}
}

func TestSMA_LastPeriodOnly(t *testing.T) {
	// Mean of the LAST `period` values, not the whole slice.
	// last 3 of 1..10 = (8+9+10)/3 = 9.
	v, ok := SMA([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 3)
	if !ok || !approx(v, 9) {
		t.Fatalf("expected mean of last 3 = 9, got %v/%v", v, ok)
	}
	// Whole-slice when period==len.
	v, ok = SMA([]float64{2, 4, 6}, 3)
	if !ok || !approx(v, 4) {
		t.Fatalf("expected 4, got %v/%v", v, ok)
	}
}

func TestEMA_TooFewValues(t *testing.T) {
	if _, ok := EMA([]float64{1, 2}, 3); ok {
		t.Fatal("len<period: expected ok=false")
	}
}

func TestEMA_BadPeriod(t *testing.T) {
	if _, ok := EMA([]float64{1, 2, 3}, 0); ok {
		t.Fatal("period 0: expected ok=false")
	}
}

func TestEMA_Constants(t *testing.T) {
	// An EMA of a constant series equals the constant (seed = SMA = c, then
	// every step keeps it at c).
	v, ok := EMA([]float64{7, 7, 7, 7, 7, 7}, 4)
	if !ok || !approx(v, 7) {
		t.Fatalf("constant series: expected 7/true, got %v/%v", v, ok)
	}
}

func TestEMA_KnownSmallCase(t *testing.T) {
	// values [1,2,3,4], period 2. k = 2/(2+1) = 2/3.
	// seed EMA[1] = SMA([1,2]) = 1.5
	// EMA[2] = 3*2/3 + 1.5*1/3 = 2 + 0.5     = 2.5
	// EMA[3] = 4*2/3 + 2.5*1/3 = 2.6667 + 0.8333 = 3.5
	v, ok := EMA([]float64{1, 2, 3, 4}, 2)
	if !ok || math.Abs(v-3.5) > 1e-9 {
		t.Fatalf("expected 3.5, got %v/%v", v, ok)
	}
}

func TestEMA_Period1IsLastValue(t *testing.T) {
	// period 1 → k=1 → EMA tracks the latest value exactly.
	v, ok := EMA([]float64{10, 20, 30}, 1)
	if !ok || !approx(v, 30) {
		t.Fatalf("expected 30, got %v/%v", v, ok)
	}
}

func TestSMAEMA_LargeWindow200(t *testing.T) {
	// At the real period (200) the strategy depends on, verify both MAs on a
	// 220-bar series. A constant tail makes the expected values exact: SMA/EMA
	// of a constant window both equal the constant.
	flat := make([]float64, 220)
	for i := range flat {
		flat[i] = 152.345
	}
	if v, ok := SMA(flat, 200); !ok || !approx(v, 152.345) {
		t.Fatalf("SMA200 of constant: expected 152.345/true, got %v/%v", v, ok)
	}
	if v, ok := EMA(flat, 200); !ok || !approx(v, 152.345) {
		t.Fatalf("EMA200 of constant: expected 152.345/true, got %v/%v", v, ok)
	}
	// On a steady 200-bar ramp the EMA (recency-weighted) sits above the SMA and
	// both stay finite & inside the data range — guards numerical drift at scale.
	ramp := make([]float64, 220)
	for i := range ramp {
		ramp[i] = 150.00 + float64(i)*0.01
	}
	sma, _ := SMA(ramp, 200)
	ema, _ := EMA(ramp, 200)
	if !(ema > sma) {
		t.Fatalf("EMA200(%.4f) should exceed SMA200(%.4f) on a rising ramp", ema, sma)
	}
	last := ramp[len(ramp)-1]
	if ema > last || sma > last || sma < ramp[0] {
		t.Fatalf("MAs out of range: sma=%.4f ema=%.4f range=[%.2f,%.2f]", sma, ema, ramp[0], last)
	}
}

func TestEMA_ReactsFasterThanSMA(t *testing.T) {
	// On a fresh up-move the EMA (recency-weighted) should sit above the SMA.
	vals := []float64{1, 1, 1, 1, 1, 2, 3, 4, 5, 6}
	sma, ok1 := SMA(vals, 5)
	ema, ok2 := EMA(vals, 5)
	if !ok1 || !ok2 {
		t.Fatalf("expected ok, got %v %v", ok1, ok2)
	}
	if !(ema > sma) {
		t.Fatalf("on a rising tail EMA(%.3f) should exceed SMA(%.3f)", ema, sma)
	}
}

// TestSMASeries_MatchesScalar is the key guard: the rolling series MUST equal
// the scalar SMA/EMA at every defined index, so a chart overlay built from the
// series matches the single MA value the strategy trades on. (This is why the
// series math lives here in domain/ta next to SMA/EMA — a single source of
// truth — rather than being re-derived in the read-model usecase.)
func TestSMASeries_MatchesScalar(t *testing.T) {
	// Rising ramp with a periodic wobble so SMA≠EMA, longer than the period.
	closes := make([]float64, 260)
	for i := range closes {
		closes[i] = 150.00 + float64(i)*0.01
		if i%7 == 0 {
			closes[i] -= 0.03
		}
	}
	const period = 200
	sma := SMASeries(closes, period)
	ema := EMASeries(closes, period)
	if len(sma) != len(closes) || len(ema) != len(closes) {
		t.Fatalf("series length: sma=%d ema=%d want %d", len(sma), len(ema), len(closes))
	}
	for i := range closes {
		wantSMA, okS := SMA(closes[:i+1], period)
		wantEMA, okE := EMA(closes[:i+1], period)
		if i < period-1 {
			// Before a full window: undefined → series carry 0, scalar reports !ok.
			if sma[i] != 0 || ema[i] != 0 {
				t.Fatalf("index %d before warmup: expected 0/0, got sma=%v ema=%v", i, sma[i], ema[i])
			}
			if okS || okE {
				t.Fatalf("index %d: scalar should be !ok before warmup", i)
			}
			continue
		}
		if !okS || !okE {
			t.Fatalf("index %d: scalar unexpectedly !ok", i)
		}
		if !approx(sma[i], wantSMA) {
			t.Errorf("SMASeries[%d] = %.6f, SMA = %.6f", i, sma[i], wantSMA)
		}
		if !approx(ema[i], wantEMA) {
			t.Errorf("EMASeries[%d] = %.6f, EMA = %.6f", i, ema[i], wantEMA)
		}
	}
}

func TestMASeries_TooFewValues(t *testing.T) {
	closes := []float64{1, 2, 3, 4, 5} // < period
	const period = 200
	sma := SMASeries(closes, period)
	ema := EMASeries(closes, period)
	if len(sma) != len(closes) || len(ema) != len(closes) {
		t.Fatalf("series should be 0-filled at input length, got sma=%d ema=%d", len(sma), len(ema))
	}
	for i := range closes {
		if sma[i] != 0 || ema[i] != 0 {
			t.Fatalf("index %d: expected 0/0 (no full window), got sma=%v ema=%v", i, sma[i], ema[i])
		}
	}
}

func TestSMASeries_SmallKnown(t *testing.T) {
	// period 3 over closes 1..6: SMA defined from index 2.
	// idx2=(1+2+3)/3=2, idx3=3, idx4=4, idx5=5.
	got := SMASeries([]float64{1, 2, 3, 4, 5, 6}, 3)
	want := []float64{0, 0, 2, 3, 4, 5}
	for i := range want {
		if !approx(got[i], want[i]) {
			t.Errorf("SMASeries[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestMASeries_BadPeriod(t *testing.T) {
	if got := SMASeries([]float64{1, 2, 3}, 0); len(got) != 3 || got[0] != 0 || got[2] != 0 {
		t.Errorf("period<1 should give 0-filled series, got %v", got)
	}
	if got := EMASeries([]float64{1, 2, 3}, 0); len(got) != 3 || got[0] != 0 || got[2] != 0 {
		t.Errorf("period<1 should give 0-filled series, got %v", got)
	}
}
