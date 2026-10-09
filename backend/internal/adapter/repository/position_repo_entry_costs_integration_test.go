//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/port"
)

// Migration 0008 introduces 3 nullable columns on
// positions for entry-time cost capture:
//   - entry_fee_jpy       (broker 実報告手数料。NULL=未捕捉 / 0=実報告ゼロを区別)
//   - entry_spread_pips   (発注直前 ticker の実測スプレッド)
//   - entry_slippage_pips (符号付き adverse slippage)
//
// この test は Insert→ListOpenOrClosing の round-trip で sqlc / SQL 列順
// mismatch と NULL ↔ *float64 マッピングを早期に潰す (0002/0003 と同パターン)。

func TestPositionRepo_EntryCostsRoundTrip(t *testing.T) {
	pool := requireDB(t)
	truncateAll(t, pool)
	seedStrategyConfig(t, pool, "cfg-ec-1")

	ctx := context.Background()
	repo := NewPositionRepo(pool)

	fee, spread, slip := 3.7, 1.0, 0.5
	now := time.Now().UTC()
	want := port.PositionRecord{
		Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 150.135,
		TakeProfitPips: 30, StopLossPips: 12, MaxHoldMinutes: 480,
		EntryFeeJPY:       &fee,
		EntrySpreadPips:   &spread,
		EntrySlippagePips: &slip,
		StrategyConfigID:  "cfg-ec-1",
		Status:            port.PositionStatusOpen,
		OpenedAt:          now,
	}
	if _, err := repo.Insert(ctx, port.PositionInsertInput{Position: want}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := repo.ListOpenOrClosing(ctx, "USD_JPY")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 row, got %d", len(got))
	}
	if got[0].EntryFeeJPY == nil || *got[0].EntryFeeJPY != fee {
		t.Errorf("EntryFeeJPY round-trip: got %v want %v", got[0].EntryFeeJPY, fee)
	}
	if got[0].EntrySpreadPips == nil || *got[0].EntrySpreadPips != spread {
		t.Errorf("EntrySpreadPips round-trip: got %v want %v", got[0].EntrySpreadPips, spread)
	}
	if got[0].EntrySlippagePips == nil || *got[0].EntrySlippagePips != slip {
		t.Errorf("EntrySlippagePips round-trip: got %v want %v", got[0].EntrySlippagePips, slip)
	}
}

// 未捕捉 (nil) は NULL で永続化され、読み戻しでも nil のまま — 0 と混同しない
// (NULL=未捕捉 / 0=broker 実報告ゼロ の区別が fee_estimated 判定の前提)。
func TestPositionRepo_EntryCostsNullStaysNull(t *testing.T) {
	pool := requireDB(t)
	truncateAll(t, pool)
	seedStrategyConfig(t, pool, "cfg-ec-2")

	ctx := context.Background()
	repo := NewPositionRepo(pool)

	now := time.Now().UTC()
	rec := port.PositionRecord{
		Symbol: "USD_JPY", Side: "SELL", Quantity: 1000, EntryPrice: 150.0,
		TakeProfitPips: 20, StopLossPips: 10, MaxHoldMinutes: 240,
		// EntryFeeJPY / EntrySpreadPips / EntrySlippagePips は意図的に nil。
		StrategyConfigID: "cfg-ec-2",
		Status:           port.PositionStatusOpen,
		OpenedAt:         now,
	}
	if _, err := repo.Insert(ctx, port.PositionInsertInput{Position: rec}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := repo.ListOpenOrClosing(ctx, "USD_JPY")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 row, got %d", len(got))
	}
	if got[0].EntryFeeJPY != nil || got[0].EntrySpreadPips != nil || got[0].EntrySlippagePips != nil {
		t.Errorf("nil costs must stay NULL/nil: fee=%v spread=%v slip=%v",
			got[0].EntryFeeJPY, got[0].EntrySpreadPips, got[0].EntrySlippagePips)
	}
}

// Migration 0007 列 (trades.fee_jpy / swap_jpy / fee_estimated) の書込→読戻し。
// CloseAndRecord (本番 close 経路の Tx) で書き、ListClosedBySymbolSince (edge
// metrics の読出経路) で読み戻す — コスト永続化契約の end-to-end pin。
func TestPositionCloserRepo_CloseAndRecord_PersistsTradeCosts(t *testing.T) {
	pool := requireDB(t)
	truncateAll(t, pool)
	seedStrategyConfig(t, pool, "cfg-ec-3")

	ctx := context.Background()
	posRepo := NewPositionRepo(pool)
	closer := NewPositionCloserRepo(pool)
	trades := NewTradeRepo(pool)

	now := time.Now().UTC()
	id, err := posRepo.Insert(ctx, port.PositionInsertInput{Position: port.PositionRecord{
		Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 150.00,
		TakeProfitPips: 30, StopLossPips: 12, MaxHoldMinutes: 480,
		StrategyConfigID: "cfg-ec-3", Status: port.PositionStatusOpen, OpenedAt: now,
	}})
	if err != nil {
		t.Fatalf("insert position: %v", err)
	}
	if claimed, cerr := posRepo.ClaimForClose(ctx, id, now); cerr != nil || !claimed {
		t.Fatalf("claim: ok=%v err=%v", claimed, cerr)
	}

	ok, err := closer.CloseAndRecord(ctx, id, now.Add(time.Hour), port.TradeRecord{
		PositionID: id, StrategyConfigID: "cfg-ec-3",
		Symbol: "USD_JPY", Side: "BUY", Quantity: 1000,
		EntryPrice: 150.00, ExitPrice: 150.50, ProfitLossPips: 50, ProfitLossJPY: 500,
		CloseReason:  "take_profit",
		FeeJPY:       7.5,
		SwapJPY:      -12.0,
		FeeEstimated: true,
		OpenedAt:     now, ClosedAt: now.Add(time.Hour),
	})
	if err != nil || !ok {
		t.Fatalf("CloseAndRecord: ok=%v err=%v", ok, err)
	}

	got, err := trades.ListClosedBySymbolSince(ctx, "USD_JPY", now, 10)
	if err != nil {
		t.Fatalf("list trades: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 trade, got %d", len(got))
	}
	tr := got[0]
	if tr.FeeJPY != 7.5 {
		t.Errorf("FeeJPY round-trip: got %v want 7.5", tr.FeeJPY)
	}
	if tr.SwapJPY != -12.0 {
		t.Errorf("SwapJPY round-trip: got %v want -12.0", tr.SwapJPY)
	}
	if !tr.FeeEstimated {
		t.Errorf("FeeEstimated round-trip: got false want true")
	}
	// gross 不変条件: profit_loss_jpy はコストを混ぜない。
	if tr.ProfitLossJPY != 500 {
		t.Errorf("ProfitLossJPY must stay GROSS: got %v want 500", tr.ProfitLossJPY)
	}
}
