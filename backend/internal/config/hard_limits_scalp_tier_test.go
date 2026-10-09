package config

import (
	"testing"
	"time"
)

// The tracked hard_limits.yaml is not a day-trade-only floor
// (TP≥15 / SL≥15 / MaxHold≥240); it also admits a conservative scalp tier
// (TP 8-20 / SL 6-15 / MaxHold 30-120) so the bot can trade more frequently.
// This pins that a representative scalp config is accepted by the *tracked*
// hard_limits.yaml (not an in-code fixture) — the change is data, so the test
// guards the data.
func TestHardLimitsYAML_AcceptsScalpTier(t *testing.T) {
	h, err := LoadHardLimits("../../../configs/hard_limits.yaml")
	if err != nil {
		t.Fatalf("load hard_limits.yaml: %v", err)
	}
	now := time.Now()
	v := NewValidator(h)
	v.Now = func() time.Time { return now }

	c := validStrategyConfig(now) // momentum_pullback, enabled
	c.Risk.Quantity = 1000        // tracked hard_limits quantity floor is 1000
	// Scalp-sized exit: TP10/SL8/MaxHold30. Must still satisfy the schema
	// invariants: ratchet locked = arm-give = 5 ≥ TP×0.5=5, early_exit ≤ MaxHold.
	c.Exit.TakeProfitPips = 10
	c.Exit.StopLossPips = 8
	c.Exit.MaxHoldMinutes = 30
	c.Exit.EarlyExitWindowMinutes = 15
	c.Exit.EarlyExitTargetPips = -2
	c.Exit.RatchetArmPips = 7
	c.Exit.RatchetGivebackPips = 2

	if r := v.ValidateSchema(c); !r.OK() {
		t.Fatalf("scalp config failed schema: %s", r.Summary())
	}
	if r := v.ValidateHardLimit(c); !r.OK() {
		t.Fatalf("scalp config rejected by tracked hard_limits (ranges not scalp-ready): %s", r.Summary())
	}
}
