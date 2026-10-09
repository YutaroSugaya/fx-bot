package command

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
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedActiveConfig returns an InMemoryStrategyConfigRepo with a single
// active row for (USD_JPY, paper_config). Returns an empty repo when
// configID is "" so GetActive returns (nil, nil) (= reconcile's
// no-active-config branch).
func seedActiveConfig(configID string) *backtest.InMemoryStrategyConfigRepo {
	repo := backtest.NewInMemoryStrategyConfigRepo()
	if configID != "" {
		repo.SeedActive(configID, "USD_JPY", "paper_config")
	}
	return repo
}

// Startup mode (recovery after a restart):
//   - broker has position the DB doesn't → ADOPT into DB
//   - DB has OPEN that broker no longer has → MarkClosed
//   - neither case trips emergency_stop (recovery is normal at startup)
//
// Runtime mode (paper):
//   - discrepancy → emergency_stop trip (Live runtime: see the ReconcileMode doc)
func TestReconcile_StartupMode_AdoptsAndSyncs(t *testing.T) {
	ctx := context.Background()

	t.Run("broker has position DB doesnt: adopts into DB without tripping", func(t *testing.T) {
		dir := t.TempDir()
		flagPath := filepath.Join(dir, "emergency_stop.flag")
		brokerPos := position.Position{
			BrokerPositionID: "broker-XYZ", Symbol: "USD_JPY",
			Side: order.SideBuy, Quantity: 100, EntryPrice: 150.10,
			Status: position.StatusOpen, OpenedAt: time.Now(),
		}
		mb := &broker.MockBroker{
			GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
				return []position.Position{brokerPos}, nil
			},
		}
		posRepo := backtest.NewInMemoryPositionRepo()
		// Step B: positions.strategy_config_id is a NOT NULL FK; the adopt
		// path resolves the active paper config via this seeded repo.
		strat := seedActiveConfig("cfg-active-paper")

		r := &Reconcile{
			Broker: mb, Positions: posRepo, StrategyConfigs: strat,
			Symbol: "USD_JPY",
			Logger: discardLogger(), EmergencyFlagPath: flagPath,
			Mode: ReconcileModeStartup,
		}
		sum, err := r.Run(ctx)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if sum.Adopted != 1 || sum.Tripped != 0 {
			t.Errorf("summary: %+v", sum)
		}
		open, _ := posRepo.ListOpenOrClosing(ctx, "USD_JPY")
		if len(open) != 1 {
			t.Errorf("DB should have 1 adopted row, got %d", len(open))
		}
		live, _ := posRepo.GetLive(ctx, open[0].ID)
		if live == nil || live.BrokerPositionID != "broker-XYZ" {
			t.Errorf("adopted row should have positions_live row with broker_position_id; got %+v", live)
		}
		if open[0].StrategyConfigID != "cfg-active-paper" {
			t.Errorf("adopted row should FK to active paper config; got %q", open[0].StrategyConfigID)
		}
		if _, err := os.Stat(flagPath); err == nil {
			t.Errorf("emergency_stop should NOT be tripped in Startup mode")
		}
	})

	t.Run("adopt with no active paper config trips emergency_stop", func(t *testing.T) {
		dir := t.TempDir()
		flagPath := filepath.Join(dir, "emergency_stop.flag")
		mb := &broker.MockBroker{
			GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
				return []position.Position{{
					BrokerPositionID: "broker-noconfig", Symbol: "USD_JPY",
					Side: order.SideBuy, Quantity: 100, EntryPrice: 150.10,
					Status: position.StatusOpen, OpenedAt: time.Now(),
				}}, nil
			},
		}
		posRepo := backtest.NewInMemoryPositionRepo()
		strat := seedActiveConfig("") // empty → GetActive returns (nil, nil)

		r := &Reconcile{
			Broker: mb, Positions: posRepo, StrategyConfigs: strat,
			Symbol: "USD_JPY",
			Logger: discardLogger(), EmergencyFlagPath: flagPath,
			Mode: ReconcileModeStartup,
		}
		sum, _ := r.Run(ctx)
		if sum.Tripped == 0 {
			t.Errorf("expected Tripped > 0 when no active config; got %+v", sum)
		}
		if _, err := os.Stat(flagPath); err != nil {
			t.Errorf("flag should exist after no-active-config trip: %v", err)
		}
	})

	t.Run("DB OPEN broker has nothing: marks DB closed via synthetic trade", func(t *testing.T) {
		dir := t.TempDir()
		flagPath := filepath.Join(dir, "emergency_stop.flag")
		posRepo := backtest.NewInMemoryPositionRepo()
		tradeRepo := backtest.NewInMemoryTradeRepo()
		_, _ = posRepo.Insert(ctx, port.PositionInsertInput{
			Position: port.PositionRecord{
				Symbol: "USD_JPY", Side: "BUY", Quantity: 100,
				StrategyConfigID: "cfg-test",
				Status:           port.PositionStatusOpen, OpenedAt: time.Now(),
			},
			Live: &port.PositionLive{BrokerPositionID: "broker-Z"},
		})
		mb := &broker.MockBroker{
			GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
				return nil, nil
			},
		}
		r := &Reconcile{
			Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
			Logger: discardLogger(), EmergencyFlagPath: flagPath,
			Mode:   ReconcileModeStartup,
			Closer: backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		}
		sum, _ := r.Run(ctx)
		if sum.MarkedDone != 1 || sum.Tripped != 0 {
			t.Errorf("summary: %+v", sum)
		}
		open, _ := posRepo.ListOpenOrClosing(ctx, "USD_JPY")
		if len(open) != 0 {
			t.Errorf("DB OPEN/CLOSING should be empty after startup close, got %d rows", len(open))
		}
		if _, err := os.Stat(flagPath); err == nil {
			t.Errorf("emergency_stop should NOT be tripped in Startup mode")
		}
	})

	t.Run("clean state: no adopt, no MarkClosed, no trip", func(t *testing.T) {
		dir := t.TempDir()
		flagPath := filepath.Join(dir, "emergency_stop.flag")
		pid := "broker-CLEAN"
		bp := position.Position{
			BrokerPositionID: pid, Symbol: "USD_JPY", Side: order.SideBuy, Quantity: 100,
			EntryPrice: 150.10, Status: position.StatusOpen, OpenedAt: time.Now(),
		}
		mb := &broker.MockBroker{
			GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
				return []position.Position{bp}, nil
			},
		}
		posRepo := backtest.NewInMemoryPositionRepo()
		_, _ = posRepo.Insert(ctx, port.PositionInsertInput{
			Position: port.PositionRecord{
				Symbol: "USD_JPY", Side: "BUY", Quantity: 100,
				StrategyConfigID: "cfg-test",
				Status:           port.PositionStatusOpen, OpenedAt: time.Now(),
			},
			Live: &port.PositionLive{BrokerPositionID: pid},
		})
		r := &Reconcile{
			Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
			Logger: discardLogger(), EmergencyFlagPath: flagPath,
			Mode: ReconcileModeStartup,
		}
		sum, _ := r.Run(ctx)
		if sum.Adopted != 0 || sum.MarkedDone != 0 || sum.Tripped != 0 {
			t.Errorf("clean state: %+v", sum)
		}
		if _, err := os.Stat(flagPath); err == nil {
			t.Errorf("emergency_stop should NOT be tripped on clean state")
		}
	})
}

func TestReconcile_RuntimeMode_TripsOnDiscrepancy(t *testing.T) {
	ctx := context.Background()

	t.Run("naked broker position trips emergency_stop", func(t *testing.T) {
		dir := t.TempDir()
		flagPath := filepath.Join(dir, "emergency_stop.flag")
		mb := &broker.MockBroker{
			GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
				return []position.Position{{
					BrokerPositionID: "broker-X", Symbol: "USD_JPY",
					Side: order.SideBuy, Quantity: 100, EntryPrice: 150.10,
					Status: position.StatusOpen, OpenedAt: time.Now(),
				}}, nil
			},
		}
		r := &Reconcile{
			Broker: mb, Positions: backtest.NewInMemoryPositionRepo(),
			Symbol: "USD_JPY", Logger: discardLogger(),
			EmergencyFlagPath: flagPath, Mode: ReconcileModeRuntime,
		}
		sum, _ := r.Run(ctx)
		if sum.Tripped == 0 {
			t.Errorf("Runtime mode should trip; got %+v", sum)
		}
		if _, err := os.Stat(flagPath); err != nil {
			t.Errorf("flag should exist: %v", err)
		}
	})

	t.Run("stale DB position trips emergency_stop", func(t *testing.T) {
		dir := t.TempDir()
		flagPath := filepath.Join(dir, "emergency_stop.flag")
		posRepo := backtest.NewInMemoryPositionRepo()
		_, _ = posRepo.Insert(ctx, port.PositionInsertInput{
			Position: port.PositionRecord{
				Symbol: "USD_JPY", Side: "BUY", Quantity: 100,
				StrategyConfigID: "cfg-test",
				Status:           port.PositionStatusOpen, OpenedAt: time.Now(),
			},
			Live: &port.PositionLive{BrokerPositionID: "broker-Z"},
		})
		mb := &broker.MockBroker{
			GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
				return nil, nil
			},
		}
		r := &Reconcile{
			Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
			Logger: discardLogger(), EmergencyFlagPath: flagPath,
			Mode: ReconcileModeRuntime,
		}
		sum, _ := r.Run(ctx)
		if sum.Tripped == 0 {
			t.Errorf("Runtime stale DB position should trip; got %+v", sum)
		}
	})
}
