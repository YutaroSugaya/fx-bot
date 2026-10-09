package ta

import (
	"math"

	"fx-bot/backend/internal/domain/market"
)

// Signal primitives for the exhaustion-fade / mean-reversion family of
// strategies ("オーバーシュートの逆張り" + common professional fade
// practice). These are pure functions over a candle / close
// series — they describe the market, they do not decide trades. The
// deterministic DetectExhaustionFade detector (and any LLM context gate) is
// built on top of these; backtests read the same primitives so a live fill and
// a backtested fill see identical numbers.
//
// Every function follows the package ok-contract: ok=false means the value is
// undefined (not enough data / bad parameter), which callers treat as "no
// signal, do not act" rather than a usable zero.

// NetMovePips is the net close-to-close move over the last n candles, in pips:
// (close[last] - close[last-n]) / pipSize. Positive = up, negative = down. This
// is the "ガッと走った" velocity proxy — "X pips over the last K bars". ok=false
// when n < 1, fewer than n+1 candles, or pipSize <= 0.
func NetMovePips(candles []market.Candle, n int, pipSize float64) (float64, bool) {
	if n < 1 || pipSize <= 0 || len(candles) < n+1 {
		return 0, false
	}
	last := candles[len(candles)-1].Close
	prev := candles[len(candles)-1-n].Close
	return (last - prev) / pipSize, true
}

// DeviationATR measures how far price sits from a reference average (an MA), in
// ATR units: ((price - ma) / pipSize) / atrPips. Positive = stretched above the
// MA (a fade-short candidate), negative = stretched below (a fade-long
// candidate). Expressing the stretch in ATR units makes the overshoot threshold
// volatility-adaptive instead of a fixed pip distance. ok=false when atrPips <= 0
// or pipSize <= 0.
func DeviationATR(price, ma, atrPips, pipSize float64) (float64, bool) {
	if atrPips <= 0 || pipSize <= 0 {
		return 0, false
	}
	devPips := (price - ma) / pipSize
	return devPips / atrPips, true
}

// CandleShape decomposes one bar into body / wick geometry, all in pips. The
// wick-to-body ratios are the rejection signal: a long upper wick relative to a
// small body (UpperWickToBody >= ~2) is a shooting-star / pin bar = sellers
// rejecting higher prices, the classic exhaustion-fade trigger at a top
// (mirror: LowerWickToBody for a bottom). Ratios are 0 for a zero-body doji
// (no divide-by-zero).
type CandleShape struct {
	BodyPips      float64
	UpperWickPips float64
	LowerWickPips float64
	RangePips     float64
	Bullish       bool // Close > Open

	UpperWickToBody float64 // UpperWickPips / BodyPips, 0 when body == 0
	LowerWickToBody float64 // LowerWickPips / BodyPips, 0 when body == 0
}

// ShapeOf decomposes candle c into a CandleShape. ok=false when pipSize <= 0.
func ShapeOf(c market.Candle, pipSize float64) (CandleShape, bool) {
	if pipSize <= 0 {
		return CandleShape{}, false
	}
	bodyTop := math.Max(c.Open, c.Close)
	bodyBottom := math.Min(c.Open, c.Close)
	s := CandleShape{
		BodyPips:      math.Abs(c.Close-c.Open) / pipSize,
		UpperWickPips: (c.High - bodyTop) / pipSize,
		LowerWickPips: (bodyBottom - c.Low) / pipSize,
		RangePips:     (c.High - c.Low) / pipSize,
		Bullish:       c.Close > c.Open,
	}
	if s.BodyPips > 0 {
		s.UpperWickToBody = s.UpperWickPips / s.BodyPips
		s.LowerWickToBody = s.LowerWickPips / s.BodyPips
	}
	return s, true
}

// ConsecutiveBars counts how many of the most-recent candles share the last
// candle's direction (bullish = Close > Open, bearish = Close < Open). A doji
// at the end (Close == Open) breaks the streak and returns (0, false). dirUp is
// the direction of the streak (the last candle's). A long run of same-coloured
// bars with shrinking bodies is the "最後のひと伸び" exhaustion cue.
func ConsecutiveBars(candles []market.Candle) (count int, dirUp bool) {
	if len(candles) == 0 {
		return 0, false
	}
	last := candles[len(candles)-1]
	if last.Close == last.Open {
		return 0, false
	}
	up := last.Close > last.Open
	for i := len(candles) - 1; i >= 0; i-- {
		c := candles[i]
		barUp := c.Close > c.Open
		barDown := c.Close < c.Open
		if (up && barUp) || (!up && barDown) {
			count++
			continue
		}
		break
	}
	return count, up
}

// MakesNewHigh reports whether the last candle's High exceeds the highest High
// of the `lookback` candles immediately before it. True = price is still making
// new highs (momentum intact, do NOT fade yet); false = the push has stalled
// (the exhaustion precondition). Returns false when there are fewer than
// lookback+1 candles or lookback < 1.
func MakesNewHigh(candles []market.Candle, lookback int) bool {
	if lookback < 1 || len(candles) < lookback+1 {
		return false
	}
	last := candles[len(candles)-1]
	priorMax := candles[len(candles)-1-lookback].High
	for i := len(candles) - lookback; i < len(candles)-1; i++ {
		if candles[i].High > priorMax {
			priorMax = candles[i].High
		}
	}
	return last.High > priorMax
}

// MakesNewLow is the mirror of MakesNewHigh for a down-spike: true = still
// making new lows (do not fade-long yet), false = the slide has stalled.
func MakesNewLow(candles []market.Candle, lookback int) bool {
	if lookback < 1 || len(candles) < lookback+1 {
		return false
	}
	last := candles[len(candles)-1]
	priorMin := candles[len(candles)-1-lookback].Low
	for i := len(candles) - lookback; i < len(candles)-1; i++ {
		if candles[i].Low < priorMin {
			priorMin = candles[i].Low
		}
	}
	return last.Low < priorMin
}

// LastSwingHigh returns the price of the MOST RECENT confirmed fractal swing
// high (see SwingHighs for the pivot / prominence contract) — the "直近高値"
// level a fade-short leans against. ok=false when no swing is found. n and
// minProminencePips are passed straight through to the detector.
func LastSwingHigh(candles []market.Candle, n int, pipSize, minProminencePips float64) (float64, bool) {
	sw := SwingHighs(candles, n, pipSize, minProminencePips)
	if len(sw) == 0 {
		return 0, false
	}
	return sw[len(sw)-1].Price, true
}

// LastSwingLow is the mirror of LastSwingHigh — the "直近安値" level a fade-long
// leans against.
func LastSwingLow(candles []market.Candle, n int, pipSize, minProminencePips float64) (float64, bool) {
	sw := SwingLows(candles, n, pipSize, minProminencePips)
	if len(sw) == 0 {
		return 0, false
	}
	return sw[len(sw)-1].Price, true
}

// RoundNumberDistancePips returns the distance in pips from price to the nearest
// round level on the given grid (step), plus that nearest level. Use step=0.50
// for USDJPY to catch both figures (.00) and half-figures (.50). Round numbers
// are where stop / limit orders cluster — the levels a fade leans against. distPips
// is always >= 0. ok=false when step <= 0 or pipSize <= 0.
func RoundNumberDistancePips(price, step, pipSize float64) (distPips, nearest float64, ok bool) {
	if step <= 0 || pipSize <= 0 {
		return 0, 0, false
	}
	nearest = math.Round(price/step) * step
	return math.Abs(price-nearest) / pipSize, nearest, true
}

// EfficiencyRatio is Kaufman's Efficiency Ratio over the last n close-to-close
// steps: |close[last]-close[last-n]| / sum(|close[i]-close[i-1]|). 1.0 = a
// perfectly straight, directional run (a clean orderly move worth fading once it
// stalls); near 0 = a choppy round-trip (往復ビンタ — the chop a fade strategy
// should avoid). ok=false when n < 1, fewer than n+1 closes, or the path length is 0
// (a flat series — no move to measure).
func EfficiencyRatio(closes []float64, n int) (float64, bool) {
	if n < 1 || len(closes) < n+1 {
		return 0, false
	}
	last := len(closes) - 1
	net := math.Abs(closes[last] - closes[last-n])
	var path float64
	for i := last - n + 1; i <= last; i++ {
		path += math.Abs(closes[i] - closes[i-1])
	}
	if path == 0 {
		return 0, false
	}
	return net / path, true
}
