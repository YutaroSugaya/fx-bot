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

// EntryAdmission must re-evaluate spread + cooldown as the final
// authoritative gate. Worker's pre-tick gate is a fast pre-filter; the
// admission lock is where the truth is. Two failing tests:
//   1. Spread above cap rejects, even if Summary is not pre-computed by the
//      caller — we accept a Ticker and derive SpreadPips internally.
//   2. Cooldown rejects when CooldownFn says so, regardless of caller-side
//      snapshot freshness (= worker missed a close that happened between
//      pre-tick and lock).

func TestEntryAdmission_SpreadAboveCap_RejectsViaTicker(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	a, _ := newAdmission(t, posRepo, tradeRepo, flag, 5)

	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 5},
		Entry: config.EntrySection{Direction: config.DirectionBoth, MaxSpreadPips: 0.5},
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	// Ticker shows 1.0 pip spread (Bid=100.00, Ask=100.01, pipSize 0.01 → 1.0 pip)
	ticker := &market.Ticker{Symbol: "USD_JPY", Bid: 100.00, Ask: 100.01, Timestamp: time.Now()}

	verdict, release, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal:       sig,
		ActiveConfig: cfg,
		Ticker:       ticker, // new field
		Source:       "auto",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if verdict.Allowed {
		t.Fatalf("spread 1.0 pip > cap 0.5 pip must reject; got Allowed=true")
	}
	if release != nil {
		t.Errorf("release should be nil on denial")
	}
}

func TestEntryAdmission_NoTicker_SkipsSpreadGateLikeBefore(t *testing.T) {
	// Regression: when no Ticker is supplied (e.g. integration test path with
	// non-spread data), admission must NOT crash and the spread gate must be
	// skipped (matching the earlier behavior so callers without a ticker still
	// function).
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	a, _ := newAdmission(t, posRepo, tradeRepo, flag, 5)

	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 5},
		Entry: config.EntrySection{Direction: config.DirectionBoth, MaxSpreadPips: 0.5},
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	verdict, _, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal:       sig,
		ActiveConfig: cfg,
		Source:       "auto",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if !verdict.Allowed {
		t.Errorf("no ticker should skip spread gate; got rejected reason=%q", verdict.Reason)
	}
}

func TestEntryAdmission_CooldownFnReturnsTrue_RejectsAuto(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	a, _ := newAdmission(t, posRepo, tradeRepo, flag, 5)
	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	a.Clock = func() time.Time { return now }
	// Inject a cooldown evaluator that says "we are in cooldown".
	a.CooldownFn = func(_ time.Time) (bool, time.Time, string) {
		return true, now.Add(30 * time.Second), "after_loss"
	}

	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 5},
		Entry: config.EntrySection{Direction: config.DirectionBoth},
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	verdict, release, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal:       sig,
		ActiveConfig: cfg,
		Source:       "auto",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if verdict.Allowed {
		t.Fatalf("cooldown active must reject auto entry; got Allowed=true")
	}
	if release != nil {
		t.Errorf("release should be nil on denial")
	}
}

func TestEntryAdmission_CooldownFnNil_SkipsCooldownGate(t *testing.T) {
	// Backwards compat: when CooldownFn is nil (e.g. callers not yet
	// migrated), cooldown gate is silently disabled.
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	a, _ := newAdmission(t, posRepo, tradeRepo, flag, 5)
	a.CooldownFn = nil // explicit

	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 5},
		Entry: config.EntrySection{Direction: config.DirectionBoth},
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	verdict, _, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal:       sig,
		ActiveConfig: cfg,
		Source:       "auto",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if !verdict.Allowed {
		t.Errorf("nil CooldownFn must not reject; got reason=%q", verdict.Reason)
	}
}
