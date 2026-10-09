package analysis

import (
	"math"
	"testing"
)

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestSharpeRatio(t *testing.T) {
	// [1,2,3]: mean 2, sample std (ddof=1) = 1 → SR 2.
	if sr, ok := SharpeRatio([]float64{1, 2, 3}); !ok || !approx(sr, 2, 1e-12) {
		t.Fatalf("SharpeRatio=%v ok=%v want 2", sr, ok)
	}
	// zero variance → undefined.
	if _, ok := SharpeRatio([]float64{5, 5, 5}); ok {
		t.Fatalf("zero-variance returns must be undefined")
	}
	if _, ok := SharpeRatio([]float64{1}); ok {
		t.Fatalf("fewer than 2 returns must be undefined")
	}
}

func TestNormalCDF(t *testing.T) {
	if !approx(normalCDF(0), 0.5, 1e-12) {
		t.Errorf("Phi(0) want 0.5, got %v", normalCDF(0))
	}
	if !approx(normalCDF(1.959963985), 0.975, 1e-6) {
		t.Errorf("Phi(1.96) want 0.975, got %v", normalCDF(1.959963985))
	}
	if !approx(normalCDF(-1.959963985), 0.025, 1e-6) {
		t.Errorf("Phi(-1.96) want 0.025, got %v", normalCDF(-1.959963985))
	}
}

func TestInverseNormalCDF(t *testing.T) {
	if !approx(inverseNormalCDF(0.5), 0, 1e-9) {
		t.Errorf("Zinv(0.5) want 0, got %v", inverseNormalCDF(0.5))
	}
	if !approx(inverseNormalCDF(0.975), 1.959963985, 1e-4) {
		t.Errorf("Zinv(0.975) want 1.95996, got %v", inverseNormalCDF(0.975))
	}
	// round-trip Phi(Zinv(p)) ≈ p.
	for _, p := range []float64{0.1, 0.3, 0.8, 0.99} {
		if !approx(normalCDF(inverseNormalCDF(p)), p, 1e-6) {
			t.Errorf("round-trip p=%v -> %v", p, normalCDF(inverseNormalCDF(p)))
		}
	}
}

func TestProbabilisticSharpeRatio(t *testing.T) {
	r := []float64{2, -1, 3, -0.5, 1.5, -1, 2.5, 0.5, -0.5, 2, 1, -1.5, 2, 0, 1}
	srHat, ok := SharpeRatio(r)
	if !ok {
		t.Fatal("SharpeRatio undefined")
	}
	// At benchmark = own SR the numerator is 0 → PSR = 0.5 exactly.
	if psr, ok := ProbabilisticSharpeRatio(r, srHat); !ok || !approx(psr, 0.5, 1e-9) {
		t.Errorf("PSR(SR_hat) want 0.5, got %v ok=%v", psr, ok)
	}
	// A very low benchmark → near-certain the true SR beats it.
	if psr, _ := ProbabilisticSharpeRatio(r, -5); psr < 0.999 {
		t.Errorf("PSR(-5) want ~1, got %v", psr)
	}
	// A very high benchmark → near-impossible.
	if psr, _ := ProbabilisticSharpeRatio(r, 5); psr > 0.001 {
		t.Errorf("PSR(5) want ~0, got %v", psr)
	}
}

func TestDeflatedSharpeRatio_MoreTrialsIsHarder(t *testing.T) {
	// Clearly positive returns.
	r := make([]float64, 60)
	for i := range r {
		if i%3 == 0 {
			r[i] = -1
		} else {
			r[i] = 2
		}
	}
	v := 0.5 // variance of trial Sharpes across the search
	d1, ok1 := DeflatedSharpeRatio(r, 1, v)
	d50, ok50 := DeflatedSharpeRatio(r, 50, v)
	if !ok1 || !ok50 {
		t.Fatal("DSR undefined")
	}
	if !(d1 > d50) {
		t.Errorf("more trials must lower the DSR: d1=%v d50=%v", d1, d50)
	}
	for _, d := range []float64{d1, d50} {
		if d < 0 || d > 1 {
			t.Errorf("DSR must be a probability in [0,1], got %v", d)
		}
	}
	// nTrials < 1 is invalid.
	if _, ok := DeflatedSharpeRatio(r, 0, v); ok {
		t.Errorf("nTrials<1 must be undefined")
	}
}
