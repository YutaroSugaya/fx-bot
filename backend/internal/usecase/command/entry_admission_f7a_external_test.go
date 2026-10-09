package command

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
)

// External positions (opened by the user directly in the GMO app)
// must NOT count toward `max_open_positions` capacity. The bot is display-only
// for them; blocking the bot entirely off a single app-side trade would
// surprise the user ("I opened one trade in the app and now my bot stops").
//
// The pyramiding rule refines this: an external position must still
// block the bot from stacking the SAME SIDE (pyramiding), but the OPPOSITE side
// must remain open (capacity not frozen). This test seeds an external BUY and
// proves a bot SELL is still ALLOWED; the same-side block is pinned below.
func TestEntryAdmission_F7a_ExternalOppositeSideDoesNotFreezeCapacity(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()

	// Pre-seed 1 EXTERNAL BUY position. Without the external filter, this fills the
	// max_open_positions=1 cap and the bot entry below would be rejected.
	_, _ = posRepo.Insert(context.Background(), port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 156.30,
			StrategyConfigID: "cfg-seed",
			Status:           port.PositionStatusOpen, OpenedAt: time.Now(),
		},
		Live: &port.PositionLive{BrokerPositionID: "broker-EXT-seed"},
		Recovered: &port.RecoveredPositionMeta{
			Reason:      port.RecoveryReasonExternalBrokerAdoption,
			RecoveredAt: time.Now(),
		},
	})

	a, _ := newAdmission(t, posRepo, tradeRepo, flag, 1) // max=1, but the 1 open is external

	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 1},
		Entry: config.EntrySection{Direction: config.DirectionBoth},
	}
	// Opposite side (SELL) of the external BUY: capacity must NOT be frozen.
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideSell, TakeProfitPips: 10, StopLossPips: 10}
	verdict, release, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal:       sig,
		ActiveConfig: cfg,
		Source:       "auto",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if !verdict.Allowed {
		t.Fatalf("opposite-side auto entry must be ALLOWED when the only open is an external opposite-side position; got denied reason=%q", verdict.Reason)
	}
	if release == nil {
		t.Errorf("release callback must be non-nil on Allowed=true")
	} else {
		release()
	}
}

// A same-side external position MUST block the bot from
// stacking the same direction (pyramiding), even though it is excluded from
// capacity. external BUY → bot BUY is rejected.
func TestEntryAdmission_F7a_ExternalSameSideBlocksPyramiding(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()

	_, _ = posRepo.Insert(context.Background(), port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 156.30,
			StrategyConfigID: "cfg-seed",
			Status:           port.PositionStatusOpen, OpenedAt: time.Now(),
		},
		Live: &port.PositionLive{BrokerPositionID: "broker-EXT-seed"},
		Recovered: &port.RecoveredPositionMeta{
			Reason:      port.RecoveryReasonExternalBrokerAdoption,
			RecoveredAt: time.Now(),
		},
	})

	a, _ := newAdmission(t, posRepo, tradeRepo, flag, 1)
	cfg := &config.StrategyConfig{
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 1},
		Entry: config.EntrySection{Direction: config.DirectionBoth},
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	verdict, _, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal: sig, ActiveConfig: cfg, Source: "auto",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if verdict.Allowed {
		t.Fatal("same-side bot entry must be BLOCKED by pyramiding when a same-side external position is open (no pyramiding)")
	}
}

// Symmetric: a bot-owned position DOES count, so when the cap is full of
// bot positions the entry is denied. This pins the "we only exclude
// external" behaviour — without it a naive change might exclude ALL
// positions and the cap would stop working.
func TestEntryAdmission_F7a_BotPositionStillCountsTowardMaxOpenPositions(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()

	// Bot-owned position (no Recovered marker → Source=PositionSourceBot).
	_, _ = posRepo.Insert(context.Background(), port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 156.30,
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
	verdict, _, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal: sig, ActiveConfig: cfg, Source: "auto",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if verdict.Allowed {
		t.Fatalf("entry must be denied when a bot-owned position already fills the cap; got Allowed=true")
	}
}
