package backtest

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/port"
)

func TestInMemoryStrategyConfigRepo_InsertGetActive_OnlyLatestActiveWins(t *testing.T) {
	ctx := context.Background()
	repo := NewInMemoryStrategyConfigRepo()

	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	_ = repo.Insert(ctx, port.StrategyConfigRecord{
		ConfigID: "cfg-old", Mode: "paper_config", Symbol: "USD_JPY",
		Status: port.StrategyConfigStatusActive, ValidFrom: now.Add(-time.Hour),
	})
	_ = repo.Insert(ctx, port.StrategyConfigRecord{
		ConfigID: "cfg-new", Mode: "paper_config", Symbol: "USD_JPY",
		Status: port.StrategyConfigStatusActive, ValidFrom: now,
	})

	got, err := repo.GetActive(ctx, "USD_JPY", "paper_config")
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if got == nil || got.ConfigID != "cfg-new" {
		t.Fatalf("expected cfg-new (most recently inserted active), got %+v", got)
	}
}

func TestInMemoryStrategyConfigRepo_GetActive_NilWhenNone(t *testing.T) {
	ctx := context.Background()
	repo := NewInMemoryStrategyConfigRepo()
	got, err := repo.GetActive(ctx, "USD_JPY", "paper_config")
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for empty repo; got %+v", got)
	}
}

func TestInMemoryStrategyConfigRepo_GetActive_FiltersBySymbolAndMode(t *testing.T) {
	ctx := context.Background()
	repo := NewInMemoryStrategyConfigRepo()
	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)

	_ = repo.Insert(ctx, port.StrategyConfigRecord{
		ConfigID: "live-USDJPY", Mode: "live_config", Symbol: "USD_JPY",
		Status: port.StrategyConfigStatusActive, ValidFrom: now,
	})
	_ = repo.Insert(ctx, port.StrategyConfigRecord{
		ConfigID: "paper-EURJPY", Mode: "paper_config", Symbol: "EUR_JPY",
		Status: port.StrategyConfigStatusActive, ValidFrom: now,
	})

	// Query (USD_JPY, paper_config) → no match.
	got, _ := repo.GetActive(ctx, "USD_JPY", "paper_config")
	if got != nil {
		t.Errorf("expected nil for unmatched (symbol, mode); got %+v", got)
	}

	// Query (EUR_JPY, paper_config) → matches paper-EURJPY.
	got, _ = repo.GetActive(ctx, "EUR_JPY", "paper_config")
	if got == nil || got.ConfigID != "paper-EURJPY" {
		t.Errorf("expected paper-EURJPY; got %+v", got)
	}
}

func TestInMemoryStrategyConfigRepo_MarkExpired_FlipsStatus(t *testing.T) {
	ctx := context.Background()
	repo := NewInMemoryStrategyConfigRepo()
	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	_ = repo.Insert(ctx, port.StrategyConfigRecord{
		ConfigID: "cfg-1", Mode: "paper_config", Symbol: "USD_JPY",
		Status: port.StrategyConfigStatusActive, ValidFrom: now,
	})

	if err := repo.MarkExpired(ctx, "cfg-1"); err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}
	got, _ := repo.GetActive(ctx, "USD_JPY", "paper_config")
	if got != nil {
		t.Errorf("MarkExpired should remove from active; got %+v", got)
	}
	// Audit accessor — observable for tests that assert which config was expired.
	if exp := repo.ExpiredIDs(); len(exp) != 1 || exp[0] != "cfg-1" {
		t.Errorf("ExpiredIDs: got %v", exp)
	}
}

func TestInMemoryStrategyConfigRepo_MarkActive_RecordsTimestamp(t *testing.T) {
	ctx := context.Background()
	repo := NewInMemoryStrategyConfigRepo()
	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	_ = repo.Insert(ctx, port.StrategyConfigRecord{
		ConfigID: "cfg-x", Mode: "paper_config", Symbol: "USD_JPY",
		Status: port.StrategyConfigStatusGenerated, ValidFrom: now,
	})

	at := now.Add(5 * time.Minute)
	if err := repo.MarkActive(ctx, "cfg-x", at); err != nil {
		t.Fatalf("MarkActive: %v", err)
	}
	ts, ok := repo.ActivatedAt("cfg-x")
	if !ok || !ts.Equal(at) {
		t.Errorf("ActivatedAt: ok=%v ts=%v want %v", ok, ts, at)
	}
}

func TestInMemoryStrategyConfigRepo_ListRecent_HonorsLimit(t *testing.T) {
	ctx := context.Background()
	repo := NewInMemoryStrategyConfigRepo()
	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		_ = repo.Insert(ctx, port.StrategyConfigRecord{
			ConfigID:  string(rune('a' + i)),
			Mode:      "paper_config",
			Symbol:    "USD_JPY",
			Status:    port.StrategyConfigStatusGenerated,
			ValidFrom: now.Add(time.Duration(i) * time.Minute),
		})
	}
	rows, err := repo.ListRecent(ctx, 3)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("ListRecent: got %d want 3", len(rows))
	}
}

func TestInMemoryValidationEventRepo_Insert_And_ByType(t *testing.T) {
	ctx := context.Background()
	repo := NewInMemoryValidationEventRepo()

	_ = repo.Insert(ctx, port.ConfigValidationEvent{ConfigID: "c1", ValidationType: "schema", Status: "pass"})
	_ = repo.Insert(ctx, port.ConfigValidationEvent{ConfigID: "c1", ValidationType: "hard_limit", Status: "fail", Message: "qty>cap"})
	_ = repo.Insert(ctx, port.ConfigValidationEvent{ConfigID: "c2", ValidationType: "schema", Status: "fail"})

	schemaPass := repo.ByType("schema", "pass")
	if len(schemaPass) != 1 || schemaPass[0].ConfigID != "c1" {
		t.Errorf("ByType schema/pass: got %+v", schemaPass)
	}
	hardFail := repo.ByType("hard_limit", "fail")
	if len(hardFail) != 1 || hardFail[0].Message != "qty>cap" {
		t.Errorf("ByType hard_limit/fail: got %+v", hardFail)
	}
}
