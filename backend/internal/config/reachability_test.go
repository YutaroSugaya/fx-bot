package config

import (
	"testing"
	"time"
)

// Dead-market churn: when the 1h range is compressed, a TP several times larger
// than the range is never approachable within a short hold window, so trades only
// resolve via full SL or a breakeven early_exit — a badly inverted realized RR.
// The prompt already says "TP ≤ range_1h × 1.3" but the advisor can ignore it;
// UnreachableTPReason + DowngradeToNoTrade make it a hard backstop (downgrade,
// NOT reject — rejecting would freeze the stale active config).
func TestUnreachableTPReason_DowngradesWhenTPExceedsRangeMultiple(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	c := validStrategyConfig(now)
	c.Exit.TakeProfitPips = 10 // a typical scalp-probe TP
	const range1h = 4.0        // compressed: 1.3×4 = 5.2 < 10 → unreachable

	reason, downgrade := UnreachableTPReason(c, range1h)
	if !downgrade {
		t.Fatalf("TP 10 vs 1h range 4 (max %.1f) must downgrade", ReachabilityTPRangeMultiple*range1h)
	}
	if reason == "" {
		t.Error("downgrade must carry a non-empty reason for the audit trail")
	}
}

func TestUnreachableTPReason_AllowsReachableTP(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	c := validStrategyConfig(now)
	c.Exit.TakeProfitPips = 10
	// 1h range 20 → 1.3×20 = 26 ≥ 10 → reachable, no downgrade.
	if _, downgrade := UnreachableTPReason(c, 20.0); downgrade {
		t.Error("TP 10 vs 1h range 20 is reachable; must not downgrade")
	}
}

// Fail open: an unknown/zero 1h range (missing candles) must NOT force no_trade —
// we only block when we positively know the range is too small for the TP.
func TestUnreachableTPReason_UnknownRangeFailsOpen(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	c := validStrategyConfig(now)
	c.Exit.TakeProfitPips = 10
	if _, downgrade := UnreachableTPReason(c, 0); downgrade {
		t.Error("unknown (0) 1h range must fail open (no downgrade)")
	}
}

// A config that is already no_trade has no TP to judge.
func TestUnreachableTPReason_NoTradeIsNoOp(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	c := validStrategyConfig(now)
	c.Enabled = false
	c.Strategy.Name = StrategyNoTrade
	if _, downgrade := UnreachableTPReason(c, 1.0); downgrade {
		t.Error("already-no_trade config must not be flagged for downgrade")
	}
}

func TestDowngradeToNoTrade_ProducesValidNoTradeConfig(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	c := validStrategyConfig(now)
	c.DowngradeToNoTrade("dead_market: TP 10 > 5.2")

	if !c.IsNoTradeDecision() {
		t.Fatal("downgraded config must read as a no_trade decision")
	}
	if c.Enabled {
		t.Error("downgraded config must be disabled")
	}
	if c.Strategy.Name != StrategyNoTrade {
		t.Errorf("strategy.name = %q, want no_trade", c.Strategy.Name)
	}
	if !c.NoTrade.Enabled || c.NoTrade.Reason == "" {
		t.Errorf("no_trade block must be enabled with a reason, got %+v", c.NoTrade)
	}
	if c.Exit.TakeProfitPips != 0 || c.Exit.StopLossPips != 0 {
		t.Errorf("exit must be zeroed (inert under no_trade), got %+v", c.Exit)
	}
	if c.Entry.Direction != DirectionNone || c.Risk.Quantity != 0 {
		t.Errorf("entry/risk must be canonicalized, dir=%q qty=%d", c.Entry.Direction, c.Risk.Quantity)
	}
	// Identity/validity window is preserved so the no_trade decision keeps the
	// same config_id and is active for the intended window.
	if c.ConfigID != "20260515-100000-usdjpy" || !c.ValidUntil.After(c.ValidFrom) {
		t.Errorf("identity/validity must be preserved: id=%q from=%v until=%v", c.ConfigID, c.ValidFrom, c.ValidUntil)
	}
}

// The downgraded config must survive the strict Validator (no_trade is exempt
// from the entry-side guards), so promotion activates it rather than rejecting.
func TestDowngradeToNoTrade_PassesValidator(t *testing.T) {
	now := time.Now()
	c := validStrategyConfig(now)
	c.DowngradeToNoTrade("dead_market")

	v := newValidator(now)
	res := v.ValidateAll(c, AccountState{MaxDailyLossJPY: 1000, MaxConsecutiveLosses: 3, MaxOpenPositions: 2})
	if !res.OK() {
		t.Errorf("downgraded no_trade config must validate, got: %s", res.Summary())
	}
}
