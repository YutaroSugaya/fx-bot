package ta

import (
	"math"
	"testing"
)

func mkSwings(indices []int, prices []float64, kind SwingKind) []Swing {
	out := make([]Swing, len(indices))
	for i := range indices {
		out[i] = Swing{Index: indices[i], Price: prices[i], Kind: kind}
	}
	return out
}

func TestTouchedLine_TooFewTouches(t *testing.T) {
	sw := mkSwings([]int{0, 2}, []float64{10.00, 10.10}, SwingHigh)
	if _, ok := TouchedLine(sw, 3, 0, pip); ok {
		t.Fatal("2 touches with minTouches=3: expected ok=false (wall not yet proven)")
	}
}

func TestTouchedLine_ThreeCollinearTouches(t *testing.T) {
	// idx 0,2,4 prices on a perfect line: slope 0.05/idx, intercept 10.00.
	sw := mkSwings([]int{0, 2, 4}, []float64{10.00, 10.10, 10.20}, SwingHigh)
	l, ok := TouchedLine(sw, 3, 5, pip)
	if !ok {
		t.Fatal("3 collinear touches: expected ok=true")
	}
	if !approx(l.Slope, 0.05) || !approx(l.Intercept, 10.00) {
		t.Fatalf("expected slope=0.05 intercept=10.00, got %+v", l)
	}
}

func TestTouchedLine_RejectsPointsNotOnALine(t *testing.T) {
	// Middle touch bulges off the line → RMS residual ≈ 9.4 pips.
	sw := mkSwings([]int{0, 2, 4}, []float64{10.00, 10.30, 10.20}, SwingHigh)
	if _, ok := TouchedLine(sw, 3, 5, pip); ok {
		t.Fatal("maxResidual=5 pips (< 9.4): expected ok=false (not a real line)")
	}
	if _, ok := TouchedLine(sw, 3, 15, pip); !ok {
		t.Fatal("maxResidual=15 pips (> 9.4): expected ok=true")
	}
}

func TestTouchedLine_ResidualGateDisabled(t *testing.T) {
	// maxResidualPips <= 0 disables the residual check: any ≥minTouches set that
	// fits at all is accepted.
	sw := mkSwings([]int{0, 2, 4}, []float64{10.00, 10.30, 10.20}, SwingHigh)
	if _, ok := TouchedLine(sw, 3, 0, pip); !ok {
		t.Fatal("residual gate disabled: expected ok=true")
	}
}

func TestTouchedLine_VerticalRejected(t *testing.T) {
	// All touches at the same index (degenerate) → FitLine fails → ok=false.
	sw := mkSwings([]int{3, 3, 3}, []float64{10.00, 10.10, 10.20}, SwingLow)
	if _, ok := TouchedLine(sw, 3, 0, pip); ok {
		t.Fatal("vertical touch set: expected ok=false")
	}
}

func TestTouchedLine_NonFiniteRejected(t *testing.T) {
	// A NaN touch price must not produce a "valid" line, even with the residual
	// gate disabled (the NaN residual silently passes the gate otherwise).
	sw := mkSwings([]int{0, 2, 4}, []float64{10.00, math.NaN(), 10.20}, SwingHigh)
	if _, ok := TouchedLine(sw, 3, 0, pip); ok {
		t.Fatal("NaN touch, gate off: expected ok=false")
	}
	if _, ok := TouchedLine(sw, 3, 5, pip); ok {
		t.Fatal("NaN touch, gate on: expected ok=false")
	}
}
