package command

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/safety"
)

// Manual override is an EXPLICIT, allowlist-only flag.
// direction_* is on the overridable allowlist so an
// operator can place a manual trade against Claude's no_trade / direction-
// constrained recommendation. Default (AllowOverride=false) still hits the
// same gate as auto. Hard market safety (emergency_stop / no_active_config /
// daily_loss) remains enforced regardless of AllowOverride.

func newAdmissionForOverrideTest(t *testing.T, posRepo port.PositionRepository, tradeRepo port.TradeRepository, flagPath string, maxOpen int) *EntryAdmission {
	t.Helper()
	return &EntryAdmission{
		Mutex:     &sync.Mutex{},
		Positions: posRepo,
		Trades:    tradeRepo,
		BotConfig: &config.BotConfig{
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
			Symbol: "USD_JPY",
			Risk:   config.RiskSection{MaxOpenPositions: maxOpen},
		},
		EmergencyFlagPath: flagPath,
		Logger:            silentLogger(),
		Clock:             time.Now,
	}
}

// Default override=false: manual entry at cap must be REJECTED (= same gate
// as auto). Regression guard: older code passed AllowOverride=true
// unconditionally.
func TestEntryAdmission_ManualNoOverride_BlockedAtMaxOpenPositions(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	_, _ = posRepo.Insert(context.Background(), port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 100,
			StrategyConfigID: "cfg-seed",
			Status:           port.PositionStatusOpen, OpenedAt: time.Now(),
		},
	})
	a := newAdmissionForOverrideTest(t, posRepo, tradeRepo, flag, 1)

	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 1},
		Entry: config.EntrySection{Direction: config.DirectionBoth},
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	verdict, release, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal:        sig,
		ActiveConfig:  cfg,
		AllowOverride: false, // explicit: no operator override
		Source:        "manual",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if verdict.Allowed {
		t.Fatalf("manual without AllowOverride must be denied at max_open_positions; got Allowed=true")
	}
	if release != nil {
		t.Errorf("release should be nil on denial")
	}
}

// AllowOverride=true bypasses direction policy mismatch (operator can
// place a BUY despite a sell-only active config): operator-explicit
// manual entries should be able to override Claude's directional view.
func TestEntryAdmission_ManualOverride_AllowsDirectionMismatch(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	a := newAdmissionForOverrideTest(t, posRepo, tradeRepo, flag, 5)

	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 5},
		Entry: config.EntrySection{Direction: config.DirectionSellOnly}, // sell-only
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	verdict, release, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal:        sig,
		ActiveConfig:  cfg,
		AllowOverride: true,
		Source:        "manual",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if !verdict.Allowed {
		t.Fatalf("AllowOverride=true must bypass direction policy; got reason=%q", verdict.Reason)
	}
	if release == nil {
		t.Errorf("release callback must be non-nil on allow")
	} else {
		release()
	}
}

// AllowOverride=true now bypasses direction_none (operator can place a
// manual entry while active config is no_trade).
func TestEntryAdmission_ManualOverride_AllowsDirectionNone(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	a := newAdmissionForOverrideTest(t, posRepo, tradeRepo, flag, 5)

	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 5},
		Entry: config.EntrySection{Direction: config.DirectionNone},
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	verdict, release, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal:        sig,
		ActiveConfig:  cfg,
		AllowOverride: true,
		Source:        "manual",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if !verdict.Allowed {
		t.Fatalf("AllowOverride=true must bypass direction_none; got reason=%q", verdict.Reason)
	}
	if release == nil {
		t.Errorf("release callback must be non-nil on allow")
	} else {
		release()
	}
}

// AllowOverride=false (= same gate as auto): direction_none still rejects.
func TestEntryAdmission_ManualNoOverride_RejectsDirectionNone(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	a := newAdmissionForOverrideTest(t, posRepo, tradeRepo, flag, 5)

	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 5},
		Entry: config.EntrySection{Direction: config.DirectionNone},
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	verdict, _, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal: sig, ActiveConfig: cfg,
		AllowOverride: false, Source: "manual",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if verdict.Allowed || verdict.Reason != "direction_none" {
		t.Fatalf("without override, direction_none must reject; verdict=%+v", verdict)
	}
}

// AllowOverride=true must NOT bypass emergency_stop.
func TestEntryAdmission_ManualOverride_RejectsEmergencyStop(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	if err := safety.Trip(flag, "test"); err != nil {
		t.Fatalf("trip: %v", err)
	}
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	a := newAdmissionForOverrideTest(t, posRepo, tradeRepo, flag, 5)

	cfg := &config.StrategyConfig{Entry: config.EntrySection{Direction: config.DirectionBoth}}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	verdict, _, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal: sig, ActiveConfig: cfg,
		AllowOverride: true, Source: "manual",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if verdict.Allowed || verdict.Reason != "emergency_stop" {
		t.Fatalf("emergency_stop must never be overridable; verdict=%+v", verdict)
	}
}

// AllowOverride=true must NOT bypass daily_loss cap.
func TestEntryAdmission_ManualOverride_RejectsDailyLoss(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	// Pin clock to midday UTC so the seeded loss stays inside today's
	// (UTC) window regardless of when the test runs (see sibling test
	// in admission_hard_safety_recheck_test.go for the same rationale).
	fixedNow := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	// Seed a closing trade today that exceeds the cap.
	loss := port.TradeRecord{
		Symbol: "USD_JPY", Side: "BUY", Quantity: 100,
		EntryPrice: 150, ExitPrice: 149,
		ProfitLossPips: -100, ProfitLossJPY: -100_000,
		CloseReason: "stop_loss",
		OpenedAt:    fixedNow.Add(-2 * time.Hour),
		ClosedAt:    fixedNow.Add(-1 * time.Hour),
	}
	if err := tradeRepo.Insert(context.Background(), loss); err != nil {
		t.Fatalf("seed loss: %v", err)
	}

	a := &EntryAdmission{
		Mutex:     &sync.Mutex{},
		Positions: posRepo,
		Trades:    tradeRepo,
		BotConfig: &config.BotConfig{
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
			Symbol: "USD_JPY",
			Risk: config.RiskSection{
				MaxOpenPositions: 5,
				MaxDailyLossJPY:  50_000, // cap exceeded by seed loss
			},
		},
		EmergencyFlagPath: flag,
		Logger:            silentLogger(),
		Clock:             func() time.Time { return fixedNow },
	}

	cfg := &config.StrategyConfig{Entry: config.EntrySection{Direction: config.DirectionBoth}}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	verdict, _, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal: sig, ActiveConfig: cfg,
		AllowOverride: true, Source: "manual",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if verdict.Allowed {
		t.Fatalf("daily_loss must never be overridable; got Allowed=true reason=%q", verdict.Reason)
	}
	if !strings.HasPrefix(verdict.Reason, "daily_loss") {
		t.Errorf("reason: got %q want daily_loss prefix", verdict.Reason)
	}
}

// isOverridableReason: explicit allowlist check (the unit test for the
// allowlist refactor). Confirms that future rejection reasons default to
// NOT overridable until explicitly added.
func TestIsOverridableReason_Allowlist(t *testing.T) {
	overridable := []string{
		"open_positions 1 >= cap 1",
		"cooldown after_loss until 12:00:00",
		"consecutive_losses 3 >= cap 3",
		"trades_in_window 5 >= cap 5",
		"loss_in_window -1000 >= cap -1000",
		"direction_none",
		"direction_buy_only_blocks_short",
		"direction_sell_only_blocks_long",
		// spread is in the allowlist (operator accepts
		// the cost via the dashboard's live spread display).
		"spread 5.00 > cap 3.00",
	}
	for _, r := range overridable {
		if !isOverridableReason(r) {
			t.Errorf("expected %q to be overridable", r)
		}
	}
	notOverridable := []string{
		"emergency_stop",
		"no_active_config",
		"daily_loss 100 >= cap 50",
		"some_future_reason_that_did_not_exist_yet", // default-deny
	}
	for _, r := range notOverridable {
		if isOverridableReason(r) {
			t.Errorf("expected %q to be NOT overridable", r)
		}
	}
}
