package command

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fx-bot/backend/internal/adapter/broker"
	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
)

// External position auto-adoption (see docs/runtime/OPERATIONS_RUNBOOK.md §1).
//
// The user opens a position directly in the GMO app/web while the bot is
// either down or running. Reconcile sees broker has a position the DB
// doesn't. Previously the Live path tripped emergency_stop ("we can't
// manage what we didn't open"). The rule now: bot is
// DISPLAY-ONLY for external positions — adopt them so they appear in the
// dashboard, but the bot does NOT manage their TP/SL/MaxHold and does NOT
// count them toward `max_open_positions`.
//
// This file pins the reconcile half of external adoption:
//   1. Live startup: adopts naked_broker into recovered_positions with
//      reason="external_broker_adoption". emergency_stop NOT tripped.
//   2. Live runtime: same — naked_broker that appears mid-run is also
//      adopted (the user may trade in the GMO app while the bot is up).
//   3. The adopted row carries Source=PositionSourceExternalBroker so
//      downstream callers (EntryAdmission, ManageOpenPositions) can skip it.
//   4. strategy_config_id FK is satisfied via the active config — falls
//      back to paper_config when no active live_config exists yet (the
//      common case on first live restart before the advisor has produced
//      a live config).
//
// EntryAdmission and ManageOpenPositions skip behaviour are pinned by
// their own *_test.go files.

func seedActiveLiveConfig(t *testing.T, configID string) *backtest.InMemoryStrategyConfigRepo {
	t.Helper()
	repo := backtest.NewInMemoryStrategyConfigRepo()
	if configID != "" {
		repo.SeedActive(configID, "USD_JPY", "live_config")
	}
	return repo
}

func seedActiveLiveAndPaper(t *testing.T, liveID, paperID string) *backtest.InMemoryStrategyConfigRepo {
	t.Helper()
	repo := backtest.NewInMemoryStrategyConfigRepo()
	if liveID != "" {
		repo.SeedActive(liveID, "USD_JPY", "live_config")
	}
	if paperID != "" {
		repo.SeedActive(paperID, "USD_JPY", "paper_config")
	}
	return repo
}

func TestReconcile_F7a_LiveStartup_AdoptsExternalPositionAsExternalBroker(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	brokerPos := position.Position{
		BrokerPositionID: "broker-EXT-1", Symbol: "USD_JPY",
		Side: order.SideBuy, Quantity: 100, EntryPrice: 156.50,
		Status: position.StatusOpen, OpenedAt: time.Now(),
	}
	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
			return []position.Position{brokerPos}, nil
		},
	}
	posRepo := backtest.NewInMemoryPositionRepo()
	strat := seedActiveLiveConfig(t, "cfg-active-live")

	r := &Reconcile{
		Broker: mb, Positions: posRepo, StrategyConfigs: strat,
		Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode:     ReconcileModeStartup,
		LiveMode: config.ModeLiveConfig,
	}
	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Adopted != 1 {
		t.Errorf("summary.Adopted: got %d, want 1 (live external must be adopted, not tripped)", sum.Adopted)
	}
	if sum.Tripped != 0 {
		t.Errorf("summary.Tripped: got %d, want 0", sum.Tripped)
	}
	if _, err := os.Stat(flagPath); err == nil {
		t.Errorf("emergency_stop must NOT be tripped on live external adoption")
	}

	open, _ := posRepo.ListOpenOrClosing(ctx, "USD_JPY")
	if len(open) != 1 {
		t.Fatalf("DB must contain 1 adopted row, got %d", len(open))
	}
	if open[0].Source != port.PositionSourceExternalBroker {
		t.Errorf("Source: got %q, want %q (so downstream skips it)",
			open[0].Source, port.PositionSourceExternalBroker)
	}
	live, _ := posRepo.GetLive(ctx, open[0].ID)
	if live == nil || live.BrokerPositionID != "broker-EXT-1" {
		t.Errorf("adopted row must carry positions_live.broker_position_id; got %+v", live)
	}
}

func TestReconcile_F7a_LiveRuntime_AdoptsExternalPositionWithoutTripping(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	brokerPos := position.Position{
		BrokerPositionID: "broker-EXT-runtime", Symbol: "USD_JPY",
		Side: order.SideSell, Quantity: 200, EntryPrice: 157.20,
		Status: position.StatusOpen, OpenedAt: time.Now(),
	}
	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
			return []position.Position{brokerPos}, nil
		},
	}
	posRepo := backtest.NewInMemoryPositionRepo()
	strat := seedActiveLiveConfig(t, "cfg-live-runtime")

	r := &Reconcile{
		Broker: mb, Positions: posRepo, StrategyConfigs: strat,
		Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode:     ReconcileModeRuntime,
		LiveMode: config.ModeLiveConfig,
	}
	sum, _ := r.Run(ctx)

	if sum.Adopted != 1 {
		t.Errorf("summary.Adopted: got %d, want 1 (runtime external must be adopted)", sum.Adopted)
	}
	if sum.Tripped != 0 {
		t.Errorf("summary.Tripped: got %d, want 0 (user is allowed to open external positions while bot is running)", sum.Tripped)
	}
	if _, err := os.Stat(flagPath); err == nil {
		t.Errorf("emergency_stop must NOT be tripped on runtime external adoption")
	}
	open, _ := posRepo.ListOpenOrClosing(ctx, "USD_JPY")
	if len(open) != 1 || open[0].Source != port.PositionSourceExternalBroker {
		t.Errorf("expected 1 adopted external_broker row; got %+v", open)
	}
}

// Fallback: on first live restart, no active live_config exists yet (advisor
// hasn't produced one). The user still wants their external position adopted.
// The adoption path falls back to the active paper_config so the FK is
// satisfied. Source remains external_broker — the bot won't manage it.
func TestReconcile_F7a_LiveStartup_FallsBackToPaperConfigWhenNoActiveLiveConfig(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
			return []position.Position{{
				BrokerPositionID: "broker-EXT-fb", Symbol: "USD_JPY",
				Side: order.SideBuy, Quantity: 100, EntryPrice: 156.00,
				Status: position.StatusOpen, OpenedAt: time.Now(),
			}}, nil
		},
	}
	posRepo := backtest.NewInMemoryPositionRepo()
	// Active live config is missing; only the legacy paper one exists.
	strat := seedActiveLiveAndPaper(t, "", "cfg-paper-legacy")

	r := &Reconcile{
		Broker: mb, Positions: posRepo, StrategyConfigs: strat,
		Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode:     ReconcileModeStartup,
		LiveMode: config.ModeLiveConfig,
	}
	sum, _ := r.Run(ctx)
	if sum.Adopted != 1 || sum.Tripped != 0 {
		t.Errorf("expected 1 adopted, 0 tripped via paper fallback; got %+v", sum)
	}
	if _, err := os.Stat(flagPath); err == nil {
		t.Errorf("emergency_stop must NOT be tripped when paper fallback applies")
	}
	open, _ := posRepo.ListOpenOrClosing(ctx, "USD_JPY")
	if len(open) != 1 {
		t.Fatalf("want 1 row, got %d", len(open))
	}
	if open[0].StrategyConfigID != "cfg-paper-legacy" {
		t.Errorf("FK config_id: got %q, want fallback %q", open[0].StrategyConfigID, "cfg-paper-legacy")
	}
	if open[0].Source != port.PositionSourceExternalBroker {
		t.Errorf("Source even on fallback must be external_broker; got %q", open[0].Source)
	}
}

// When NO active config exists at all (neither live nor paper), reconcile
// has no FK target — trip emergency_stop with a clear reason. This is the
// expected behaviour for a brand-new install with zero advisor cycles AND
// an externally-opened position; the operator must either close the
// external position or seed a config before restarting.
func TestReconcile_F7a_LiveStartup_TripsWhenNoActiveConfigAtAll(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
			return []position.Position{{
				BrokerPositionID: "broker-EXT-none", Symbol: "USD_JPY",
				Side: order.SideBuy, Quantity: 100, EntryPrice: 156.00,
				Status: position.StatusOpen, OpenedAt: time.Now(),
			}}, nil
		},
	}
	posRepo := backtest.NewInMemoryPositionRepo()
	strat := seedActiveLiveAndPaper(t, "", "")

	r := &Reconcile{
		Broker: mb, Positions: posRepo, StrategyConfigs: strat,
		Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode:     ReconcileModeStartup,
		LiveMode: config.ModeLiveConfig,
	}
	sum, _ := r.Run(ctx)
	if sum.Tripped == 0 {
		t.Errorf("Tripped: got 0, want > 0 (no active config means no FK target)")
	}
	if _, err := os.Stat(flagPath); err != nil {
		t.Errorf("emergency_stop flag should exist when no config FK target available: %v", err)
	}
}
