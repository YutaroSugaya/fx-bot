//go:build integration

package repository

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/port"
)

// requireDB opens a pgx pool against INTEGRATION_TEST_DB_URL.
// 未設定なら t.Skip。CI で Docker が無いマシンでも make test が壊れないように。
func requireDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("INTEGRATION_TEST_DB_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_TEST_DB_URL not set; skipping integration test")
	}
	// Last wall against wiping a real DB — refuse to run table-wiping tests
	// unless the DSN is a dedicated
	// _test DB and distinct from DATABASE_URL. Fires regardless of make / env.
	if err := SafeIntegrationTestDSN(dsn, os.Getenv("DATABASE_URL")); err != nil {
		t.Fatalf("refusing integration run: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

// truncateAll wipes every table the integration suite touches. CASCADE
// handles FK dependencies so order is irrelevant. Run before each test
// to guarantee a clean slate.
func truncateAll(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	const stmt = `
TRUNCATE TABLE
  trade_signals,
  trades,
  position_state_events,
  recovered_positions,
  manual_positions,
  positions_live,
  positions,
  signal_rejections,
  advisor_run_errors,
  advisor_run_io,
  ai_advisor_runs,
  market_summaries,
  config_validation_events,
  strategy_config_parse_failures,
  strategy_config_activations,
  strategy_config_rejections,
  strategy_configs,
  candles
RESTART IDENTITY CASCADE`
	if _, err := pool.Exec(context.Background(), stmt); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// seedStrategyConfig inserts a generated-status row so FK-bearing tests
// (positions.strategy_config_id → strategy_configs.config_id) can reference
// a real config without exercising the full promote/active flow.
func seedStrategyConfig(t *testing.T, pool *pgxpool.Pool, configID string) {
	t.Helper()
	repo := NewStrategyConfigRepo(pool)
	now := time.Now().UTC()
	if err := repo.Insert(context.Background(), port.StrategyConfigRecord{
		ConfigID:               configID,
		Source:                 "manual",
		Mode:                   "paper_config",
		Symbol:                 "USD_JPY",
		Enabled:                true,
		MarketRegimeType:       "trend",
		MarketRegimeConfidence: 0.5,
		StrategyName:           "test",
		ValidFrom:              now,
		ValidUntil:             now.Add(time.Hour),
		RawYAML:                "test: true\n",
		Status:                 port.StrategyConfigStatusGenerated,
	}); err != nil {
		t.Fatalf("seed strategy_configs: %v", err)
	}
}
