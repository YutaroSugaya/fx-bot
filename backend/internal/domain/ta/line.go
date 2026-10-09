// Package ta holds pure technical-analysis geometry primitives used by the
// multi-timeframe strategies (mtf_pullback, ma_pullback): swing/pivot
// detection, least-squares line fitting + extrapolation, and parallel
// channels. Everything here is deterministic and side-effect free — it
// operates on slices of market.Candle and plain points, never on I/O.
//
// These replace the discretionary "draw a trendline" step of the source
// strategies with a numeric proxy: a trendline is the least-squares line
// through a set of swing points (see TouchedLine), extrapolated to the
// current bar with Line.At.
package ta

import "math"

// Point is one (x, y) sample for line fitting. For candle-derived swings x is
// the candle index (bars are uniformly spaced in time within a timeframe, so
// index is proportional to time) and y is the price.
type Point struct {
	X float64
	Y float64
}

// Line is y = Slope*x + Intercept, the least-squares fit through a point set.
type Line struct {
	Slope     float64
	Intercept float64
}

// At returns the line's y at x. With x = current bar index this is the
// extrapolation of a fitted trendline to "now" (the method's extrapolate()).
func (l Line) At(x float64) float64 { return l.Slope*x + l.Intercept }

// FitLine returns the ordinary-least-squares line through pts.
//
// ok=false when the fit is undefined: fewer than 2 points, or every point
// shares the same X (a vertical set, where slope would be infinite). Callers
// treat ok=false as "no valid line" (the method's nil).
func FitLine(pts []Point) (Line, bool) {
	n := len(pts)
	if n < 2 {
		return Line{}, false
	}
	var sumX, sumY, sumXY, sumXX float64
	for _, p := range pts {
		sumX += p.X
		sumY += p.Y
		sumXY += p.X * p.Y
		sumXX += p.X * p.X
	}
	fn := float64(n)
	denom := fn*sumXX - sumX*sumX
	if denom == 0 {
		// All X equal → vertical line, slope undefined.
		return Line{}, false
	}
	slope := (fn*sumXY - sumX*sumY) / denom
	intercept := (sumY - slope*sumX) / fn
	// A non-finite result means a non-finite input (NaN/Inf High/Low) poisoned
	// the sums. Reject it: ok=true must always mean "a real, tradable line".
	// IEEE semantics make the denom==0 guard above miss this (NaN != 0).
	if !isFinite(slope) || !isFinite(intercept) {
		return Line{}, false
	}
	return Line{Slope: slope, Intercept: intercept}, true
}

func isFinite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
