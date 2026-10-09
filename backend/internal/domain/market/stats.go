package market

import (
	"math"
	"time"
)

// BuildWindowSummary aggregates a slice of candles into a WindowSummary.
// pipSize is e.g. 0.01 for USDJPY.
//
// realized_volatility is stdev of log returns over close prices, multiplied
// by sqrt(n) so it reflects total observed move.
//
// trend_direction is the sign of the slope of a simple linear regression
// over close prices (or "flat" when |slope| is very small).
func BuildWindowSummary(candles []Candle, pipSize float64) WindowSummary {
	n := len(candles)
	if n == 0 {
		return WindowSummary{TrendDirection: "flat"}
	}
	hi := candles[0].High
	lo := candles[0].Low
	closes := make([]float64, n)
	for i, c := range candles {
		if c.High > hi {
			hi = c.High
		}
		if c.Low < lo {
			lo = c.Low
		}
		closes[i] = c.Close
	}
	rangePips := 0.0
	changePips := 0.0
	if pipSize > 0 {
		rangePips = (hi - lo) / pipSize
		changePips = (candles[n-1].Close - candles[0].Open) / pipSize
	}
	return WindowSummary{
		High:               hi,
		Low:                lo,
		RangePips:          rangePips,
		ChangePips:         changePips,
		RealizedVolatility: realizedVolatility(closes),
		ATRPips:            averageTrueRangePips(candles, pipSize),
		TrendDirection:     trendDirection(closes),
		// Support/Resistance: simple heuristic = window low/high. これは
		// "swing point" ではないが、デイトレ判定には「直近 N 時間の上限/下限」
		// が必要十分。より高度な検出 (peak/trough、フラクタル) は将来拡張。
		Support:    lo,
		Resistance: hi,
		NumCandles: n,
	}
}

// ATRPips is the exported form of the window's mean True Range (in pips) — the
// same value BuildWindowSummary stores in WindowSummary.ATRPips. Exposed so the
// ta package can size stops / measure MA-deviation in ATR units without
// re-implementing the True Range math. Returns 0 for empty input or
// non-positive pipSize.
func ATRPips(candles []Candle, pipSize float64) float64 {
	return averageTrueRangePips(candles, pipSize)
}

// averageTrueRangePips = mean True Range over the window's candles, in pips.
// TR_i = max(high-low, |high-prevClose|, |low-prevClose|). The first candle has
// no previous close, so its TR is high-low. Returns 0 for empty input or
// non-positive pipSize.
func averageTrueRangePips(candles []Candle, pipSize float64) float64 {
	if len(candles) == 0 || pipSize <= 0 {
		return 0
	}
	var sum float64
	for i, c := range candles {
		tr := c.High - c.Low
		if i > 0 {
			pc := candles[i-1].Close
			if d := math.Abs(c.High - pc); d > tr {
				tr = d
			}
			if d := math.Abs(c.Low - pc); d > tr {
				tr = d
			}
		}
		sum += tr
	}
	return (sum / float64(len(candles))) / pipSize
}

// RangePosition returns where price sits within [low, high]: 0 = at low,
// 1 = at high. Values <0 / >1 mean price is below/above the window's range
// (a breakout). Returns 0.5 (neutral) when high<=low (degenerate window).
func RangePosition(price, low, high float64) float64 {
	if high <= low {
		return 0.5
	}
	return (price - low) / (high - low)
}

// ApplyWindowSpreads sets AvgSpreadPips/MaxSpreadPips on `w` from the given
// per-tick spread samples (taken within the same window). Caller passes the
// spread slice already filtered to the window's [start, end).
// No-op when samples is empty (the omitempty fields stay zero).
func ApplyWindowSpreads(w WindowSummary, spreads []float64) WindowSummary {
	if len(spreads) == 0 {
		return w
	}
	var sum, mx float64
	for _, s := range spreads {
		sum += s
		if s > mx {
			mx = s
		}
	}
	w.AvgSpreadPips = sum / float64(len(spreads))
	w.MaxSpreadPips = mx
	return w
}

// realizedVolatility = stdev(log returns) * sqrt(n).
func realizedVolatility(closes []float64) float64 {
	if len(closes) < 2 {
		return 0
	}
	rets := make([]float64, len(closes)-1)
	var mean float64
	for i := 1; i < len(closes); i++ {
		if closes[i-1] <= 0 || closes[i] <= 0 {
			continue
		}
		r := math.Log(closes[i] / closes[i-1])
		rets[i-1] = r
		mean += r
	}
	mean /= float64(len(rets))
	var ssq float64
	for _, r := range rets {
		d := r - mean
		ssq += d * d
	}
	std := math.Sqrt(ssq / float64(len(rets)))
	return std * math.Sqrt(float64(len(rets)))
}

// trendDirection is the sign of the OLS slope of closes vs index. "flat"
// when |slope| is below a small threshold relative to mean price.
func trendDirection(closes []float64) string {
	n := len(closes)
	if n < 2 {
		return "flat"
	}
	var sumX, sumY, sumXY, sumXX float64
	for i, y := range closes {
		x := float64(i)
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}
	fn := float64(n)
	denom := fn*sumXX - sumX*sumX
	if denom == 0 {
		return "flat"
	}
	slope := (fn*sumXY - sumX*sumY) / denom
	mean := sumY / fn
	if mean == 0 {
		return "flat"
	}
	// 0.0001% of price per bar threshold — extremely small but discriminates
	// genuine flat lines from real moves on USDJPY.
	threshold := math.Abs(mean) * 1e-6
	if slope > threshold {
		return "up"
	}
	if slope < -threshold {
		return "down"
	}
	return "flat"
}

// SinceWindow returns candles within [now-d, now].
func SinceWindow(candles []Candle, now time.Time, d time.Duration) []Candle {
	cutoff := now.Add(-d)
	for i, c := range candles {
		if !c.OpenTime.Before(cutoff) {
			return candles[i:]
		}
	}
	return nil
}
