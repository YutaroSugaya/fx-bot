package ta

// Moving averages — the two lines the 200MA pullback strategy (ma_pullback) is
// built on: a Simple and an
// Exponential moving average. Both are pure functions over a close series and
// return the CURRENT (latest) average value, so the strategy reads "price vs
// 200MA" directly. ok=false means the average is undefined (not enough data /
// bad period) — callers treat that as "no MA yet, do not trade", mirroring the
// FitLine ok-contract used elsewhere in this package.

// SMA returns the simple moving average of the LAST `period` values — the mean
// of values[len-period:]. ok=false when period < 1 or there are fewer than
// `period` values (no full window yet).
func SMA(values []float64, period int) (float64, bool) {
	if period < 1 || len(values) < period {
		return 0, false
	}
	var sum float64
	for _, v := range values[len(values)-period:] {
		sum += v
	}
	return sum / float64(period), true
}

// EMA returns the exponential moving average at the latest value, seeded by the
// SMA of the first `period` values and then iterated forward with the standard
// smoothing factor k = 2/(period+1). Recency-weighted, so it reacts to a fresh
// move sooner than the SMA. ok=false when period < 1 or len(values) < period.
func EMA(values []float64, period int) (float64, bool) {
	if period < 1 || len(values) < period {
		return 0, false
	}
	// Seed with the SMA of the first window so the recursion has a defined start.
	seed, _ := SMA(values[:period], period)
	ema := seed
	k := 2.0 / (float64(period) + 1.0)
	for _, v := range values[period:] {
		ema = v*k + ema*(1-k)
	}
	return ema, true
}

// SMASeries is the per-index ("series") form of SMA: out[i] is the simple
// moving average of values[max(0,i-period+1)..i], and 0 for indices before a
// full window (i < period-1). By construction out[i] == SMA(values[:i+1], period)
// at every defined index, so a chart overlay drawn from this series matches the
// scalar SMA the strategy trades on. Returns a 0-filled slice for period < 1.
func SMASeries(values []float64, period int) []float64 {
	out := make([]float64, len(values))
	if period < 1 {
		return out
	}
	var sum float64
	for i, v := range values {
		sum += v
		if i >= period {
			sum -= values[i-period]
		}
		if i >= period-1 {
			out[i] = sum / float64(period)
		}
	}
	return out
}

// EMASeries is the per-index ("series") form of EMA: seeded with the SMA of the
// first window (out[period-1]) then iterated forward with k = 2/(period+1), so
// out[i] == EMA(values[:i+1], period) at every defined index. 0 for indices
// before the seed point (i < period-1). Returns a 0-filled slice when period < 1
// or len(values) < period.
func EMASeries(values []float64, period int) []float64 {
	out := make([]float64, len(values))
	if period < 1 || len(values) < period {
		return out
	}
	seed, _ := SMA(values[:period], period)
	ema := seed
	out[period-1] = seed
	k := 2.0 / (float64(period) + 1.0)
	for i := period; i < len(values); i++ {
		ema = values[i]*k + ema*(1-k)
		out[i] = ema
	}
	return out
}
