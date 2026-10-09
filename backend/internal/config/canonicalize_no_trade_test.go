package config

import (
	"reflect"
	"testing"
	"time"
)

// Claude can emit a textbook no_trade config (enabled=false,
// strategy.name=no_trade, no_trade.enabled=true, all exit/risk zeroed) but leave
// entry.direction at "both". ValidateSchema's no_trade-consistency block rejects
// it, the previous config stays active past its valid_until, and the advisor's
// fresh judgment is discarded. CanonicalizeNoTrade zeroes the
// inert entry/exit/risk fields so a clear no_trade judgment is never thrown away
// over a single leftover field.
func TestCanonicalizeNoTrade_SalvagesVestigialDirection(t *testing.T) {
	now := time.Date(2026, 6, 3, 10, 40, 0, 0, time.UTC)
	c := validStrategyConfig(now) // enabled trading config (direction=both, real tp/sl)
	// Flip only the no_trade signals, leaving the entry/exit/risk fields dirty —
	// exactly the shape of a rejected no_trade config.
	c.Enabled = false
	c.Strategy.Name = StrategyNoTrade
	c.NoTrade = NoTradeSection{Enabled: true, Reason: "regime unclear"}

	c.CanonicalizeNoTrade()

	if c.Entry.Direction != DirectionNone {
		t.Errorf("entry.direction = %q, want %q", c.Entry.Direction, DirectionNone)
	}
	if c.Risk.Quantity != 0 {
		t.Errorf("risk.quantity = %d, want 0", c.Risk.Quantity)
	}
	if c.Exit.TakeProfitPips != 0 || c.Exit.StopLossPips != 0 || c.Exit.MaxHoldMinutes != 0 {
		t.Errorf("exit tp/sl/maxhold not zeroed: %+v", c.Exit)
	}
	if c.Exit.RatchetArmPips != 0 || c.Exit.RatchetGivebackPips != 0 {
		t.Errorf("exit ratchet not zeroed: arm=%v give=%v", c.Exit.RatchetArmPips, c.Exit.RatchetGivebackPips)
	}
	if c.Strategy.Name != StrategyNoTrade {
		t.Errorf("strategy.name = %q, want %q", c.Strategy.Name, StrategyNoTrade)
	}

	// The whole point: a canonicalized no_trade config must pass ValidateSchema.
	v := newValidator(now)
	if r := v.ValidateSchema(c); !r.OK() {
		t.Errorf("canonicalized no_trade config still rejected: %s", r.Summary())
	}
}

// enabled=false alone (strategy.name still a trade strategy) is a no_trade
// decision too — canonicalize must coerce strategy.name to no_trade so the
// schema block (which requires name==no_trade) cannot reject it either.
func TestCanonicalizeNoTrade_EnabledFalseCoercesStrategyName(t *testing.T) {
	now := time.Date(2026, 6, 3, 10, 40, 0, 0, time.UTC)
	c := validStrategyConfig(now)
	c.Enabled = false // only signal; strategy.name still momentum_pullback

	c.CanonicalizeNoTrade()

	if c.Strategy.Name != StrategyNoTrade {
		t.Errorf("strategy.name = %q, want %q", c.Strategy.Name, StrategyNoTrade)
	}
	if c.Entry.Direction != DirectionNone {
		t.Errorf("entry.direction = %q, want %q", c.Entry.Direction, DirectionNone)
	}
	v := newValidator(now)
	if r := v.ValidateSchema(c); !r.OK() {
		t.Errorf("canonicalized no_trade config still rejected: %s", r.Summary())
	}
}

// A genuine enabled trading config must be left completely untouched.
func TestCanonicalizeNoTrade_LeavesEnabledTradeConfigUntouched(t *testing.T) {
	now := time.Date(2026, 6, 3, 10, 40, 0, 0, time.UTC)
	c := validStrategyConfig(now)
	before := *c // copy

	c.CanonicalizeNoTrade()

	if !reflect.DeepEqual(*c, before) {
		t.Errorf("enabled trade config was mutated:\n before=%+v\n after =%+v", before, *c)
	}
}

func TestCanonicalizeNoTrade_NilSafe(t *testing.T) {
	var c *StrategyConfig
	c.CanonicalizeNoTrade() // must not panic
}
