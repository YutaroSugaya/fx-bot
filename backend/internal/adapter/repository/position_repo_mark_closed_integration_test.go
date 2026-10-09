//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/port"
)

// PositionRepo.MarkClosed must not insert a position_state_events('CLOSED')
// row when the UPDATE affects 0 rows (= row was already CLOSED), matching
// InMemoryPositionRepo.MarkClosed, which is a no-op for already-closed
// rows. This integration test pins that behaviour: a repeat call
// must NOT add another CLOSED state event.

func TestPositionRepoMarkClosed_IdempotentOnAlreadyClosed(t *testing.T) {
	pool := requireDB(t)
	truncateAll(t, pool)
	seedStrategyConfig(t, pool, "cfg-mc-1")

	ctx := context.Background()
	repo := NewPositionRepo(pool)

	now := time.Now().UTC()
	id, err := repo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 150.0,
			TakeProfitPips: 10, StopLossPips: 10, MaxHoldMinutes: 60,
			StrategyConfigID: "cfg-mc-1",
			Status:           port.PositionStatusOpen, OpenedAt: now.Add(-time.Hour),
		},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := repo.MarkClosed(ctx, id, now); err != nil {
		t.Fatalf("first MarkClosed: %v", err)
	}
	first := countClosedStateEvents(ctx, t, pool, id)
	if first != 1 {
		t.Fatalf("after first MarkClosed: state_events(CLOSED) = %d want 1", first)
	}

	// Second call on the same already-CLOSED row must NOT add another event.
	if err := repo.MarkClosed(ctx, id, now); err != nil {
		t.Fatalf("second MarkClosed: %v", err)
	}
	second := countClosedStateEvents(ctx, t, pool, id)
	if second != 1 {
		t.Errorf("after second MarkClosed: state_events(CLOSED) = %d want 1 (must be idempotent)", second)
	}
}

func countClosedStateEvents(ctx context.Context, t *testing.T, pool *pgxpool.Pool, positionID int64) int {
	t.Helper()
	var n int
	err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM position_state_events WHERE position_id=$1 AND state='CLOSED'",
		positionID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("count state events: %v", err)
	}
	return n
}
