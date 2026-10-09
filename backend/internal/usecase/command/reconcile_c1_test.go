package command

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"fx-bot/backend/internal/adapter/broker"
	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
)

// Regression: a position left in CLOSING state by a crashed close saga
// must be visible to runtime reconcile via Positions.ListOpenOrClosing
// (SQL `WHERE status IN ('OPEN','CLOSING')`). The SQL already includes
// CLOSING; this pins it with explicit test coverage.
//
// Scenario: broker closed the position (TP fired), saga crashed mid-flight
// → DB row is stuck in CLOSING. Runtime reconcile must spot it and resolve
// via the recorded TP settle leg.
func TestReconcile_LiveRuntime_StaleClosingRow_ResolvesViaTPLeg(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	openedAt := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	closedAt := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)

	id, _ := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 150.00,
			StrategyConfigID: "cfg-live",
			Status:           port.PositionStatusOpen, // initially OPEN
			OpenedAt:         openedAt,
		},
		Live: &port.PositionLive{
			BrokerPositionID: "broker-Crashed",
			TPOrderID:        "tp-c1",
			SLOrderID:        "sl-c1",
		},
	})
	// Simulate a crashed close saga: ClaimForClose succeeded (= OPEN → CLOSING)
	// but the broker call (or CloseAndRecord) didn't finish. DB row sits in
	// CLOSING.
	if ok, err := posRepo.ClaimForClose(ctx, id, openedAt.Add(30*time.Minute)); err != nil || !ok {
		t.Fatalf("setup ClaimForClose: ok=%v err=%v", ok, err)
	}

	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
			return nil, nil // broker no longer has it
		},
		GetExecutionsFn: func(_ context.Context, orderID string) ([]order.Execution, error) {
			if orderID == "tp-c1" {
				return []order.Execution{{Price: 150.30, Quantity: 100, Timestamp: closedAt}}, nil
			}
			return nil, nil
		},
	}

	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode:     ReconcileModeRuntime,
		LiveMode: config.ModeLiveConfig,
		Closer:   backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		Clock:    func() time.Time { return closedAt },
	}
	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Resolved != 1 {
		t.Errorf("Resolved: got %d want 1 (CLOSING row must be resolved via TP leg)", sum.Resolved)
	}
	if sum.Tripped != 0 {
		t.Errorf("Tripped: got %d want 0", sum.Tripped)
	}

	// Position should now be CLOSED, and a real trade recorded.
	open, _ := posRepo.ListOpenOrClosing(ctx, "USD_JPY")
	if len(open) != 0 {
		t.Errorf("CLOSING row should have been resolved to CLOSED; %d open/closing remain", len(open))
	}
	trades, _ := tradeRepo.ListSince(ctx, openedAt, 10)
	if len(trades) != 1 || trades[0].CloseReason != "take_profit" || trades[0].ExitPrice != 150.30 {
		t.Errorf("expected 1 take_profit trade @ 150.30; got %+v", trades)
	}
}
