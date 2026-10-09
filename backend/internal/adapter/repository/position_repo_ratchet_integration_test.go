//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/port"
)

// Migration 0003 adds 4 columns to positions for ratchet TP:
//   - ratchet_arm_pips, ratchet_giveback_pips (snapshot from active config)
//   - peak_unrealized_pips, ratchet_armed     (runtime state, OnTick で更新)
//
// この test は Insert→ListOpenOrClosing で snapshot を、UpdateRatchetState→
// 再 List で runtime state を round-trip させて sqlc / SQL 列順 mismatch を
// 早期に潰す。

func TestPositionRepo_RatchetSnapshotRoundTrip(t *testing.T) {
	pool := requireDB(t)
	truncateAll(t, pool)
	seedStrategyConfig(t, pool, "cfg-rt-1")

	ctx := context.Background()
	repo := NewPositionRepo(pool)

	now := time.Now().UTC()
	want := port.PositionRecord{
		Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 158.918,
		TakeProfitPips: 30, StopLossPips: 18, MaxHoldMinutes: 270,
		// ratchet snapshot — Insert 時に config から凍結保存される
		RatchetArmPips:      5.0,
		RatchetGivebackPips: 3.0,
		StrategyConfigID:    "cfg-rt-1",
		Status:              port.PositionStatusOpen,
		OpenedAt:            now,
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
	if got[0].RatchetArmPips != want.RatchetArmPips {
		t.Errorf("RatchetArmPips round-trip: got %v want %v", got[0].RatchetArmPips, want.RatchetArmPips)
	}
	if got[0].RatchetGivebackPips != want.RatchetGivebackPips {
		t.Errorf("RatchetGivebackPips round-trip: got %v want %v", got[0].RatchetGivebackPips, want.RatchetGivebackPips)
	}
	// runtime state は新規 Insert 直後は DEFAULT (0.0 / false)
	if got[0].PeakUnrealizedPips != 0 {
		t.Errorf("PeakUnrealizedPips fresh insert: got %v want 0", got[0].PeakUnrealizedPips)
	}
	if got[0].RatchetArmed {
		t.Errorf("RatchetArmed fresh insert: got true want false")
	}
}

// 既存 config (ratchet フィールド無し) で開いた position は ratchet snapshot
// が 0 / 0 (= OFF) で記録されること。後方互換ガード。
func TestPositionRepo_RatchetDefaultsToZero(t *testing.T) {
	pool := requireDB(t)
	truncateAll(t, pool)
	seedStrategyConfig(t, pool, "cfg-rt-2")

	ctx := context.Background()
	repo := NewPositionRepo(pool)

	now := time.Now().UTC()
	rec := port.PositionRecord{
		Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 159.0,
		TakeProfitPips: 25, StopLossPips: 16, MaxHoldMinutes: 240,
		// Intentionally NO Ratchet fields set — relying on Go zero values.
		StrategyConfigID: "cfg-rt-2",
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
	if got[0].RatchetArmPips != 0 || got[0].RatchetGivebackPips != 0 {
		t.Errorf("expected DEFAULT 0/0, got arm=%v give=%v",
			got[0].RatchetArmPips, got[0].RatchetGivebackPips)
	}
}

// OnTick の peak 更新 / armed フラグ立ては UpdateRatchetState で行い、
// position status='OPEN' の row のみ更新する。CLOSING / CLOSED な position
// に対する更新は 0 rows affected で no-op。
func TestPositionRepo_RatchetStateUpdate(t *testing.T) {
	pool := requireDB(t)
	truncateAll(t, pool)
	seedStrategyConfig(t, pool, "cfg-rt-3")

	ctx := context.Background()
	repo := NewPositionRepo(pool)

	now := time.Now().UTC()
	id, err := repo.Insert(ctx, port.PositionInsertInput{Position: port.PositionRecord{
		Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 158.918,
		TakeProfitPips: 30, StopLossPips: 18, MaxHoldMinutes: 270,
		RatchetArmPips: 5, RatchetGivebackPips: 3,
		StrategyConfigID: "cfg-rt-3",
		Status:           port.PositionStatusOpen,
		OpenedAt:         now,
	}})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// 1 回目 update: peak を 11.5 に、armed を true に (損切り側はまだ 0/false)
	if err := repo.UpdateRatchetState(ctx, id, 11.5, true, 0, false); err != nil {
		t.Fatalf("first update: %v", err)
	}
	got, err := repo.ListOpenOrClosing(ctx, "USD_JPY")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got[0].PeakUnrealizedPips != 11.5 {
		t.Errorf("peak after 1st update: got %v want 11.5", got[0].PeakUnrealizedPips)
	}
	if !got[0].RatchetArmed {
		t.Errorf("armed after 1st update: got false want true")
	}

	// 2 回目 update: peak を 15.0 に + 損切り側 trough を -16.0 / loss_armed を true に
	if err := repo.UpdateRatchetState(ctx, id, 15.0, true, -16.0, true); err != nil {
		t.Fatalf("second update: %v", err)
	}
	got, err = repo.ListOpenOrClosing(ctx, "USD_JPY")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got[0].PeakUnrealizedPips != 15.0 {
		t.Errorf("peak after 2nd update: got %v want 15.0", got[0].PeakUnrealizedPips)
	}
	if got[0].TroughUnrealizedPips != -16.0 {
		t.Errorf("trough after 2nd update: got %v want -16.0", got[0].TroughUnrealizedPips)
	}
	if !got[0].LossRatchetArmed {
		t.Errorf("loss_armed after 2nd update: got false want true")
	}
}
