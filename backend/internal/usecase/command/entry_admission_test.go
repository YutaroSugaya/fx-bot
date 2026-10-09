package command

import (
	"context"
	"path/filepath"
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

func newAdmission(t *testing.T, posRepo port.PositionRepository, tradeRepo port.TradeRepository, flagPath string, maxOpen int) (*EntryAdmission, *sync.Mutex) {
	t.Helper()
	mu := &sync.Mutex{}
	return &EntryAdmission{
		Mutex:     mu,
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
	}, mu
}

// E2 fix: emergency_stop blocks both auto and manual entries unconditionally.
func TestEntryAdmission_EmergencyStopBlocksManualOverride(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	if err := safety.Trip(flag, "test_setup"); err != nil {
		t.Fatalf("trip: %v", err)
	}
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	a, _ := newAdmission(t, posRepo, tradeRepo, flag, 5)

	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	verdict, release, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal:        sig,
		AllowOverride: true, // operator override flag — must NOT bypass emergency_stop
		Source:        "manual",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if verdict.Allowed {
		t.Fatalf("manual entry must be denied while emergency_stop is active (got Allowed=true)")
	}
	if verdict.Reason != "emergency_stop" {
		t.Errorf("reason: got %q want emergency_stop", verdict.Reason)
	}
	if release != nil {
		t.Errorf("release callback should be nil on denial")
	}
}

// E1 fix: auto entry is denied when, AT THE LOCK CHECK, OpenPositions has
// already reached the cap (e.g. a manual entry committed between snapshot
// and lock).
func TestEntryAdmission_AutoBlockedWhenPositionsAtCap(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	// Pre-seed one OPEN position to simulate "another path committed first".
	_, _ = posRepo.Insert(context.Background(), port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 100,
			StrategyConfigID: "cfg-seed",
			Status:           port.PositionStatusOpen, OpenedAt: time.Now(),
		},
	})
	a, _ := newAdmission(t, posRepo, tradeRepo, flag, 1) // max=1, already 1 open

	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 1},
		Entry: config.EntrySection{Direction: config.DirectionBoth},
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	verdict, release, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal:       sig,
		ActiveConfig: cfg,
		Source:       "auto",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if verdict.Allowed {
		t.Fatalf("auto entry must be denied when OpenPositions >= cap; got Allowed=true")
	}
	if release != nil {
		t.Errorf("release callback should be nil on denial")
	}
}

// 2 連敗の状態でも admission verdict の QtyMultiplier は 1.0 (連敗時の qty 半減は廃止)。
// ExecuteOrder.OnSignal は verdict.QtyMultiplier を読んで実際の発注 qty を決める。
// 注: 同方向 SL 2 回の direction block を避けるため side を混ぜる
// (1 BUY SL + 1 SELL SL → 連敗 streak は 2 だが方向別 SL は各 1)。
func TestEntryAdmission_NoQtyHalveOnTwoConsecutiveLosses(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	// Fixed clock so seeded SLs and the snapshot's startOfDay share one instant
	// — otherwise this flakes in the ~1h after midnight (bot TZ).
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	seedAlternatingSLs(t, tradeRepo, now)
	a, _ := newAdmission(t, posRepo, tradeRepo, flag, 5)
	a.Clock = func() time.Time { return now }
	// MaxConsecutiveLosses=4 を入れて binary cap が 2 で発火しないことを担保。
	a.BotConfig.Risk.MaxConsecutiveLosses = 4

	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 5},
		Entry: config.EntrySection{Direction: config.DirectionBoth},
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	verdict, release, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal:       sig,
		ActiveConfig: cfg,
		Source:       "auto",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if !verdict.Allowed {
		t.Fatalf("expected allow at 2 consecutive losses; reason=%q", verdict.Reason)
	}
	// 連敗時の qty 半減は廃止: 2 連敗でも full qty (mul=1.0)。
	if verdict.QtyMultiplier != 1.0 {
		t.Errorf("verdict.QtyMultiplier: got %v want 1.0 (halve retired)", verdict.QtyMultiplier)
	}
	if release != nil {
		release()
	}
}

// Manual entry with AllowOverride=true should pass when the only block is
// max_open_positions (operator chooses to add another). The open position is on
// the OPPOSITE side: a same-side one would hit the no-nanpin rule, which is never
// overridable (entry_admission_no_nanpin_override_test.go).
func TestEntryAdmission_ManualOverridePassesOverridableReason(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	_, _ = posRepo.Insert(context.Background(), port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "SELL", Quantity: 100, EntryPrice: 100,
			StrategyConfigID: "cfg-seed",
			Status:           port.PositionStatusOpen, OpenedAt: time.Now(),
		},
	})
	a, _ := newAdmission(t, posRepo, tradeRepo, flag, 1)

	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 1},
		Entry: config.EntrySection{Direction: config.DirectionBoth},
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
		t.Fatalf("manual override should allow max_open_positions case; reason=%q", verdict.Reason)
	}
	if release == nil {
		t.Fatalf("release callback must be non-nil on success")
	}
	release()
}
