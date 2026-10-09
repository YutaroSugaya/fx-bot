package ta

import (
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
)

// RSI / regular divergence — momentum-confirmation primitives for the
// exhaustion-fade family. A fade is safer when the
// oscillator REFUSES to confirm the new price extreme (price prints a higher
// high but momentum makes a lower high = bearish regular divergence = the push
// is running out of fuel). Pure functions over a close / candle series; they
// describe the market, they do not decide trades. Same ok-contract as the rest
// of the package: ok=false = undefined (insufficient data / no movement).

// RSISeries computes Wilder's Relative Strength Index over `closes` for the
// given period and returns the value AT EACH index (same length as closes).
// Indices [0, period-1] are undefined (set to 0) — callers must only read at
// indices >= period (the returned validFrom). Seeding follows Wilder: the first
// RSI (at index = period) uses the simple average of the first `period`
// close-to-close changes, then the averages are smoothed forward
// avg = (avg*(period-1) + current) / period. A window with no down moves yields
// RSI 100; no up moves yields 0; a perfectly flat window yields the neutral 50.
// ok=false when period < 1 or there are fewer than period+1 closes.
func RSISeries(closes []float64, period int) (rsi []float64, validFrom int, ok bool) {
	if period < 1 || len(closes) < period+1 {
		return nil, 0, false
	}
	rsi = make([]float64, len(closes))

	// Seed: simple average of the first `period` changes (indices 1..period).
	var sumGain, sumLoss float64
	for i := 1; i <= period; i++ {
		ch := closes[i] - closes[i-1]
		if ch > 0 {
			sumGain += ch
		} else {
			sumLoss += -ch
		}
	}
	avgGain := sumGain / float64(period)
	avgLoss := sumLoss / float64(period)
	rsi[period] = rsiFrom(avgGain, avgLoss)

	// Wilder smoothing forward.
	for i := period + 1; i < len(closes); i++ {
		ch := closes[i] - closes[i-1]
		gain, loss := 0.0, 0.0
		if ch > 0 {
			gain = ch
		} else {
			loss = -ch
		}
		avgGain = (avgGain*float64(period-1) + gain) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + loss) / float64(period)
		rsi[i] = rsiFrom(avgGain, avgLoss)
	}
	return rsi, period, true
}

// rsiFrom maps smoothed average gain/loss to the 0..100 RSI, handling the
// degenerate divisions: no losses = 100 (only up moves), no gains = 0, and a
// flat window (neither) = 50 (neutral, no information).
func rsiFrom(avgGain, avgLoss float64) float64 {
	switch {
	case avgLoss == 0 && avgGain == 0:
		return 50
	case avgLoss == 0:
		return 100
	case avgGain == 0:
		return 0
	}
	rs := avgGain / avgLoss
	return 100 - 100/(1+rs)
}

// RSI returns the latest Wilder RSI over `closes`. ok=false when undefined
// (period < 1, fewer than period+1 closes, or a perfectly flat series).
func RSI(closes []float64, period int) (float64, bool) {
	series, _, ok := RSISeries(closes, period)
	if !ok {
		return 0, false
	}
	// A fully flat series carries no momentum information.
	flat := true
	for i := 1; i < len(closes); i++ {
		if closes[i] != closes[i-1] {
			flat = false
			break
		}
	}
	if flat {
		return 0, false
	}
	return series[len(series)-1], true
}

// RegularDivergence reports whether the last two confirmed fractal swings of the
// fade side show a REGULAR divergence between price and RSI:
//   - SELL (bearish): the newer swing HIGH prints a higher high than the prior
//     swing high, but its RSI is LOWER (momentum failing to confirm new highs).
//   - BUY  (bullish): the newer swing LOW prints a lower low than the prior
//     swing low, but its RSI is HIGHER (selling pressure failing to confirm).
//
// Both swing indices must be >= the RSI validFrom (RSI defined there). Returns
// false (no confirmation) when fewer than two swings exist or RSI is undefined —
// callers requiring divergence treat that as "do not act". swingN /
// minProminencePips are passed straight to the swing detector.
func RegularDivergence(candles []market.Candle, side order.Side, rsiPeriod, swingN int, pipSize, minProminencePips float64) bool {
	closes := make([]float64, len(candles))
	for i, c := range candles {
		closes[i] = c.Close
	}
	rsi, validFrom, ok := RSISeries(closes, rsiPeriod)
	if !ok {
		return false
	}
	switch side {
	case order.SideSell:
		sw := SwingHighs(candles, swingN, pipSize, minProminencePips)
		if len(sw) < 2 {
			return false
		}
		s1, s2 := sw[len(sw)-2], sw[len(sw)-1]
		if s1.Index < validFrom || s2.Index < validFrom {
			return false
		}
		return s2.Price > s1.Price && rsi[s2.Index] < rsi[s1.Index]
	case order.SideBuy:
		sw := SwingLows(candles, swingN, pipSize, minProminencePips)
		if len(sw) < 2 {
			return false
		}
		s1, s2 := sw[len(sw)-2], sw[len(sw)-1]
		if s1.Index < validFrom || s2.Index < validFrom {
			return false
		}
		return s2.Price < s1.Price && rsi[s2.Index] > rsi[s1.Index]
	}
	return false
}
