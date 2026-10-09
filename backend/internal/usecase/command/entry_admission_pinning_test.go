package command

import (
	"context"
	"testing"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/safety"
)

// Pinning: emergency_stop must HARD-block every new
// entry, with NO operator override. This invariant guards every strategy and a
// retune must never weaken it. Non-integration so it runs in `make check-backend`.
// (The OCO-required / CLOSING-no-ratchet pins are DB-backed
// integration tests under `make test-integration`.)
func TestPinning_EmergencyStopBlocksEntry_EvenWithOverride(t *testing.T) {
	flag := tempFlag(t)
	a, _ := newAdmission(t, backtest.NewInMemoryPositionRepo(), backtest.NewInMemoryTradeRepo(), flag, 1)
	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 1},
		Entry: config.EntrySection{Direction: config.DirectionBoth},
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}

	if err := safety.Trip(flag, "pinning-test"); err != nil {
		t.Fatalf("trip: %v", err)
	}

	// auto entry → blocked.
	v, _, err := a.CheckAndHold(context.Background(), AdmissionRequest{Signal: sig, ActiveConfig: cfg, Source: "auto"})
	if err != nil {
		t.Fatalf("admission(auto): %v", err)
	}
	if v.Allowed || v.Reason != "emergency_stop" {
		t.Fatalf("auto entry under emergency_stop: allowed=%v reason=%q, want blocked/emergency_stop", v.Allowed, v.Reason)
	}

	// manual entry WITH operator override → still blocked (emergency_stop is a
	// hard gate, NOT in the overridable allowlist).
	v2, _, err := a.CheckAndHold(context.Background(), AdmissionRequest{Signal: sig, ActiveConfig: cfg, Source: "manual", AllowOverride: true})
	if err != nil {
		t.Fatalf("admission(manual override): %v", err)
	}
	if v2.Allowed {
		t.Fatal("emergency_stop must NOT be operator-overridable")
	}
}
