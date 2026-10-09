//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/port"
)

// Ratchet TP (trailing take-profit) は usecase 側で close_reason="ratchet_takeprofit"
// を返す (manage_open_positions_exits.go の ratchet 判定) が、当初 schema (0001_init.up.sql)
// の CHECK 制約には含まれていなかった。このため本番で ratchet TP が発火すると
// trades INSERT が CHECK 違反で失敗し close saga が止まる。
//
// migration 0004 で CHECK を拡張済み。このテストはその回帰防止。
func TestPositionCloserRepo_CloseAndRecord_RatchetTakeProfit(t *testing.T) {
	pool := requireDB(t)
	truncateAll(t, pool)
	ctx := context.Background()

	posRepo := NewPositionRepo(pool)
	closer := NewPositionCloserRepo(pool)

	now := time.Now().UTC()
	const cfgID = "cfg-ratchet-close-reason-it"

	seedStrategyConfig(t, pool, cfgID)
	id, err := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 150.00,
			TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240,
			RatchetArmPips: 5, RatchetGivebackPips: 3,
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
		EntryPrice: 150.00, ExitPrice: 150.07,
		ProfitLossPips: 7, ProfitLossJPY: 7,
		CloseReason: "ratchet_takeprofit",
		OpenedAt:    now, ClosedAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("CloseAndRecord with ratchet_takeprofit: %v", err)
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
	if reason != "ratchet_takeprofit" {
		t.Errorf("close_reason: got %q want ratchet_takeprofit", reason)
	}
}
