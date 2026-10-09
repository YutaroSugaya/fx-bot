package risk

import (
	"strings"
	"testing"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
)

// EvaluateHardSafety must check ONLY the never-overridable hard gates
// (no_active_config / emergency_stop / event_freeze / daily_loss /
// account_daily_loss / reentry_cooldown / account_open_positions / pyramiding). It must NOT
// reject on overridable gates (cooldown / consecutive_losses /
// open_positions / trades_in_window / loss_in_window / direction_* /
// spread).
//
// Why this exists: EvaluateSignal short-circuits on the first matching
// reason, so EntryAdmission's override path (which sees one reason from
// EvaluateSignal and bypasses it when on the allowlist) used to skip
// over later, hard-safety rejections.
// EvaluateHardSafety gives admission a clean second-pass gate that
// AllowOverride cannot bypass.
//
// `spread` is not hard safety; it is on the overridable allowlist. Rationale: the operator pressing the manual
// trade button has the current spread displayed in the dashboard and is
// accepting the explicit per-trade transaction cost (1 pip ≈ qty * pip
// size). Spread is a cost gate, not a safety gate. Auto-entry callers
// still reject on spread because they pass AllowOverride=false.

func TestEvaluateHardSafety_AllowsWhenNoHardReason(t *testing.T) {
	cfg := &config.StrategyConfig{
		Entry: config.EntrySection{Direction: config.DirectionBoth, MaxSpreadPips: 5},
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10, ConfigID: "c1"}
	d := EvaluateHardSafety(sig, cfg, AccountSnapshot{}, nil)
	if !d.Allowed {
		t.Fatalf("expected allow, got reason=%q", d.Reason)
	}
}

func TestEvaluateHardSafety_NoActiveConfig(t *testing.T) {
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy}
	d := EvaluateHardSafety(sig, nil, AccountSnapshot{}, nil)
	if d.Allowed || d.Reason != "no_active_config" {
		t.Fatalf("expected no_active_config, got %+v", d)
	}
}

func TestEvaluateHardSafety_EmergencyStop(t *testing.T) {
	cfg := &config.StrategyConfig{Entry: config.EntrySection{Direction: config.DirectionBoth}}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy}
	snap := AccountSnapshot{EmergencyStop: true}
	d := EvaluateHardSafety(sig, cfg, snap, nil)
	if d.Allowed || d.Reason != "emergency_stop" {
		t.Fatalf("expected emergency_stop, got %+v", d)
	}
}

func TestEvaluateHardSafety_DailyLossExceeded(t *testing.T) {
	cfg := &config.StrategyConfig{Entry: config.EntrySection{Direction: config.DirectionBoth}}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy}
	snap := AccountSnapshot{MaxDailyLossJPY: 5000, DailyLossJPY: 6000}
	d := EvaluateHardSafety(sig, cfg, snap, nil)
	if d.Allowed || !strings.HasPrefix(d.Reason, "daily_loss") {
		t.Fatalf("expected daily_loss prefix, got %+v", d)
	}
}

// spread is overridable. EvaluateHardSafety must NOT
// reject on a wide spread — admission relies on this so the manual
// override path can let an operator-confirmed trade through. Auto entry
// is still gated because EvaluateSignal (called first by admission)
// rejects spread before AllowOverride is consulted.
func TestEvaluateHardSafety_IgnoresSpread(t *testing.T) {
	cfg := &config.StrategyConfig{Entry: config.EntrySection{Direction: config.DirectionBoth, MaxSpreadPips: 1.0}}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy}
	summary := &market.MarketSummary{CurrentRate: market.CurrentRate{SpreadPips: 2.5}}
	d := EvaluateHardSafety(sig, cfg, AccountSnapshot{}, summary)
	if !d.Allowed {
		t.Fatalf("hard safety must NOT trip on spread; got reason=%q", d.Reason)
	}
}

// The point of the new function: hard-safety check must IGNORE
// overridable gates entirely, so admission's override path can rely on
// it.

func TestEvaluateHardSafety_IgnoresCooldown(t *testing.T) {
	cfg := &config.StrategyConfig{Entry: config.EntrySection{Direction: config.DirectionBoth}}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy}
	snap := AccountSnapshot{InCooldown: true, CooldownKind: "after_loss"}
	d := EvaluateHardSafety(sig, cfg, snap, nil)
	if !d.Allowed {
		t.Fatalf("hard safety must NOT trip on cooldown; got reason=%q", d.Reason)
	}
}

func TestEvaluateHardSafety_IgnoresConsecutiveLosses(t *testing.T) {
	cfg := &config.StrategyConfig{Entry: config.EntrySection{Direction: config.DirectionBoth}}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy}
	snap := AccountSnapshot{MaxConsecutiveLosses: 3, ConsecutiveLosses: 5}
	d := EvaluateHardSafety(sig, cfg, snap, nil)
	if !d.Allowed {
		t.Fatalf("hard safety must NOT trip on consecutive_losses; got reason=%q", d.Reason)
	}
}

func TestEvaluateHardSafety_IgnoresOpenPositions(t *testing.T) {
	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 1},
		Entry: config.EntrySection{Direction: config.DirectionBoth},
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy}
	snap := AccountSnapshot{OpenPositions: 5}
	d := EvaluateHardSafety(sig, cfg, snap, nil)
	if !d.Allowed {
		t.Fatalf("hard safety must NOT trip on open_positions; got reason=%q", d.Reason)
	}
}

func TestEvaluateHardSafety_IgnoresDirectionNone(t *testing.T) {
	cfg := &config.StrategyConfig{Entry: config.EntrySection{Direction: config.DirectionNone}}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy}
	d := EvaluateHardSafety(sig, cfg, AccountSnapshot{}, nil)
	if !d.Allowed {
		t.Fatalf("hard safety must NOT trip on direction_none; got reason=%q", d.Reason)
	}
}

// Composite: a snapshot that fails BOTH cooldown (overridable) and
// daily_loss (hard). EvaluateSignal returns cooldown first.
// EvaluateHardSafety must surface daily_loss regardless.
func TestEvaluateHardSafety_CooldownPlusDailyLoss_ReturnsDailyLoss(t *testing.T) {
	cfg := &config.StrategyConfig{Entry: config.EntrySection{Direction: config.DirectionBoth}}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy}
	snap := AccountSnapshot{
		InCooldown:      true,
		CooldownKind:    "after_loss",
		MaxDailyLossJPY: 5000,
		DailyLossJPY:    6000,
	}
	d := EvaluateHardSafety(sig, cfg, snap, nil)
	if d.Allowed || !strings.HasPrefix(d.Reason, "daily_loss") {
		t.Fatalf("expected daily_loss to surface despite cooldown also tripped; got %+v", d)
	}
}
