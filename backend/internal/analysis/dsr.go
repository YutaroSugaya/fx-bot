package analysis

import "math"

// Deflated Sharpe Ratio (DSR) and its building blocks — Bailey & López de Prado
// (2014). The project already gates promotion with a bootstrap CI + a sealed
// holdout (judge.go), but neither penalises the NUMBER OF VARIANTS TRIED: try
// enough thresholds/strategies and the best one looks great by luck. DSR closes
// that hole — it deflates an observed Sharpe by the expected MAXIMUM Sharpe that
// `nTrials` random strategies would produce, and returns the probability the
// true Sharpe is still positive. Pure, stdlib-only, deterministic.

// eulerMascheroni is the γ constant used in the expected-maximum-Sharpe term.
const eulerMascheroni = 0.5772156649015329

// SharpeRatio is the (non-annualised) Sharpe of a per-trade return series:
// mean / sample-stdev (ddof=1). ok=false when there are fewer than 2 returns or
// the series has zero variance (Sharpe undefined).
func SharpeRatio(returns []float64) (float64, bool) {
	n := len(returns)
	if n < 2 {
		return 0, false
	}
	mean := 0.0
	for _, r := range returns {
		mean += r
	}
	mean /= float64(n)
	var ss float64
	for _, r := range returns {
		d := r - mean
		ss += d * d
	}
	std := math.Sqrt(ss / float64(n-1))
	if std == 0 {
		return 0, false
	}
	return mean / std, true
}

// skewKurt returns the population skewness and (non-excess) kurtosis of the
// series (normal kurtosis = 3). ok=false when undefined (n<2 or zero variance).
func skewKurt(returns []float64) (skew, kurt float64, ok bool) {
	n := len(returns)
	if n < 2 {
		return 0, 0, false
	}
	mean := 0.0
	for _, r := range returns {
		mean += r
	}
	mean /= float64(n)
	var m2, m3, m4 float64
	for _, r := range returns {
		d := r - mean
		d2 := d * d
		m2 += d2
		m3 += d2 * d
		m4 += d2 * d2
	}
	m2 /= float64(n)
	m3 /= float64(n)
	m4 /= float64(n)
	if m2 == 0 {
		return 0, 0, false
	}
	return m3 / math.Pow(m2, 1.5), m4 / (m2 * m2), true
}

// normalCDF is the standard normal CDF Φ(x) via the error function.
func normalCDF(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }

// inverseNormalCDF is the standard normal quantile Φ⁻¹(p) for p∈(0,1) using
// Acklam's rational approximation (abs error < ~1.2e-9). p<=0 → -Inf, p>=1 → +Inf.
func inverseNormalCDF(p float64) float64 {
	if p <= 0 {
		return math.Inf(-1)
	}
	if p >= 1 {
		return math.Inf(1)
	}
	a := [6]float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02, 1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := [5]float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02, 6.680131188771972e+01, -1.328068155288572e+01}
	c := [6]float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00, -2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := [4]float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00, 3.754408661907416e+00}
	const plow = 0.02425
	const phigh = 1 - plow
	switch {
	case p < plow:
		q := math.Sqrt(-2 * math.Log(p))
		return (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p <= phigh:
		q := p - 0.5
		r := q * q
		return (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q /
			(((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
	default:
		q := math.Sqrt(-2 * math.Log(1-p))
		return -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
}

// ProbabilisticSharpeRatio is PSR(benchmarkSR) = P(true SR > benchmarkSR),
// correcting the Gaussian SR estimate for the sample's skew and (fat) kurtosis
// (López de Prado 2012). At benchmarkSR == the observed SR it is exactly 0.5.
// ok=false when the SR is undefined or the variance term is non-positive.
func ProbabilisticSharpeRatio(returns []float64, benchmarkSR float64) (float64, bool) {
	sr, ok := SharpeRatio(returns)
	if !ok {
		return 0, false
	}
	skew, kurt, ok := skewKurt(returns)
	if !ok {
		return 0, false
	}
	radicand := 1 - skew*sr + (kurt-1)/4*sr*sr
	if radicand <= 0 {
		return 0, false
	}
	n := float64(len(returns))
	z := (sr - benchmarkSR) * math.Sqrt(n-1) / math.Sqrt(radicand)
	return normalCDF(z), true
}

// DeflatedSharpeRatio is PSR evaluated at the expected MAXIMUM Sharpe of
// `nTrials` independent strategies whose Sharpe estimates have variance
// trialSRVariance — i.e. the probability the strategy's true Sharpe beats what
// the luckiest of nTrials random strategies would show. nTrials=1 applies no
// multiple-testing deflation (DSR = PSR(0)). Honestly count EVERY variant tried
// (strategies × params × thresholds) as a trial. ok=false when nTrials<1 or the
// Sharpe is undefined.
func DeflatedSharpeRatio(returns []float64, nTrials int, trialSRVariance float64) (float64, bool) {
	if nTrials < 1 {
		return 0, false
	}
	sr0 := 0.0
	if nTrials > 1 && trialSRVariance > 0 {
		nF := float64(nTrials)
		maxZ := (1-eulerMascheroni)*inverseNormalCDF(1-1/nF) +
			eulerMascheroni*inverseNormalCDF(1-1/(nF*math.E))
		sr0 = math.Sqrt(trialSRVariance) * maxZ
	}
	return ProbabilisticSharpeRatio(returns, sr0)
}
