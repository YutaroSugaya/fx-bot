package command

import (
	"fmt"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
)

// ValidateSignalBoundaries enforces hard_limits at the ORDER boundary on the
// REAL values the strategy emits (Signal), not the config placeholders.
// The config validator only sees placeholder TP/SL; the
// strategy can emit anything via the Signal path, so the actual safety must be
// checked here, right before the broker call.
//
// quoteJPYRate is the quote→JPY multiplier (1.0 for JPY pairs; USD/JPY for
// USD-quote pairs; <=0 = unavailable → the per-trade JPY loss cap is skipped,
// the other bounds still apply so a transient rate error never naked-blocks a
// trade or naked-passes a fat finger).
//
// Returns nil when limits is nil (feature off / tests). The non-FX bounds
// (qty range, MaxHold cap) are always enforced when limits is non-nil; the
// sanity caps + loss cap require limits.OrderBoundary.
func ValidateSignalBoundaries(strategyName, symbol string, slPips, tpPips float64, maxHoldMin, qty int, quoteJPYRate float64, limits *config.HardLimits) error {
	if limits == nil {
		return nil
	}
	// Quantity within the approved range — defense in depth at the order
	// boundary (config promotion + manual-trade already check it elsewhere).
	if !limits.Quantity.Contains(qty) {
		return fmt.Errorf("order boundary: quantity %d outside [%d,%d]", qty, limits.Quantity.Min, limits.Quantity.Max)
	}
	// MaxHold must not exceed the cap — catches a strategy (or a positions-row
	// manual UPDATE) emitting e.g. 780 > a 600 cap.
	if maxHoldMin > limits.MaxHoldMinutes.Max {
		return fmt.Errorf("order boundary: max_hold %d > cap %d", maxHoldMin, limits.MaxHoldMinutes.Max)
	}

	// Per-strategy override (e.g. daily signature_breakout gets a wider SL/TP sanity cap) falling
	// back to the global OrderBoundary. The per-trade JPY loss cap stays the real capital control.
	ob := limits.OrderBoundaryFor(strategyName)
	if ob == nil {
		return nil
	}
	if ob.MaxStopLossPips > 0 && slPips > ob.MaxStopLossPips {
		return fmt.Errorf("order boundary: stop_loss %.1f > sanity cap %.1f (decimal typo / abnormal SL)", slPips, ob.MaxStopLossPips)
	}
	if ob.MaxTakeProfitPips > 0 && tpPips > ob.MaxTakeProfitPips {
		return fmt.Errorf("order boundary: take_profit %.1f > sanity cap %.1f", tpPips, ob.MaxTakeProfitPips)
	}
	// Per-trade worst-case loss in JPY = SL_pips × pipSize × qty × quote→JPY.
	// This is the real capital control as qty scales.
	if ob.MaxLossPerTradeJPY > 0 && quoteJPYRate > 0 && slPips > 0 {
		lossJPY := slPips * market.PipSize(symbol) * float64(qty) * quoteJPYRate
		if lossJPY > float64(ob.MaxLossPerTradeJPY) {
			return fmt.Errorf("order boundary: est per-trade loss %.0f JPY (SL %.1f pips × %d units × %.2f) > cap %d JPY",
				lossJPY, slPips, qty, quoteJPYRate, ob.MaxLossPerTradeJPY)
		}
	}
	return nil
}
