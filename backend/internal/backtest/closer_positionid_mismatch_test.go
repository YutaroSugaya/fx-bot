package backtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"fx-bot/backend/internal/port"
)

// CloseAndRecord uses the function arg `positionID` to mark a row CLOSED,
// but the embedded trade.PositionID to write the trade row. When those two
// diverge the FK still resolves to a real (different) position, so the
// ledger and the closed row would get out of sync silently. Both implementations
// (production PositionCloserRepo + InMemoryPositionCloser) must
// refuse the call instead of papering over the mismatch.

func TestInMemoryCloseAndRecord_RejectsPositionIDMismatch(t *testing.T) {
	posRepo := NewInMemoryPositionRepo()
	tradeRepo := NewInMemoryTradeRepo()
	closer := &InMemoryPositionCloser{Positions: posRepo, Trades: tradeRepo}
	ctx := context.Background()

	// Seed two positions, claim both to CLOSING so the only thing that
	// can fail is the mismatch guard.
	idA, _ := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 150,
			StrategyConfigID: "cfg-A",
			Status:           port.PositionStatusOpen, OpenedAt: time.Now(),
		},
	})
	idB, _ := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 150,
			StrategyConfigID: "cfg-B",
			Status:           port.PositionStatusOpen, OpenedAt: time.Now(),
		},
	})
	if _, err := posRepo.ClaimForClose(ctx, idA, time.Now()); err != nil {
		t.Fatalf("claim a: %v", err)
	}

	trade := port.TradeRecord{
		PositionID:       idB, // ← different from the close target
		StrategyConfigID: "cfg-B",
		Symbol:           "USD_JPY",
		Side:             "BUY",
		Quantity:         100,
		EntryPrice:       150,
		ExitPrice:        150.1,
		ProfitLossPips:   10,
		ProfitLossJPY:    1000,
		CloseReason:      "manual",
		OpenedAt:         time.Now().Add(-time.Hour),
		ClosedAt:         time.Now(),
	}
	ok, err := closer.CloseAndRecord(ctx, idA, time.Now(), trade)
	if err == nil {
		t.Fatalf("expected error on positionID mismatch; got ok=%v", ok)
	}
	if ok {
		t.Errorf("ok must be false when mismatch rejected")
	}
	if !strings.Contains(err.Error(), "position_id mismatch") {
		t.Errorf("expected 'position_id mismatch' in error; got %v", err)
	}
	// Ensure no trade was written and the position remained CLOSING (not corrupted).
	if tradeCount := len(tradeRepo.rows); tradeCount != 0 {
		t.Errorf("no trade should have been inserted; got %d rows", tradeCount)
	}
	if posRepo.rows[0].Status != port.PositionStatusClosing {
		t.Errorf("position A status: got %q want CLOSING (mismatch must not flip it)", posRepo.rows[0].Status)
	}
}

func TestInMemoryCloseAndRecord_AcceptsWhenPositionIDsMatch(t *testing.T) {
	posRepo := NewInMemoryPositionRepo()
	tradeRepo := NewInMemoryTradeRepo()
	closer := &InMemoryPositionCloser{Positions: posRepo, Trades: tradeRepo}
	ctx := context.Background()

	pid, _ := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 150,
			StrategyConfigID: "cfg-A",
			Status:           port.PositionStatusOpen, OpenedAt: time.Now(),
		},
	})
	if _, err := posRepo.ClaimForClose(ctx, pid, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}

	trade := port.TradeRecord{
		PositionID:       pid,
		StrategyConfigID: "cfg-A",
		Symbol:           "USD_JPY",
		Side:             "BUY",
		Quantity:         100,
		EntryPrice:       150,
		ExitPrice:        150.1,
		ProfitLossPips:   10,
		ProfitLossJPY:    1000,
		CloseReason:      "manual",
		OpenedAt:         time.Now().Add(-time.Hour),
		ClosedAt:         time.Now(),
	}
	ok, err := closer.CloseAndRecord(ctx, pid, time.Now(), trade)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Errorf("ok=false when ids match should not happen")
	}
}

// Sanity that the closer rejects the obvious zero-id case (TradeRecord
// left default) — guard catches programmer errors too.
func TestInMemoryCloseAndRecord_RejectsZeroTradePositionID(t *testing.T) {
	posRepo := NewInMemoryPositionRepo()
	tradeRepo := NewInMemoryTradeRepo()
	closer := &InMemoryPositionCloser{Positions: posRepo, Trades: tradeRepo}
	ctx := context.Background()

	pid, _ := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 150,
			StrategyConfigID: "cfg-A",
			Status:           port.PositionStatusOpen, OpenedAt: time.Now(),
		},
	})
	if _, err := posRepo.ClaimForClose(ctx, pid, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}

	trade := port.TradeRecord{ // PositionID left 0
		Symbol: "USD_JPY", Side: "BUY", Quantity: 100,
		EntryPrice: 150, ExitPrice: 150.1, ProfitLossPips: 10, ProfitLossJPY: 1000,
		CloseReason: "manual",
		OpenedAt:    time.Now().Add(-time.Hour), ClosedAt: time.Now(),
	}
	_, err := closer.CloseAndRecord(ctx, pid, time.Now(), trade)
	if err == nil {
		t.Fatalf("expected mismatch error when trade.PositionID is zero")
	}
	if !errors.Is(err, port.ErrTradePositionIDMismatch) && !strings.Contains(err.Error(), "position_id mismatch") {
		t.Errorf("expected mismatch sentinel/string; got %v", err)
	}
}
