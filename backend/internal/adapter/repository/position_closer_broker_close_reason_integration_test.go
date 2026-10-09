//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/port"
)

// reconcile の findFillByPositionLookup は GMO 実約定から close を復元し、exit が
// TP/SL 理論価格に一致しない場合 classifyCloseReason が close_reason="broker_close"
// を返す (reconcile.go:526)。この値が trades の CHECK 制約に無いと
// CloseAndRecord が CHECK 違反で失敗 → resolveAndRecordClose が synthetic 0-PnL
// (reconcile_cold_close) に倒れ、実損益が帳簿に乗らない。
//
// migration 0006 で CHECK を拡張済み。このテストはその回帰防止
// (0004=ratchet / 0005=early_exit と同じ不具合クラス)。
func TestPositionCloserRepo_CloseAndRecord_BrokerClose(t *testing.T) {
	pool := requireDB(t)
	truncateAll(t, pool)
	ctx := context.Background()

	posRepo := NewPositionRepo(pool)
	closer := NewPositionCloserRepo(pool)

	now := time.Now().UTC()
	const cfgID = "cfg-broker-close-reason-it"

	seedStrategyConfig(t, pool, cfgID)
	id, err := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "EUR_USD", Side: "BUY", Quantity: 1000, EntryPrice: 1.1533,
			TakeProfitPips: 12, StopLossPips: 6, MaxHoldMinutes: 90,
			StrategyConfigID: cfgID, Status: port.PositionStatusOpen, OpenedAt: now,
		},
	})
	if err != nil {
		t.Fatalf("insert position: %v", err)
	}
	if _, err := posRepo.ClaimForClose(ctx, id, now); err != nil {
		t.Fatalf("claim_for_close: %v", err)
	}
	// EUR_USD closed at -3 pips (not matching TP +12 / SL -6) → "broker_close".
	ok, err := closer.CloseAndRecord(ctx, id, now.Add(time.Minute), port.TradeRecord{
		PositionID: id, StrategyConfigID: cfgID,
		Symbol: "EUR_USD", Side: "BUY", Quantity: 1000,
		EntryPrice: 1.1533, ExitPrice: 1.1530,
		ProfitLossPips: -3.0, ProfitLossJPY: -48.09,
		CloseReason: "broker_close",
		OpenedAt:    now, ClosedAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("CloseAndRecord with broker_close: %v", err)
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
	if reason != "broker_close" {
		t.Errorf("close_reason: got %q want broker_close", reason)
	}
}
