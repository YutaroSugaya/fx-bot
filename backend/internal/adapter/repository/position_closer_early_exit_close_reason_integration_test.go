//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/port"
)

// Early-exit window は usecase 側で close_reason="early_exit" を返す
// ([manage_open_positions_exits.go] の evaluateMaxHoldExit early-exit 分岐、
// "max_hold" とは区別される) が、CHECK 制約 (0001_init.up.sql:276,
// 0004 まで) には含まれていない。このままだと本番で early-exit が発火したとき
// trades INSERT が CHECK 違反で失敗し close saga が止まる (0003→0004 と同じ不具合クラス)。
//
// migration 0005 で CHECK を拡張済み。このテストはその回帰防止。
func TestPositionCloserRepo_CloseAndRecord_EarlyExit(t *testing.T) {
	pool := requireDB(t)
	truncateAll(t, pool)
	ctx := context.Background()

	posRepo := NewPositionRepo(pool)
	closer := NewPositionCloserRepo(pool)

	now := time.Now().UTC()
	const cfgID = "cfg-early-exit-close-reason-it"

	seedStrategyConfig(t, pool, cfgID)
	id, err := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 150.00,
			TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240,
			EarlyExitWindowMinutes: 30, EarlyExitTargetPips: -2,
			StrategyConfigID: cfgID, Status: port.PositionStatusOpen, OpenedAt: now,
		},
	})
	if err != nil {
		t.Fatalf("insert position: %v", err)
	}
	if _, err := posRepo.ClaimForClose(ctx, id, now); err != nil {
		t.Fatalf("claim_for_close: %v", err)
	}
	ok, err := closer.CloseAndRecord(ctx, id, now.Add(time.Minute), port.TradeRecord{
		PositionID: id, StrategyConfigID: cfgID,
		Symbol: "USD_JPY", Side: "BUY", Quantity: 100,
		EntryPrice: 150.00, ExitPrice: 149.985,
		ProfitLossPips: -1.5, ProfitLossJPY: -1.5,
		CloseReason: "early_exit",
		OpenedAt:    now, ClosedAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("CloseAndRecord with early_exit: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true for claimed CLOSING position")
	}
	var reason string
	if err := pool.QueryRow(ctx,
		"SELECT close_reason FROM trades WHERE position_id=$1", id,
	).Scan(&reason); err != nil {
		t.Fatalf("verify trade: %v", err)
	}
	if reason != "early_exit" {
		t.Errorf("close_reason: got %q want early_exit", reason)
	}
}
