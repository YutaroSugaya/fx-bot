package ta

import "fx-bot/backend/internal/domain/market"

// SwingKind distinguishes a pivot high from a pivot low.
type SwingKind int

const (
	SwingHigh SwingKind = iota
	SwingLow
)

// Swing is a detected fractal pivot. Price is the High of a SwingHigh / the
// Low of a SwingLow. Index is the position in the candle slice it was found in.
type Swing struct {
	Index int
	Price float64
	Kind  SwingKind
}

// SwingHighs detects fractal pivot highs. A bar i is a swing high when its
// High is STRICTLY greater than the High of every bar within n bars on each
// side (so i must sit at least n bars from both ends).
//
// minProminencePips is a noise gate: the pivot must protrude at least that
// many pips above its surroundings, measured as the smaller of the two drops
// to the lowest Low in the n bars on the left and on the right. This rejects a
// single wick poking out of an otherwise flat cluster (the method's "ヒゲ1本の
// ノイズを反発と誤カウントしない"). minProminencePips <= 0 disables the gate.
//
// When the gate IS requested (minProminencePips > 0) but pipSize <= 0, the
// request cannot be honoured (no pip scale) and we return nil rather than
// silently emit ungated swings — the method warns that a defeated noise gate
// turns the strategy into a different one ("ザル化").
//
// Returns pivots in ascending index order, or nil when n < 1 or there are too
// few candles to form one.
func SwingHighs(candles []market.Candle, n int, pipSize, minProminencePips float64) []Swing {
	return detectSwings(candles, n, pipSize, minProminencePips, SwingHigh)
}

// SwingLows is the mirror of SwingHighs for pivot lows (strictly-lowest Low,
// prominence measured against the highest High of the neighbours).
func SwingLows(candles []market.Candle, n int, pipSize, minProminencePips float64) []Swing {
	return detectSwings(candles, n, pipSize, minProminencePips, SwingLow)
}

func detectSwings(candles []market.Candle, n int, pipSize, minProminencePips float64, kind SwingKind) []Swing {
	if n < 1 || len(candles) < 2*n+1 {
		return nil
	}
	// A requested noise gate with no usable pip scale cannot be honoured.
	if minProminencePips > 0 && pipSize <= 0 {
		return nil
	}
	var out []Swing
	for i := n; i < len(candles)-n; i++ {
		// Skip any window containing a corrupt (non-finite) bar: with NaN/Inf
		// present, the strict-extremum comparisons silently misbehave and would
		// emit a poisoned swing.
		if !windowFinite(candles, i, n) {
			continue
		}
		if !isStrictExtremum(candles, i, n, kind) {
			continue
		}
		if minProminencePips > 0 {
			if prominence(candles, i, n, kind) < minProminencePips*pipSize {
				continue
			}
		}
		price := candles[i].High
		if kind == SwingLow {
			price = candles[i].Low
		}
		out = append(out, Swing{Index: i, Price: price, Kind: kind})
	}
	return out
}

// windowFinite reports whether every bar's High and Low in [i-n, i+n] is a
// finite number. A non-finite value anywhere in the comparison/prominence
// window makes detection unreliable, so the caller skips such windows.
func windowFinite(candles []market.Candle, i, n int) bool {
	for j := i - n; j <= i+n; j++ {
		if !isFinite(candles[j].High) || !isFinite(candles[j].Low) {
			return false
		}
	}
	return true
}

// isStrictExtremum reports whether bar i is a strict local high/low over the
// window [i-n, i+n].
func isStrictExtremum(candles []market.Candle, i, n int, kind SwingKind) bool {
	for j := i - n; j <= i+n; j++ {
		if j == i {
			continue
		}
		switch kind {
		case SwingHigh:
			if candles[j].High >= candles[i].High {
				return false
			}
		case SwingLow:
			if candles[j].Low <= candles[i].Low {
				return false
			}
		}
	}
	return true
}

// prominence returns the price excursion around pivot i: for a SwingHigh, the
// smaller of (High[i] - lowest Low on the left) and (High[i] - lowest Low on
// the right). Symmetric for a SwingLow against neighbouring Highs.
func prominence(candles []market.Candle, i, n int, kind SwingKind) float64 {
	switch kind {
	case SwingHigh:
		left := lowestLow(candles, i-n, i-1)
		right := lowestLow(candles, i+1, i+n)
		return minF(candles[i].High-left, candles[i].High-right)
	case SwingLow:
		left := highestHigh(candles, i-n, i-1)
		right := highestHigh(candles, i+1, i+n)
		return minF(left-candles[i].Low, right-candles[i].Low)
	}
	return 0
}

func lowestLow(candles []market.Candle, from, to int) float64 {
	lo := candles[from].Low
	for j := from + 1; j <= to; j++ {
		if candles[j].Low < lo {
			lo = candles[j].Low
		}
	}
	return lo
}

func highestHigh(candles []market.Candle, from, to int) float64 {
	hi := candles[from].High
	for j := from + 1; j <= to; j++ {
		if candles[j].High > hi {
			hi = candles[j].High
		}
	}
	return hi
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
