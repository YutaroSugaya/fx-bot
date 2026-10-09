package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fx-bot/backend/internal/adapter/broker"
	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/usecase/command"
)

// Wiring regression: the startup Reconcile constructed by buildStartupReconcile
// must carry a non-nil Closer. Without it, a Live stale_db_position cannot
// resolve via TP/SL settle leg executions and a Paper stale_db_position
// cannot synthetic-close — both fall through to emergency_stop.
//
// This guards against main.go's startup Reconcile literal omitting Closer.

func newWiringTestRepos() *port.Repositories {
	return &port.Repositories{
		Positions:       backtest.NewInMemoryPositionRepo(),
		Trades:          backtest.NewInMemoryTradeRepo(),
		StrategyConfigs: backtest.NewInMemoryStrategyConfigRepo(),
		Closer: backtest.NewInMemoryPositionCloser(
			backtest.NewInMemoryPositionRepo(),
			backtest.NewInMemoryTradeRepo(),
		),
	}
}

func TestBuildStartupReconcile_WiresCloser(t *testing.T) {
	repos := newWiringTestRepos()
	r := buildStartupReconcile(reconcileWiringDeps{
		Broker:   &broker.MockBroker{},
		Repos:    repos,
		Symbol:   "USD_JPY",
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		LiveMode: config.ModeLiveConfig,
	})
	if r.Closer == nil {
		t.Fatal("startup Reconcile.Closer must be wired; without it Live stale_db_position cannot resolve via TP/SL legs and Paper cannot synthetic-close")
	}
	if r.StrategyConfigs == nil {
		t.Fatal("startup Reconcile.StrategyConfigs must be wired; required by Paper naked_broker_position adoption FK lookup")
	}
	if r.Mode != command.ReconcileModeStartup {
		t.Errorf("Mode = %v, want %v", r.Mode, command.ReconcileModeStartup)
	}
}

func TestBuildRuntimeReconcile_WiresCloser(t *testing.T) {
	repos := newWiringTestRepos()
	r := buildRuntimeReconcile(reconcileWiringDeps{
		Broker:   &broker.MockBroker{},
		Repos:    repos,
		Symbol:   "USD_JPY",
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		LiveMode: config.ModeLiveConfig,
	})
	if r.Closer == nil {
		t.Fatal("runtime Reconcile.Closer must be wired; runtime mode's only fix path for stale_db_position is fill resolution via Closer.CloseAndRecord")
	}
	if r.StrategyConfigs == nil {
		t.Fatal("runtime Reconcile.StrategyConfigs must be wired; the runtime loop is the only periodic reconciler, so a mid-run external position needs the active config_id to adopt — without it, it trips emergency_stop after the grace window")
	}
	if r.Mode != command.ReconcileModeRuntime {
		t.Errorf("Mode = %v, want %v", r.Mode, command.ReconcileModeRuntime)
	}
}

// End-to-end wiring assertion for the Live RUNTIME loop: a position opened
// externally (user trades in the GMO app while the bot runs) is detected as a naked broker position
// and must be ADOPTED — recorded in DB FK'd to the active live config — NOT spuriously tripped. This
// only works if buildRuntimeReconcile wires StrategyConfigs; if that wiring is dropped, adoption
// fails (extNoConfig) and the position trips emergency_stop after the grace window.
func TestBuildRuntimeReconcile_Live_ExternalPosition_AdoptsNotTrips(t *testing.T) {
	ctx := context.Background()
	flagPath := filepath.Join(t.TempDir(), "emergency_stop.flag")

	bp := position.Position{
		BrokerPositionID: "EXT-runtime-1", Symbol: "USD_JPY", Side: order.SideBuy,
		Quantity: 1000, EntryPrice: 150.0, Status: position.StatusOpen, OpenedAt: time.Now(),
	}
	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
			return []position.Position{bp}, nil
		},
	}
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	cfgRepo := backtest.NewInMemoryStrategyConfigRepo().
		SeedActive("frozen-mapb-usdjpy-v3-1", "USD_JPY", string(config.ModeLiveConfig))
	repos := &port.Repositories{
		Positions:       posRepo,
		Trades:          tradeRepo,
		StrategyConfigs: cfgRepo,
		Closer:          backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
	}

	r := buildRuntimeReconcile(reconcileWiringDeps{
		Broker:            mb,
		Repos:             repos,
		Symbol:            "USD_JPY",
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		EmergencyFlagPath: flagPath,
		LiveMode:          config.ModeLiveConfig,
	})

	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Adopted != 1 {
		t.Fatalf("Adopted = %d, want 1 (H2 wiring regression: StrategyConfigs missing → external position can't FK-adopt → trips after grace)", sum.Adopted)
	}
	if sum.Tripped != 0 {
		t.Errorf("Tripped = %d, want 0 (an adoptable external position must NOT halt the live bot)", sum.Tripped)
	}
	if _, e := os.Stat(flagPath); e == nil {
		t.Error("emergency_stop.flag must NOT exist after a successful external adoption")
	}
}

// End-to-end wiring assertion for Live startup: feed a stale DB position
// (broker doesn't have it, but the recorded TP leg has a fill) through the
// wired Reconcile and assert it resolves into a real trade. If Closer is
// ever dropped from buildStartupReconcile, the position trips
// emergency_stop instead of resolving — this catches that regression.
func TestBuildStartupReconcile_Live_StaleDB_ResolvesViaTPLeg(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	openedAt := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	closedAt := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)

	_, _ = posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 150.00,
			StrategyConfigID: "cfg-live", Status: port.PositionStatusOpen, OpenedAt: openedAt,
		},
		Live: &port.PositionLive{
			BrokerPositionID: "broker-W-1",
			TPOrderID:        "tp-w1",
			SLOrderID:        "sl-w1",
		},
	})

	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) { return nil, nil },
		GetExecutionsFn: func(_ context.Context, id string) ([]order.Execution, error) {
			if id == "tp-w1" {
				return []order.Execution{{Price: 150.40, Quantity: 100, Timestamp: closedAt}}, nil
			}
			return nil, nil
		},
	}

	repos := &port.Repositories{
		Positions:       posRepo,
		Trades:          tradeRepo,
		StrategyConfigs: backtest.NewInMemoryStrategyConfigRepo(),
		Closer:          backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
	}

	r := buildStartupReconcile(reconcileWiringDeps{
		Broker:            mb,
		Repos:             repos,
		Symbol:            "USD_JPY",
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		EmergencyFlagPath: flagPath,
		LiveMode:          config.ModeLiveConfig,
	})
	r.Clock = func() time.Time { return closedAt }

	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Resolved != 1 {
		t.Fatalf("Resolved = %d, want 1 (wiring regression: Closer missing causes emergency_stop trip instead of resolution)", sum.Resolved)
	}
	if sum.Tripped != 0 {
		t.Errorf("Tripped = %d, want 0 (successful resolution must not trip)", sum.Tripped)
	}
	if _, err := os.Stat(flagPath); err == nil {
		t.Error("emergency_stop.flag should NOT exist after successful resolution")
	}
}

// End-to-end wiring assertion for Paper startup: stale DB position must be
// synthetic-closed (MarkedDone=1), not tripped. Catches Closer wiring drop.
func TestBuildStartupReconcile_Paper_StaleDB_SyntheticCloses(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	openedAt := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)

	_, _ = posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 150.00,
			StrategyConfigID: "cfg-paper", Status: port.PositionStatusOpen, OpenedAt: openedAt,
		},
		Live: &port.PositionLive{BrokerPositionID: "broker-paper-W-1"},
	})

	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) { return nil, nil },
	}

	repos := &port.Repositories{
		Positions:       posRepo,
		Trades:          tradeRepo,
		StrategyConfigs: backtest.NewInMemoryStrategyConfigRepo(),
		Closer:          backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
	}

	r := buildStartupReconcile(reconcileWiringDeps{
		Broker:            mb,
		Repos:             repos,
		Symbol:            "USD_JPY",
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		EmergencyFlagPath: flagPath,
		LiveMode:          config.ModePaperConfig,
	})

	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.MarkedDone != 1 {
		t.Fatalf("MarkedDone = %d, want 1 (wiring regression: Closer missing prevents Paper synthetic close)", sum.MarkedDone)
	}
	if sum.Tripped != 0 {
		t.Errorf("Tripped = %d, want 0 (Paper synthetic close must not trip)", sum.Tripped)
	}
}
