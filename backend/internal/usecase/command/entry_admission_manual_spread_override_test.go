package command

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
)

// spread is not hard safety; it is on the operator-override
// allowlist. The dashboard displays the live spread next to the manual
// trade button, so the operator pressing BUY/SELL is explicitly accepting
// the per-trade cost. emergency_stop / daily_loss / no_active_config
// remain hard (never overridable) because those are systemic safety
// reasons, not cost decisions the operator can sanely accept on the spot.
//
// Auto entry (AllowOverride=false) is unchanged: EvaluateSignal still
// rejects on spread before AllowOverride is consulted.

func TestEntryAdmission_ManualOverride_AllowsSpreadAboveCap(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	a, _ := newAdmission(t, posRepo, tradeRepo, flag, 5)

	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 5},
		Entry: config.EntrySection{Direction: config.DirectionBoth, MaxSpreadPips: 0.5},
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	// 3.0 pip spread — the GMO USD/JPY early-session reality that
	// triggered this change.
	ticker := &market.Ticker{Symbol: "USD_JPY", Bid: 158.83, Ask: 158.86, Timestamp: time.Now()}

	verdict, release, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal:        sig,
		ActiveConfig:  cfg,
		Ticker:        ticker,
		AllowOverride: true,
		Source:        "manual",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if !verdict.Allowed {
		t.Fatalf("manual override must allow spread 3.0 pips > cap 0.5 pips; got reason=%q", verdict.Reason)
	}
	if release == nil {
		t.Error("release must be non-nil on Allowed=true")
	} else {
		release()
	}
}

func TestEntryAdmission_AutoEntry_StillRejectsSpreadAboveCap(t *testing.T) {
	// Regression: changing the override allowlist must NOT loosen auto
	// entry. AllowOverride=false means EvaluateSignal's spread rejection
	// returns straight through admission (no override consideration).
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	a, _ := newAdmission(t, posRepo, tradeRepo, flag, 5)

	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 5},
		Entry: config.EntrySection{Direction: config.DirectionBoth, MaxSpreadPips: 0.5},
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	ticker := &market.Ticker{Symbol: "USD_JPY", Bid: 158.83, Ask: 158.86, Timestamp: time.Now()}

	verdict, _, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal:       sig,
		ActiveConfig: cfg,
		Ticker:       ticker,
		// AllowOverride: false (default)
		Source: "auto",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if verdict.Allowed {
		t.Fatalf("auto entry must STILL reject spread above cap; got Allowed=true")
	}
}
