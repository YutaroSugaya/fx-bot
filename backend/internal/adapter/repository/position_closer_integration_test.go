//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/port"
)

// CloseAndRecord は positions UPDATE + position_state_events + trades INSERT
// を 1 Tx で行う。
//
// The saga must transition OPEN → CLOSING via
// ClaimForClose BEFORE calling CloseAndRecord. CloseAndRecord now only
// flips CLOSING → CLOSED — so the integration test verifies:
//  1. happy path: Claim → CloseAndRecord → CLOSED + trade row exists
//  2. race detection: CloseAndRecord on a non-CLOSING row returns ok=false
//  3. atomicity: trade INSERT failure rolls back the position UPDATE
//     (position remains CLOSING; reconcile must finalise)
func TestPositionCloserRepo_CloseAndRecord_AtomicTx(t *testing.T) {
	pool := requireDB(t)
	truncateAll(t, pool)
	ctx := context.Background()

	posRepo := NewPositionRepo(pool)
	closer := NewPositionCloserRepo(pool)

	now := time.Now().UTC()
	const cfgID = "cfg-closer-it"

	// 1. happy path
	t.Run("happy path closes position and inserts trade", func(t *testing.T) {
		truncateAll(t, pool)
		seedStrategyConfig(t, pool, cfgID)
		id, err := posRepo.Insert(ctx, port.PositionInsertInput{
			Position: port.PositionRecord{
				Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 150.00,
				TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240,
				StrategyConfigID: cfgID, Status: port.PositionStatusOpen, OpenedAt: now,
			},
		})
		if err != nil {
			t.Fatalf("insert position: %v", err)
		}
		// Saga step 1: claim OPEN → CLOSING.
		claimed, cerr := posRepo.ClaimForClose(ctx, id, now)
		if cerr != nil || !claimed {
			t.Fatalf("claim_for_close ok=%v err=%v", claimed, cerr)
		}
		ok, err := closer.CloseAndRecord(ctx, id, now.Add(time.Minute), port.TradeRecord{
			PositionID: id, StrategyConfigID: cfgID,
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100,
			EntryPrice: 150.00, ExitPrice: 150.20, ProfitLossPips: 20, ProfitLossJPY: 20,
			CloseReason: "take_profit", OpenedAt: now, ClosedAt: now.Add(time.Minute),
		})
		if err != nil {
			t.Fatalf("CloseAndRecord: %v", err)
		}
		if !ok {
			t.Errorf("expected ok=true for claimed CLOSING position")
		}
		var status string
		if err := pool.QueryRow(ctx, "SELECT status FROM positions WHERE id=$1", id).Scan(&status); err != nil {
			t.Fatalf("verify position: %v", err)
		}
		if status != "CLOSED" {
			t.Errorf("position status: got %s want CLOSED", status)
		}
		var count int
		_ = pool.QueryRow(ctx, "SELECT COUNT(*) FROM trades WHERE position_id=$1", id).Scan(&count)
		if count != 1 {
			t.Errorf("expected 1 trade row; got %d", count)
		}
		// CLOSED state event must exist in the append-only ledger.
		var stateCount int
		_ = pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM position_state_events WHERE position_id=$1 AND state='CLOSED'", id,
		).Scan(&stateCount)
		if stateCount != 1 {
			t.Errorf("expected 1 CLOSED position_state_events row; got %d", stateCount)
		}
	})

	// 2. race detection: CloseAndRecord on a non-CLOSING row returns ok=false
	t.Run("non-closing row returns ok=false and skips trade insert", func(t *testing.T) {
		truncateAll(t, pool)
		seedStrategyConfig(t, pool, cfgID)
		id, _ := posRepo.Insert(ctx, port.PositionInsertInput{
			Position: port.PositionRecord{
				Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 150.00,
				StrategyConfigID: cfgID, Status: port.PositionStatusOpen, OpenedAt: now,
			},
		})
		// Skip the Claim step on purpose — simulates a buggy caller or a
		// parallel saga that already finalised the close.
		ok, err := closer.CloseAndRecord(ctx, id, now.Add(time.Minute), port.TradeRecord{
			PositionID: id, StrategyConfigID: cfgID,
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100,
			EntryPrice: 150.00, ExitPrice: 150.20,
			CloseReason: "manual", OpenedAt: now, ClosedAt: now.Add(time.Minute),
		})
		if err != nil {
			t.Fatalf("CloseAndRecord: %v", err)
		}
		if ok {
			t.Errorf("expected ok=false when not in CLOSING")
		}
		var count int
		_ = pool.QueryRow(ctx, "SELECT COUNT(*) FROM trades WHERE position_id=$1", id).Scan(&count)
		if count != 0 {
			t.Errorf("expected 0 trades when not claimed; got %d", count)
		}
	})

	// 3. atomicity: invalid trade (FK violation on position_id) → position
	// stays CLOSING and the CLOSED state event is rolled back too.
	t.Run("trade insert error rolls back position update", func(t *testing.T) {
		truncateAll(t, pool)
		seedStrategyConfig(t, pool, cfgID)
		id, _ := posRepo.Insert(ctx, port.PositionInsertInput{
			Position: port.PositionRecord{
				Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 150.00,
				StrategyConfigID: cfgID, Status: port.PositionStatusOpen, OpenedAt: now,
			},
		})
		if _, err := posRepo.ClaimForClose(ctx, id, now); err != nil {
			t.Fatalf("claim_for_close: %v", err)
		}
		// PositionID=999999 doesn't exist → FK violation on trades.position_id.
		_, err := closer.CloseAndRecord(ctx, id, now, port.TradeRecord{
			PositionID: 999999, StrategyConfigID: cfgID,
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100,
			EntryPrice: 150.00, ExitPrice: 150.10,
			CloseReason: "manual", OpenedAt: now, ClosedAt: now,
		})
		if err == nil {
			t.Fatal("expected error from FK violation")
		}
		// position must remain CLOSING (Tx rolled back); reconcile handles it.
		var status string
		_ = pool.QueryRow(ctx, "SELECT status FROM positions WHERE id=$1", id).Scan(&status)
		if status != "CLOSING" {
			t.Errorf("position status after rollback: got %s want CLOSING", status)
		}
		// No CLOSED state event should be present.
		var stateCount int
		_ = pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM position_state_events WHERE position_id=$1 AND state='CLOSED'", id,
		).Scan(&stateCount)
		if stateCount != 0 {
			t.Errorf("expected 0 CLOSED state events after rollback; got %d", stateCount)
		}
	})
}
