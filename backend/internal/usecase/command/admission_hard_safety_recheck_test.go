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
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
)

// Regression: EntryAdmission.CheckAndHold used to call
// risk.EvaluateSignal once and trusted its single reason. EvaluateSignal
// short-circuits, so when an overridable gate fired BEFORE a hard gate
// (e.g. cooldown before daily_loss, direction_none before spread), the
// override allowed the trade through while the hard gate stayed
// unevaluated. These tests pin the second-pass hard-safety check.

// Cooldown is overridable; daily_loss is hard. EvaluateSignal returns
// the cooldown reason first, so without the recheck the override would
// fly past daily_loss. Admission must still reject on daily_loss.
func TestEntryAdmission_ManualOverride_CooldownButDailyLossExceeded_StillRejects(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()

	// Pin clock to midday UTC so the seeded loss stays inside today's
	// (UTC) window regardless of when the test actually runs. Without a
	// fixed clock, runs near 00:00 UTC put the "30m ago" seed in yesterday
	// and the daily_loss aggregate returns 0.
	fixedNow := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	// Seed a closed trade today that already exceeds the daily loss cap.
	loss := port.TradeRecord{
		Symbol: "USD_JPY", Side: "BUY", Quantity: 100,
		EntryPrice: 150, ExitPrice: 149,
		ProfitLossPips: -100, ProfitLossJPY: -60_000,
		CloseReason: "stop_loss",
		OpenedAt:    fixedNow.Add(-2 * time.Hour),
		ClosedAt:    fixedNow.Add(-30 * time.Minute), // recent → cooldown also fires
	}
	if err := tradeRepo.Insert(context.Background(), loss); err != nil {
		t.Fatalf("seed loss: %v", err)
	}

	// Wire cooldown so it fires alongside daily_loss.
	cooldown := func(now time.Time) (bool, time.Time, string) {
		return true, now.Add(5 * time.Minute), "after_loss"
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
				MaxDailyLossJPY:  50_000, // 60_000 loss > 50_000 cap → daily_loss trips
			},
		},
		EmergencyFlagPath: flag,
		Logger:            silentLogger(),
		Clock:             func() time.Time { return fixedNow },
		CooldownFn:        cooldown,
	}

	cfg := &config.StrategyConfig{Entry: config.EntrySection{Direction: config.DirectionBoth}}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	verdict, _, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal:        sig,
		ActiveConfig:  cfg,
		AllowOverride: true, // operator override
		Source:        "manual",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if verdict.Allowed {
		t.Fatalf("override must NOT bypass daily_loss even when cooldown fires first; got allowed")
	}
	if !strings.HasPrefix(verdict.Reason, "daily_loss") {
		t.Errorf("expected daily_loss reason to surface; got %q", verdict.Reason)
	}
}

// spread is on the operator-override allowlist (not hard safety).
// Both direction_none and spread are overridable, so a manual entry
// with AllowOverride=true must pass even when both fire simultaneously.
// (Hard-safety re-check still pins emergency_stop / daily_loss / no_active_config
// — see the cooldown-but-daily-loss test above for that contract.)
func TestEntryAdmission_ManualOverride_DirectionNoneAndSpreadTooWide_AllAllowed(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	a := newAdmissionForOverrideTest(t, posRepo, tradeRepo, flag, 5)

	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 5},
		Entry: config.EntrySection{Direction: config.DirectionNone, MaxSpreadPips: 1.0},
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}

	// Pass a Ticker with a wide spread (Bid=150, Ask=151 → 100 pip @ 0.01).
	bidAsk := &market.Ticker{Symbol: "USD_JPY", Bid: 150.00, Ask: 151.00, Timestamp: time.Now()}

	verdict, release, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal:        sig,
		ActiveConfig:  cfg,
		Ticker:        bidAsk,
		AllowOverride: true,
		Source:        "manual",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if !verdict.Allowed {
		t.Fatalf("manual override must allow both direction_none and wide spread; got reason=%q", verdict.Reason)
	}
	if release == nil {
		t.Error("release must be non-nil on Allowed=true")
	} else {
		release()
	}
}
