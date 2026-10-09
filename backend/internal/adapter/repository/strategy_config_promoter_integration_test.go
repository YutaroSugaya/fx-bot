//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
)

// PromoteActive は (prev MarkExpired) + (new Insert) を 1 Tx で行う。
// 検証ポイント:
//  1. happy path: prev が expired、new が active になる
//  2. 23505 unique violation 時に Tx ロールバック → prev は active のまま
//  3. 既存 active が無くても (prevConfigID="") promote できる
func TestStrategyConfigPromoterRepo_PromoteActive_AtomicTx(t *testing.T) {
	pool := requireDB(t)
	truncateAll(t, pool)
	ctx := context.Background()

	scRepo := NewStrategyConfigRepo(pool)
	promoter := NewStrategyConfigPromoter(pool)

	now := time.Now().UTC()
	mkRec := func(id, status string) port.StrategyConfigRecord {
		return port.StrategyConfigRecord{
			ConfigID: id, Source: "auto", Mode: string(config.ModePaperConfig), Symbol: "USD_JPY",
			Enabled: true, StrategyName: "momentum_pullback",
			MarketRegimeType: "trend_up", MarketRegimeConfidence: 0.7,
			ValidFrom: now, ValidUntil: now.Add(time.Hour),
			RawYAML: "config_id: " + id + "\n", Status: port.StrategyConfigStatus(status),
		}
	}

	t.Run("happy path expires prev and inserts new active", func(t *testing.T) {
		truncateAll(t, pool)
		// seed prev active
		if err := scRepo.Insert(ctx, mkRec("cfg-prev", "active")); err != nil {
			t.Fatalf("seed prev: %v", err)
		}
		// promote new
		if err := promoter.PromoteActive(ctx, "cfg-prev", mkRec("cfg-new", "active")); err != nil {
			t.Fatalf("PromoteActive: %v", err)
		}
		// verify states
		var prevStatus, newStatus string
		_ = pool.QueryRow(ctx, "SELECT status FROM strategy_configs WHERE config_id='cfg-prev'").Scan(&prevStatus)
		_ = pool.QueryRow(ctx, "SELECT status FROM strategy_configs WHERE config_id='cfg-new'").Scan(&newStatus)
		if prevStatus != "expired" {
			t.Errorf("prev status: got %q want expired", prevStatus)
		}
		if newStatus != "active" {
			t.Errorf("new status: got %q want active", newStatus)
		}
	})

	t.Run("duplicate config_id rolls back: prev stays active", func(t *testing.T) {
		truncateAll(t, pool)
		// seed prev active
		if err := scRepo.Insert(ctx, mkRec("cfg-prev2", "active")); err != nil {
			t.Fatalf("seed prev: %v", err)
		}
		// seed a row with cfg-dup (any status) so the insert later collides
		if err := scRepo.Insert(ctx, mkRec("cfg-dup", "rejected")); err != nil {
			t.Fatalf("seed dup: %v", err)
		}
		// promote: Insert(cfg-dup) collides → Tx rolls back
		err := promoter.PromoteActive(ctx, "cfg-prev2", mkRec("cfg-dup", "active"))
		if err == nil {
			t.Fatal("expected duplicate error")
		}
		if !errors.Is(err, port.ErrDuplicateConfigID) {
			t.Errorf("expected ErrDuplicateConfigID; got %v", err)
		}
		// CRITICAL: prev must still be active (Tx rolled back the expire step)
		var prevStatus string
		_ = pool.QueryRow(ctx, "SELECT status FROM strategy_configs WHERE config_id='cfg-prev2'").Scan(&prevStatus)
		if prevStatus != "active" {
			t.Errorf("prev status after rollback: got %q want active", prevStatus)
		}
	})

	t.Run("empty prevConfigID skips expire step", func(t *testing.T) {
		truncateAll(t, pool)
		// no seed; just promote
		if err := promoter.PromoteActive(ctx, "", mkRec("cfg-first", "active")); err != nil {
			t.Fatalf("PromoteActive: %v", err)
		}
		var status string
		_ = pool.QueryRow(ctx, "SELECT status FROM strategy_configs WHERE config_id='cfg-first'").Scan(&status)
		if status != "active" {
			t.Errorf("first config status: got %q want active", status)
		}
	})
}
