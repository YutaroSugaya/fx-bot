package backtest

import "math"

// Metrics summarises a backtest run's PnL distribution. All fields are 0 when
// trades is empty (caller can treat 0 ProfitFactor as "no data").
//
// Definitions:
//   - ProfitFactor = sum(wins) / sum(|losses|). +Inf when there are wins but no
//     losses; 0 when there are no wins.
//   - Expectancy   = average PnL per trade.
//   - MaxDrawdown  = peak-to-trough drop in cumulative PnL (positive number,
//     in JPY).
//   - WinRate      = winners / total. A trade with PnL == 0 is counted as a
//     loss for the WinRate (conservative) but excluded from both sums.
//   - SampleSize   = len(trades).
type Metrics struct {
	ProfitFactor float64
	Expectancy   float64
	MaxDrawdown  float64
	WinRate      float64
	SampleSize   int
}

// ComputeMetrics derives Metrics from a chronologically ordered slice of
// trades. The caller does not need to sort beforehand for PF / Expectancy /
// WinRate, but MaxDrawdown is order-sensitive — pass trades in OpenedAt order.
func ComputeMetrics(trades []Trade) Metrics {
	if len(trades) == 0 {
		return Metrics{}
	}
	var sumWins, sumLossesAbs, sumAll float64
	winners := 0
	for _, t := range trades {
		sumAll += t.ProfitLossJPY
		switch {
		case t.ProfitLossJPY > 0:
			sumWins += t.ProfitLossJPY
			winners++
		case t.ProfitLossJPY < 0:
			sumLossesAbs += -t.ProfitLossJPY
		}
	}

	pf := 0.0
	switch {
	case sumLossesAbs == 0 && sumWins > 0:
		pf = positiveInfinity()
	case sumLossesAbs > 0:
		pf = sumWins / sumLossesAbs
	}

	m := Metrics{
		ProfitFactor: pf,
		Expectancy:   sumAll / float64(len(trades)),
		WinRate:      float64(winners) / float64(len(trades)),
		SampleSize:   len(trades),
		MaxDrawdown:  maxDrawdown(trades),
	}
	return m
}

// maxDrawdown walks the equity curve in trade order and returns the largest
// peak-to-trough drop. Returns 0 when the equity never declines.
func maxDrawdown(trades []Trade) float64 {
	var equity, peak, maxDD float64
	for _, t := range trades {
		equity += t.ProfitLossJPY
		if equity > peak {
			peak = equity
		}
		if dd := peak - equity; dd > maxDD {
			maxDD = dd
		}
	}
	return maxDD
}

// positiveInfinity returns +Inf for the "all wins, no losses" PF case.
func positiveInfinity() float64 {
	return math.Inf(1)
}
