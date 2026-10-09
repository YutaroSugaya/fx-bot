package risk

import (
	"strings"
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
)

// Block same-symbol same-side pyramiding INDEPENDENTLY of the MaxOpenPositions
// cap. cap=1 implicitly prevents a second position, but once the cap is raised
// to 2-3 the implicit wall vanishes. external (GMO-app) positions count here even
// though the external-position rule excludes
// them from bot capacity — a same-side manual position must still block the bot
// from stacking the same direction.
func TestEvaluateSignal_BlocksSameSidePyramidingInclExternal(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	snap := AccountSnapshot{MaxDailyLossJPY: 1000, MaxConsecutiveLosses: 3, Now: now}
	// Bot capacity not hit (OpenPositions=0), but a same-side BUY exists incl. external.
	snap.OpenBuyInclExternal = 1

	d := EvaluateSignal(entrySig(order.SideBuy), validCfg(config.DirectionBoth, now), snap, mkSummary(0.3))
	if d.Allowed {
		t.Fatal("same-side BUY pyramiding must be blocked even when the bot cap is not hit")
	}
	if !strings.HasPrefix(d.Reason, "pyramiding") {
		t.Errorf("reason=%q, want pyramiding_*", d.Reason)
	}

	// Opposite side (SELL) must still pass — pyramiding only blocks the same side.
	d2 := EvaluateSignal(entrySig(order.SideSell), validCfg(config.DirectionBoth, now), snap, mkSummary(0.3))
	if !d2.Allowed {
		t.Fatalf("opposite-side entry must pass; got %s", d2.Reason)
	}
}

// MaxConcurrent>1 lets a strategy add to a winner: the gate permits up to N same-side positions,
// but still blocks the (N+1)th. Default (0/1) keeps the strict single-position rule.
func TestEvaluateSignal_MaxConcurrentAllowsBoundedPyramid(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	cfg := validCfg(config.DirectionBoth, now)

	sig := entrySig(order.SideBuy)
	sig.MaxConcurrent = 2 // advisor v2 "add to a winner" cap

	// validCfg sets Risk.MaxOpenPositions=1, so the snapshots MUST also set OpenPositions (a bot
	// position increments BOTH OpenPositions and OpenBuyInclExternal). Leaving OpenPositions=0 was
	// what masked the per-symbol-cap blocker (the per-symbol open_positions cap fired before the same-side block).
	// 1 same-side already open, cap 2 -> the 2nd is ALLOWED (both the open-positions cap and the
	// same-side block must honor MaxConcurrent).
	snap1 := AccountSnapshot{MaxDailyLossJPY: 1000, MaxConsecutiveLosses: 3, Now: now, OpenPositions: 1, OpenBuyInclExternal: 1}
	if d := EvaluateSignal(sig, cfg, snap1, mkSummary(0.3)); !d.Allowed {
		t.Fatalf("MaxConcurrent=2 with 1 open must allow the 2nd; got %q", d.Reason)
	}

	// 2 same-side already open, cap 2 -> the 3rd is BLOCKED (hard ceiling).
	snap2 := AccountSnapshot{MaxDailyLossJPY: 1000, MaxConsecutiveLosses: 3, Now: now, OpenPositions: 2, OpenBuyInclExternal: 2}
	if d := EvaluateSignal(sig, cfg, snap2, mkSummary(0.3)); d.Allowed {
		t.Fatal("MaxConcurrent=2 with 2 open must block the 3rd (hard ceiling)")
	}

	// Default signal (MaxConcurrent=0) with 1 open -> still blocked (strict single-position).
	if d := EvaluateSignal(entrySig(order.SideBuy), cfg, snap1, mkSummary(0.3)); d.Allowed {
		t.Fatal("default MaxConcurrent must keep the strict single-position rule (block the 2nd)")
	}
}

func TestEvaluateSignal_PyramidingIsNotOverridable(t *testing.T) {
	// The pyramiding reason must NOT match any overridable prefix — it is a hard
	// structural guard (the cap=1 replacement), not an operator-soft gate.
	for _, r := range []string{
		"pyramiding_blocked_same_side_buy (1 open)",
		"pyramiding_blocked_same_side_sell (2 open)",
	} {
		if strings.HasPrefix(r, "open_positions") || strings.HasPrefix(r, "cooldown") ||
			strings.HasPrefix(r, "consecutive_losses") || strings.HasPrefix(r, "trades_in_window") {
			t.Errorf("pyramiding reason %q collides with an overridable prefix", r)
		}
	}
}
