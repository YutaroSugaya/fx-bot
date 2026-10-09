//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/port"
)

// Migration 0002 added early_exit_window_minutes and early_exit_target_pips
// to the positions table. This integration test pins the round-trip
// (Insert → ListOpenOrClosing) so a future sqlc regen that accidentally
// drops the columns from the generated Insert/Select fails loudly here
// instead of silently disabling the feature in production.
//
// Why integration: dbgen types and the SQL column order are only proven
// to match by an actual Postgres write+read. Unit tests with the in-memory
// repo can't catch a sqlc/SQL mismatch.

func TestPositionRepo_EarlyExitRoundTrip(t *testing.T) {
	pool := requireDB(t)
	truncateAll(t, pool)
	seedStrategyConfig(t, pool, "cfg-ee-1")

	ctx := context.Background()
	repo := NewPositionRepo(pool)

	now := time.Now().UTC()
	want := port.PositionRecord{
		Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 159.138,
		TakeProfitPips: 25, StopLossPips: 16, MaxHoldMinutes: 270,
		// Non-zero values for both new fields. Pick numbers that wouldn't
		// match any other field's value so a column-shuffle mistake in
		// SQL/sqlc would surface as an obvious type/value mismatch.
		EarlyExitWindowMinutes: 30,
		EarlyExitTargetPips:    -2.0,
		StrategyConfigID:       "cfg-ee-1",
		Status:                 port.PositionStatusOpen,
		OpenedAt:               now,
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
	if got[0].EarlyExitWindowMinutes != want.EarlyExitWindowMinutes {
		t.Errorf("EarlyExitWindowMinutes round-trip: got %d want %d",
			got[0].EarlyExitWindowMinutes, want.EarlyExitWindowMinutes)
	}
	if got[0].EarlyExitTargetPips != want.EarlyExitTargetPips {
		t.Errorf("EarlyExitTargetPips round-trip: got %v want %v",
			got[0].EarlyExitTargetPips, want.EarlyExitTargetPips)
	}
}

// Back-compat sanity: a position inserted without setting the new fields
// must read back with the schema's DEFAULT 0 — i.e. feature OFF — so old
// PositionRecord literals (and old production code paths) keep working.
func TestPositionRepo_EarlyExitDefaultsToZero(t *testing.T) {
	pool := requireDB(t)
	truncateAll(t, pool)
	seedStrategyConfig(t, pool, "cfg-ee-2")

	ctx := context.Background()
	repo := NewPositionRepo(pool)

	now := time.Now().UTC()
	rec := port.PositionRecord{
		Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 159.0,
		TakeProfitPips: 25, StopLossPips: 16, MaxHoldMinutes: 240,
		// Intentionally NO EarlyExit fields set — relying on Go zero values.
		StrategyConfigID: "cfg-ee-2",
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
	if got[0].EarlyExitWindowMinutes != 0 {
		t.Errorf("expected DEFAULT 0 for EarlyExitWindowMinutes, got %d",
			got[0].EarlyExitWindowMinutes)
	}
	if got[0].EarlyExitTargetPips != 0 {
		t.Errorf("expected DEFAULT 0 for EarlyExitTargetPips, got %v",
			got[0].EarlyExitTargetPips)
	}
}
