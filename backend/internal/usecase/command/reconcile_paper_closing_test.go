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
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
)

// A Paper-mode close saga that crashed mid-flight leaves
// a row in CLOSING. On startup, ListOpenOrClosing returns it; ClaimForClose
// only accepts OPEN, so the previous reconcile code silently continued and
// the CLOSING row stayed forever. The fix: in paper startup branch, skip
// the claim for CLOSING and call CloseAndRecord directly.
func TestReconcile_PaperStartup_StaleClosingRow_SyntheticCloses(t *testing.T) {
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
			StrategyConfigID: "cfg-paper", Status: port.PositionStatusOpen, OpenedAt: openedAt,
		},
		Live: &port.PositionLive{BrokerPositionID: "broker-paper-stuck"},
	})
	// Simulate a crashed close saga: row got into CLOSING but never finalised.
	if ok, err := posRepo.ClaimForClose(ctx, id, openedAt.Add(30*time.Minute)); err != nil || !ok {
		t.Fatalf("setup ClaimForClose: ok=%v err=%v", ok, err)
	}

	// Broker says the position is gone.
	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) { return nil, nil },
	}

	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode:     ReconcileModeStartup,
		LiveMode: config.ModePaperConfig,
		Closer:   backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		Clock:    func() time.Time { return closedAt },
	}
	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.MarkedDone != 1 {
		t.Errorf("MarkedDone = %d, want 1 (stuck CLOSING row must be synthetic-closed)", sum.MarkedDone)
	}
	if sum.Tripped != 0 {
		t.Errorf("Tripped = %d, want 0 (no trip on successful close)", sum.Tripped)
	}

	// Verify the row is now CLOSED.
	listed, _ := posRepo.ListOpenOrClosing(ctx, "USD_JPY")
	if len(listed) != 0 {
		t.Errorf("position should be CLOSED after reconcile; ListOpenOrClosing returned %d rows", len(listed))
	}
	trades, _ := tradeRepo.ListSince(ctx, openedAt, 10)
	if len(trades) != 1 {
		t.Fatalf("expected 1 synthetic trade row, got %d", len(trades))
	}
	if trades[0].CloseReason != "reconcile_cold_close" {
		t.Errorf("CloseReason: got %q want reconcile_cold_close", trades[0].CloseReason)
	}

	if _, statErr := os.Stat(flagPath); statErr == nil {
		t.Error("emergency_stop.flag should NOT exist on successful synthetic close")
	}
}
