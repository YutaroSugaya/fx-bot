package ta

import "fx-bot/backend/internal/domain/market"

// Channel is a parallel price channel: an Upper line through the swing highs
// and a Lower line through the swing lows, sharing a common slope. The mid
// line (Mid) is the average of the two — the mid-line TP2 target. Width is the
// constant vertical gap, a proxy for trend "strength".
type Channel struct {
	Upper Line
	Lower Line
}

// Mid returns the channel's middle line (same slope, intercept halfway between
// Upper and Lower).
//
// TODO(mtf_pullback scale-out): intentionally inert in production — only the
// deferred mid-line TP2 (分割利確) of mtf_pullback will consume it. Kept (with
// channel_test.go coverage) so the math is ready when that ships; do not delete
// as "dead code".
func (c Channel) Mid() Line {
	return Line{Slope: c.Upper.Slope, Intercept: (c.Upper.Intercept + c.Lower.Intercept) / 2}
}

// Width is the vertical distance between Upper and Lower. Since the two lines
// are parallel this is constant in x (= difference of intercepts).
func (c Channel) Width() float64 { return c.Upper.Intercept - c.Lower.Intercept }

// ParallelChannel builds a parallel channel from a candle window: it detects
// swing highs and lows (see SwingHighs/SwingLows), fits a regression line
// through each to get a direction, then forces them parallel by adopting the
// average of the two slopes. At that common slope the lines are positioned as
// an ENVELOPE (like a hand-drawn channel): Upper is the highest tangent — it
// sits at or above every swing high — and Lower the lowest tangent — at or
// below every swing low. Price therefore stays inside [Lower, Upper], and
// Width measures the full channel height (the "strength" proxy).
//
// ok=false when there are fewer than 2 swing highs or fewer than 2 swing lows
// (a slope needs at least two points on each side). n / minProminencePips are
// passed straight through to the swing detector.
func ParallelChannel(candles []market.Candle, n int, pipSize, minProminencePips float64) (Channel, bool) {
	highs := SwingHighs(candles, n, pipSize, minProminencePips)
	lows := SwingLows(candles, n, pipSize, minProminencePips)
	if len(highs) < 2 || len(lows) < 2 {
		return Channel{}, false
	}
	highLine, ok := FitLine(swingPoints(highs))
	if !ok {
		return Channel{}, false
	}
	lowLine, ok := FitLine(swingPoints(lows))
	if !ok {
		return Channel{}, false
	}
	slope := (highLine.Slope + lowLine.Slope) / 2
	return Channel{
		Upper: Line{Slope: slope, Intercept: tangentIntercept(highs, slope, true)},
		Lower: Line{Slope: slope, Intercept: tangentIntercept(lows, slope, false)},
	}, true
}

func swingPoints(swings []Swing) []Point {
	pts := make([]Point, len(swings))
	for i, s := range swings {
		pts[i] = Point{X: float64(s.Index), Y: s.Price}
	}
	return pts
}

// tangentIntercept returns the intercept b of the line y = slope*x + b that is
// tangent to the swing cloud at the given fixed slope. For upper=true it is the
// MAX of (price - slope*x) over the swings, so the line touches the topmost
// swing and lies at or above all others; for upper=false it is the MIN, the
// supporting line below the lows. This makes the channel an envelope, not a
// centroid regression line.
func tangentIntercept(swings []Swing, slope float64, upper bool) float64 {
	best := swings[0].Price - slope*float64(swings[0].Index)
	for _, s := range swings[1:] {
		b := s.Price - slope*float64(s.Index)
		if (upper && b > best) || (!upper && b < best) {
			best = b
		}
	}
	return best
}
