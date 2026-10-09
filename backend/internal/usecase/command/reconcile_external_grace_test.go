package command

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fx-bot/backend/internal/adapter/broker"
	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
)

// ExternalAdoptGrace: a Live naked broker position with no active config must NOT
// trip emergency_stop on first detection — it is DEFERRED for the grace window so transient broker
// artifacts (gone by the next pass) never halt the bot. Only a position that PERSISTS unadoptable
// past the window trips. Prevents recurring spurious stops from such transient artifacts.

func nakedNoConfigReconcile(t *testing.T, flagPath string, pos []position.Position, clk *time.Time) *Reconcile {
	t.Helper()
	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) { return pos, nil },
	}
	return &Reconcile{
		Broker: mb, Positions: backtest.NewInMemoryPositionRepo(),
		StrategyConfigs: backtest.NewInMemoryStrategyConfigRepo(), // NO active config -> unadoptable
		Symbol:          "USD_JPY",
		Logger:          discardLogger(), EmergencyFlagPath: flagPath,
		Mode: ReconcileModeRuntime, LiveMode: config.ModeLiveConfig,
		ExternalAdoptGrace: 3 * time.Minute,
		Clock:              func() time.Time { return *clk },
	}
}

func TestReconcile_ExternalAdoptGrace_DefersThenTrips(t *testing.T) {
	ctx := context.Background()
	flagPath := filepath.Join(t.TempDir(), "emergency_stop.flag")
	bp := position.Position{
		BrokerPositionID: "EXT-grace-1", Symbol: "USD_JPY", Side: order.SideBuy,
		Quantity: 1000, EntryPrice: 150.0, Status: position.StatusOpen, OpenedAt: time.Now(),
	}
	t0 := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	clk := t0
	r := nakedNoConfigReconcile(t, flagPath, []position.Position{bp}, &clk)

	// Pass 1 (within grace): DEFER, no trip.
	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	if sum.Deferred != 1 || sum.Tripped != 0 {
		t.Fatalf("pass1 want Deferred=1 Tripped=0, got %+v", sum)
	}
	if _, e := os.Stat(flagPath); e == nil {
		t.Fatalf("emergency_stop must NOT trip within grace")
	}

	// Pass 2 (still within grace): still no trip.
	clk = t0.Add(1 * time.Minute)
	if sum, _ = r.Run(ctx); sum.Tripped != 0 {
		t.Fatalf("pass2 (1m<grace) must not trip, got %+v", sum)
	}

	// Pass 3 (past grace, still present): TRIP.
	clk = t0.Add(4 * time.Minute)
	sum, _ = r.Run(ctx)
	if sum.Tripped != 1 {
		t.Fatalf("pass3 (4m>grace) want Tripped=1, got %+v", sum)
	}
	if _, e := os.Stat(flagPath); e != nil {
		t.Fatalf("emergency_stop MUST trip after grace on a persistent unadoptable position")
	}
}

func TestReconcile_ExternalAdoptGrace_TransientNeverTrips(t *testing.T) {
	ctx := context.Background()
	flagPath := filepath.Join(t.TempDir(), "emergency_stop.flag")
	bp := position.Position{
		BrokerPositionID: "EXT-transient", Symbol: "USD_JPY", Side: order.SideBuy,
		Quantity: 1000, EntryPrice: 150.0, Status: position.StatusOpen, OpenedAt: time.Now(),
	}
	t0 := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	clk := t0
	present := []position.Position{bp}
	r := nakedNoConfigReconcile(t, flagPath, present, &clk)
	// Swap the broker feed to empty after pass 1 (the position vanished).
	mb := r.Broker.(*broker.MockBroker)

	if sum, _ := r.Run(ctx); sum.Deferred != 1 {
		t.Fatalf("pass1 want Deferred=1, got %+v", sum)
	}
	// Position gone; even far past the grace window it must never trip (pruned, no longer present).
	mb.GetOpenPositionsFn = func(_ context.Context, _ string) ([]position.Position, error) {
		return nil, nil
	}
	clk = t0.Add(10 * time.Minute)
	if sum, _ := r.Run(ctx); sum.Tripped != 0 {
		t.Fatalf("transient (vanished) position must never trip, got %+v", sum)
	}
	if _, e := os.Stat(flagPath); e == nil {
		t.Fatalf("emergency_stop must NOT trip for a vanished transient position")
	}
}
