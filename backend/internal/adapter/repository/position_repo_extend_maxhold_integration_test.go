//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/port"
)

// ExtendMaxHold は UI/API の「延長ボタン」が叩く経路。max_hold_minutes に
// addMinutes を加算し、新しい合計 + opened_at を返す。status='OPEN' の row
// のみ対象で、CLOSING/CLOSED/未知 id は nil (no-op) を返す。
//
// bot の OnTick は毎 tick で max_hold_minutes を読み直すので、この UPDATE 後
// の次 tick から新しい soft deadline が効く (build/close 不要)。

func TestPositionRepo_ExtendMaxHold_RoundTrip(t *testing.T) {
	pool := requireDB(t)
	truncateAll(t, pool)
	seedStrategyConfig(t, pool, "cfg-ext-1")

	ctx := context.Background()
	repo := NewPositionRepo(pool)

	openedAt := time.Now().UTC().Add(-100 * time.Minute).Truncate(time.Microsecond)
	id, err := repo.Insert(ctx, port.PositionInsertInput{Position: port.PositionRecord{
		Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 159.996,
		TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240,
		StrategyConfigID: "cfg-ext-1", Status: port.PositionStatusOpen, OpenedAt: openedAt,
	}})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	ext, err := repo.ExtendMaxHold(ctx, id, 240)
	if err != nil {
		t.Fatalf("extend: %v", err)
	}
	if ext == nil {
		t.Fatalf("extend returned nil for an OPEN position")
	}
	if ext.MaxHoldMinutes != 480 {
		t.Errorf("MaxHoldMinutes: got %d want 480", ext.MaxHoldMinutes)
	}
	if !ext.OpenedAt.Equal(openedAt) {
		t.Errorf("OpenedAt: got %v want %v", ext.OpenedAt, openedAt)
	}

	// 再 List で永続化を確認。
	got, err := repo.ListOpenOrClosing(ctx, "USD_JPY")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].MaxHoldMinutes != 480 {
		t.Errorf("persisted max_hold: got %+v want 480", got)
	}

	// 2 回目: さらに +60 で累積加算 (540) になること。
	ext2, err := repo.ExtendMaxHold(ctx, id, 60)
	if err != nil {
		t.Fatalf("second extend: %v", err)
	}
	if ext2 == nil || ext2.MaxHoldMinutes != 540 {
		t.Errorf("second extend: got %+v want 540", ext2)
	}
}

func TestPositionRepo_ExtendMaxHold_NotOpenIsNil(t *testing.T) {
	pool := requireDB(t)
	truncateAll(t, pool)
	seedStrategyConfig(t, pool, "cfg-ext-2")

	ctx := context.Background()
	repo := NewPositionRepo(pool)

	// 未知 id → nil。
	if ext, err := repo.ExtendMaxHold(ctx, 424242, 60); err != nil || ext != nil {
		t.Errorf("unknown id: got (%+v, %v) want (nil, nil)", ext, err)
	}

	// CLOSING の position → nil (no-op)。
	now := time.Now().UTC()
	id, err := repo.Insert(ctx, port.PositionInsertInput{Position: port.PositionRecord{
		Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 159.0,
		TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240,
		StrategyConfigID: "cfg-ext-2", Status: port.PositionStatusOpen, OpenedAt: now,
	}})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if ok, err := repo.ClaimForClose(ctx, id, now); err != nil || !ok {
		t.Fatalf("claim for close: ok=%v err=%v", ok, err)
	}
	if ext, err := repo.ExtendMaxHold(ctx, id, 60); err != nil || ext != nil {
		t.Errorf("closing position: got (%+v, %v) want (nil, nil)", ext, err)
	}
}
