package ta

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestFitLine_TooFewPoints(t *testing.T) {
	if _, ok := FitLine(nil); ok {
		t.Fatal("nil points: expected ok=false")
	}
	if _, ok := FitLine([]Point{{X: 1, Y: 2}}); ok {
		t.Fatal("single point: expected ok=false (slope undefined)")
	}
}

func TestFitLine_PerfectAscendingLine(t *testing.T) {
	// y = 2x + 1
	pts := []Point{{0, 1}, {1, 3}, {2, 5}, {3, 7}}
	l, ok := FitLine(pts)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !approx(l.Slope, 2) || !approx(l.Intercept, 1) {
		t.Fatalf("expected slope=2 intercept=1, got slope=%v intercept=%v", l.Slope, l.Intercept)
	}
}

func TestFitLine_Horizontal(t *testing.T) {
	pts := []Point{{0, 5}, {1, 5}, {2, 5}}
	l, ok := FitLine(pts)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !approx(l.Slope, 0) || !approx(l.Intercept, 5) {
		t.Fatalf("expected slope=0 intercept=5, got slope=%v intercept=%v", l.Slope, l.Intercept)
	}
}

func TestFitLine_VerticalIsRejected(t *testing.T) {
	// All X identical → slope undefined (denom == 0) → ok=false.
	pts := []Point{{2, 1}, {2, 3}, {2, 9}}
	if _, ok := FitLine(pts); ok {
		t.Fatal("vertical point set: expected ok=false")
	}
}

func TestFitLine_NoisyBestFit(t *testing.T) {
	// Points scattered around y = x: (0,0),(1,1),(2,2),(3,3) plus a symmetric
	// pair of outliers that cancel → OLS slope stays 1, intercept 0.
	pts := []Point{{0, 0.5}, {0, -0.5}, {1, 1}, {2, 2}, {3, 3}}
	l, ok := FitLine(pts)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if math.Abs(l.Slope-1) > 1e-6 {
		t.Fatalf("expected slope≈1, got %v", l.Slope)
	}
}

func TestLine_At_Extrapolate(t *testing.T) {
	l := Line{Slope: 2, Intercept: 1} // y = 2x+1
	if got := l.At(10); !approx(got, 21) {
		t.Fatalf("At(10): expected 21, got %v", got)
	}
	if got := l.At(-5); !approx(got, -9) {
		t.Fatalf("At(-5): expected -9, got %v", got)
	}
}

func TestFitLine_NonFiniteRejected(t *testing.T) {
	// A NaN/Inf input must not yield a "valid" line — ok=true would break the
	// ok ⇒ tradable-line contract and feed NaN into SL/TP price levels.
	if _, ok := FitLine([]Point{{0, 1}, {1, math.NaN()}, {2, 3}}); ok {
		t.Fatal("NaN point: expected ok=false")
	}
	if _, ok := FitLine([]Point{{0, 1}, {1, math.Inf(1)}, {2, 3}}); ok {
		t.Fatal("+Inf point: expected ok=false")
	}
	if _, ok := FitLine([]Point{{math.NaN(), 1}, {1, 2}, {2, 3}}); ok {
		t.Fatal("NaN in X: expected ok=false")
	}
}
