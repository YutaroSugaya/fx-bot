package app

import (
	"fmt"

	"fx-bot/backend/internal/config"
)

// QtyGuardThreshold は連敗ガード/cooldown の起動時 assert が発火する実効 qty 閾値。
// これ以上のサイズで live を回すなら連敗系ガードと after-loss cooldown を ON に
// していないと起動拒否する。screening size (1000) では guards-off は意図的な
// 運用 knob として許容し、発火しない。
const QtyGuardThreshold = 10000

// AssertLiveQtyGuards refuses startup when the bot would trade at SIZE
// (effectiveQty >= QtyGuardThreshold) in Live mode with the loss-streak guards
// disabled OR no after-loss cooldown. It keys off the
// EFFECTIVE qty (the largest size any active config actually trades), NOT the
// hard_limits ceiling — keying off the ceiling would refuse every restart of a
// small-size config (e.g. qty=1000) whenever hard_limits allows a large Max
// (e.g. 100000).
func AssertLiveQtyGuards(mode config.Mode, effectiveQty int, guardsDisabled bool, afterLossCooldownSeconds int) error {
	if mode != config.ModeLiveConfig {
		return nil
	}
	if effectiveQty < QtyGuardThreshold {
		return nil
	}
	if guardsDisabled || afterLossCooldownSeconds == 0 {
		return fmt.Errorf("live qty guard: live effective qty %d >= %d requires loss-streak guards ON and an "+
			"after-loss cooldown (disable_consecutive_loss_guards=%v, cooldown.after_loss_seconds=%d). "+
			"Re-enable guards + cooldown, or drop qty to paper-screen size.",
			effectiveQty, QtyGuardThreshold, guardsDisabled, afterLossCooldownSeconds)
	}
	return nil
}

// MaxActiveConfigQuantity returns the largest risk.quantity across all active
// configs in the holder (the effective trade size). 0 when no configs loaded.
func MaxActiveConfigQuantity(h *ActiveConfigHolder) int {
	if h == nil {
		return 0
	}
	max := 0
	for _, c := range h.All() {
		if c != nil && c.Risk.Quantity > max {
			max = c.Risk.Quantity
		}
	}
	return max
}
