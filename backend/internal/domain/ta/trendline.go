package ta

import "math"

// TouchedLine fits the least-squares line through a set of swing points and
// validates it as a real, repeatedly-respected trendline ("壁"):
//
//   - len(swings) must be >= minTouches. The spec requires >= 3 rebounds: a
//     line is only proven once price has been turned away from it several
//     times, so a 2-point fit (any two points are collinear) is not a wall.
//   - The least-squares fit must exist (not vertical / degenerate).
//   - The RMS residual of the touches around the line must be <=
//     maxResidualPips. If the points do not actually lie on a straight line
//     the fit is rejected — this is what stops a loose scatter of pivots from
//     masquerading as a clean trendline. maxResidualPips <= 0 disables this
//     gate (fit-existence + count only).
//
// ok=false on any failure (the method's nil = "no valid line, do not trade").
func TouchedLine(swings []Swing, minTouches int, maxResidualPips, pipSize float64) (Line, bool) {
	if len(swings) < minTouches {
		return Line{}, false
	}
	pts := swingPoints(swings)
	line, ok := FitLine(pts)
	if !ok {
		return Line{}, false
	}
	if maxResidualPips > 0 && pipSize > 0 {
		if rmsResidual(pts, line) > maxResidualPips*pipSize {
			return Line{}, false
		}
	}
	return line, true
}

// rmsResidual is the root-mean-square vertical distance of pts from line.
func rmsResidual(pts []Point, line Line) float64 {
	if len(pts) == 0 {
		return 0
	}
	var ssq float64
	for _, p := range pts {
		d := p.Y - line.At(p.X)
		ssq += d * d
	}
	return math.Sqrt(ssq / float64(len(pts)))
}
