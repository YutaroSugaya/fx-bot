package strategy

import (
	"os"
	"testing"

	"fx-bot/backend/internal/config"
)

// TestMAPullback_AuditConfigMatchesStrategyConstants is the
// equivalence pin: the frozen ma_pullback configs carry an "honest audit row"
// (ratchet arm/give + daytrade time stop) that must stay equal to the strategy
// constants the bot actually emits. Without this, changing a constant in
// ma_pullback.go would silently make the live audit rows lie about the exits.
func TestMAPullback_AuditConfigMatchesStrategyConstants(t *testing.T) {
	arm, give, maxHold := MAPullbackAuditExits()
	files := []string{
		"../../../../configs/frozen_ma_pullback_USD_JPY.yaml",
		"../../../../configs/frozen_ma_pullback_EUR_JPY.yaml",
		"../../../../configs/frozen_ma_pullback_GBP_JPY.yaml",
		"../../../../configs/frozen_ma_pullback_EUR_USD.yaml",
		"../../../../configs/frozen_ma_pullback_GBP_USD.yaml",
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		cfg, err := config.ParseStrategyConfig(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		if cfg.Exit.RatchetArmPips != arm {
			t.Errorf("%s: ratchet_arm_pips=%.1f, strategy constant=%.1f", f, cfg.Exit.RatchetArmPips, arm)
		}
		if cfg.Exit.RatchetGivebackPips != give {
			t.Errorf("%s: ratchet_giveback_pips=%.1f, strategy constant=%.1f", f, cfg.Exit.RatchetGivebackPips, give)
		}
		if cfg.Exit.MaxHoldMinutes != maxHold {
			t.Errorf("%s: max_hold_minutes=%d, strategy constant=%d", f, cfg.Exit.MaxHoldMinutes, maxHold)
		}
		// Provenance: these configs must classify as strategy_computed (whether
		// via explicit exit_policy or the ma_pullback default).
		if !cfg.IsStrategyComputedExit() {
			t.Errorf("%s: must be strategy_computed (audit-row equivalence)", f)
		}
	}
}
