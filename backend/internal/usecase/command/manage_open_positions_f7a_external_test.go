package command

import (
	"context"
	"sync"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
)

// ManageOpenPositions.OnTick must skip positions with
// Source=PositionSourceExternalBroker. The user owns those positions in the
// GMO app/web; the bot must not silently close them on a TP/SL/MaxHold
// match. The skip is unconditional (Paper or Live mode).
func TestManageOpenPositions_F7a_OnTickSkipsExternalBrokerPositions(t *testing.T) {
	ctx := context.Background()
	posRepo := backtest.NewInMemoryPositionRepo()

	// Seed an EXTERNAL position. Mark it with a TP pip distance that *would*
	// have triggered against the tick below — the only thing preventing the
	// close should be the external Source skip.
	_, err := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 100.00,
			TakeProfitPips: 5, StopLossPips: 50, MaxHoldMinutes: 240,
			StrategyConfigID: "cfg-seed",
			Status:           port.PositionStatusOpen, OpenedAt: time.Now(),
		},
		Live: &port.PositionLive{BrokerPositionID: "broker-EXT-omt"},
		Recovered: &port.RecoveredPositionMeta{
			Reason:      port.RecoveryReasonExternalBrokerAdoption,
			RecoveredAt: time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	br := &fakeBroker{}
	u := &ManageOpenPositions{
		Broker: br, Positions: posRepo,
		Trades: backtest.NewInMemoryTradeRepo(),
		Closer: backtest.NewInMemoryPositionCloser(posRepo, backtest.NewInMemoryTradeRepo()),
		Symbol: "USD_JPY", PipSize: 0.01,
		Mode:       config.ModePaperConfig,
		CloseMutex: &sync.Mutex{},
		Logger:     silentLogger(),
		Clock:      time.Now,
	}
	// Ticker price that would trigger TP for the BUY (entry 100.00 + 5 pips):
	// bid >= 100.05 means TP-hit in evaluateExit.
	tk := market.Ticker{Bid: 100.10, Ask: 100.11}
	if err := u.OnTick(ctx, tk); err != nil {
		t.Fatalf("OnTick: %v", err)
	}
	if br.closePosCalls != 0 {
		t.Errorf("broker.ClosePosition must NOT be called for external positions; got %d calls", br.closePosCalls)
	}
	open, _ := posRepo.ListOpenOrClosing(ctx, "USD_JPY")
	if len(open) != 1 || open[0].Status != port.PositionStatusOpen {
		t.Errorf("external position must remain OPEN; got %+v", open)
	}
}

// Symmetric: a bot-owned position with the same TP/SL setup IS closed —
// confirms the skip is targeted (Source-based), not blanket disablement.
func TestManageOpenPositions_F7a_OnTickStillClosesBotOwnedPositions(t *testing.T) {
	ctx := context.Background()
	posRepo := backtest.NewInMemoryPositionRepo()

	_, err := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 100.00,
			TakeProfitPips: 5, StopLossPips: 50, MaxHoldMinutes: 240,
			StrategyConfigID: "cfg-seed",
			Status:           port.PositionStatusOpen, OpenedAt: time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	br := &fakeBroker{}
	u := &ManageOpenPositions{
		Broker: br, Positions: posRepo,
		Trades: backtest.NewInMemoryTradeRepo(),
		Closer: backtest.NewInMemoryPositionCloser(posRepo, backtest.NewInMemoryTradeRepo()),
		Symbol: "USD_JPY", PipSize: 0.01,
		Mode:       config.ModePaperConfig,
		CloseMutex: &sync.Mutex{},
		Logger:     silentLogger(),
		Clock:      time.Now,
	}
	tk := market.Ticker{Bid: 100.10, Ask: 100.11}
	if err := u.OnTick(ctx, tk); err != nil {
		t.Fatalf("OnTick: %v", err)
	}
	if br.closePosCalls != 1 {
		t.Errorf("bot-owned TP hit must close once; got %d calls", br.closePosCalls)
	}
}
