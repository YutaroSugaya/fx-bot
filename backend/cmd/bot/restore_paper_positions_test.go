package main

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"fx-bot/backend/internal/adapter/broker"
	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/port"
)

// If restorePaperPositions used ListOpenOrClosing and restored every
// returned row to the PaperBroker as Status=OPEN, a row already in CLOSING
// (= a saga that crashed before CloseAndRecord) would end up living in BOTH
// places:
//   - DB:     status=CLOSING (ManageOpenPositions then skips it)
//   - Broker: OPEN (reconcile's stale-DB branch is skipped because the
//             broker still reports the position)
// Result: the position is stuck forever — no one finalises it.
//
// The fix is for restorePaperPositions to only re-hydrate OPEN rows.
// CLOSING stays only in the DB so the reconcile stale-DB branch finds
// it and finalises via CloseAndRecord (paper synthetic-close path).

func newRestoreTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestRestorePaperPositions_SkipsClosingRows(t *testing.T) {
	ctx := context.Background()
	posRepo := backtest.NewInMemoryPositionRepo()
	paper := broker.NewPaperBroker(broker.PaperBrokerConfig{
		Pricer: func(_ context.Context, _ string) (float64, float64, error) { return 100, 100, nil },
		Clock:  time.Now,
	})

	// Seed 1 OPEN and 1 CLOSING row with broker_position_id metadata so
	// restore can build a position with both.
	openID, err := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 100,
			TakeProfitPips: 10, StopLossPips: 10, MaxHoldMinutes: 60,
			StrategyConfigID: "cfg-1",
			Status:           port.PositionStatusOpen, OpenedAt: time.Now(),
		},
		Live: &port.PositionLive{BrokerPositionID: "paper-pos-1"},
	})
	if err != nil {
		t.Fatalf("insert open: %v", err)
	}
	closingID, err := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "SELL", Quantity: 100, EntryPrice: 100,
			TakeProfitPips: 10, StopLossPips: 10, MaxHoldMinutes: 60,
			StrategyConfigID: "cfg-2",
			Status:           port.PositionStatusOpen, OpenedAt: time.Now(),
		},
		Live: &port.PositionLive{BrokerPositionID: "paper-pos-2"},
	})
	if err != nil {
		t.Fatalf("insert (pre-claim): %v", err)
	}
	if _, err := posRepo.ClaimForClose(ctx, closingID, time.Now()); err != nil {
		t.Fatalf("claim closingID: %v", err)
	}

	restorePaperPositions(ctx, paper, posRepo, "USD_JPY", newRestoreTestLogger())

	got, err := paper.GetOpenPositions(ctx, "USD_JPY")
	if err != nil {
		t.Fatalf("GetOpenPositions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected only the OPEN row restored; got %d positions in broker", len(got))
	}
	if got[0].ID != openID {
		t.Errorf("restored the wrong position; got id=%d want %d (OPEN)", got[0].ID, openID)
	}
	// And the CLOSING row must NOT have been restored — verify via brokerID.
	for _, p := range got {
		if p.BrokerPositionID == "paper-pos-2" {
			t.Errorf("CLOSING row (paper-pos-2) leaked into broker; restore must skip CLOSING")
		}
	}
}
