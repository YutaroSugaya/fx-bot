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

// Pinning: CLOSING 状態の position に armed ratchet が発火しない。
//
// CLOSING は close saga が進行中 (または人間が意図的に bot 不干渉へ退避した状態)
// を意味し、OnTick が再評価すると saga と race する。
// OnTick は Status != OPEN を ratchet 状態更新よりも前に skip しなければならない。
// この不変条件は戦略 retune で壊してはいけない。
func TestManageOpenPositions_Pinning_ClosingPositionArmedRatchetDoesNotFire(t *testing.T) {
	ctx := context.Background()
	posRepo := backtest.NewInMemoryPositionRepo()

	// armed ratchet + peak から giveback (8pips) を大きく超えて戻った価格。
	// Status が OPEN なら確実に ratchet_takeprofit で close される条件。
	_, err := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 100.00,
			TakeProfitPips: 80, StopLossPips: 50, MaxHoldMinutes: 480,
			StrategyConfigID:    "cfg-pin",
			RatchetArmPips:      16,
			RatchetGivebackPips: 8,
			PeakUnrealizedPips:  13.5,
			RatchetArmed:        true,
			Status:              port.PositionStatusClosing,
			OpenedAt:            time.Now(),
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
	// unrealized 0 pips ≤ peak(13.5) − give(8) = 5.5 → OPEN なら発火する価格。
	tk := market.Ticker{Bid: 100.00, Ask: 100.01}
	if err := u.OnTick(ctx, tk); err != nil {
		t.Fatalf("OnTick: %v", err)
	}
	if br.closePosCalls != 0 {
		t.Errorf("CLOSING position must NOT be closed by armed ratchet; got %d ClosePosition calls", br.closePosCalls)
	}
	rows, _ := posRepo.ListOpenOrClosing(ctx, "USD_JPY")
	if len(rows) != 1 || rows[0].Status != port.PositionStatusClosing {
		t.Errorf("position must remain CLOSING untouched; got %+v", rows)
	}
	if rows[0].PeakUnrealizedPips != 13.5 || !rows[0].RatchetArmed {
		t.Errorf("ratchet runtime state of CLOSING position must not be mutated; got peak=%v armed=%v",
			rows[0].PeakUnrealizedPips, rows[0].RatchetArmed)
	}
}

// 対照: 同一条件で Status=OPEN なら ratchet は発火して close される。
// 上の skip が「ratchet の全停止」でなく Status ガードによるものであることを固定。
func TestManageOpenPositions_Pinning_OpenPositionArmedRatchetStillFires(t *testing.T) {
	ctx := context.Background()
	posRepo := backtest.NewInMemoryPositionRepo()

	_, err := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 100.00,
			TakeProfitPips: 80, StopLossPips: 50, MaxHoldMinutes: 480,
			StrategyConfigID:    "cfg-pin",
			RatchetArmPips:      16,
			RatchetGivebackPips: 8,
			PeakUnrealizedPips:  13.5,
			RatchetArmed:        true,
			Status:              port.PositionStatusOpen,
			OpenedAt:            time.Now(),
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
	tk := market.Ticker{Bid: 100.00, Ask: 100.01}
	if err := u.OnTick(ctx, tk); err != nil {
		t.Fatalf("OnTick: %v", err)
	}
	if br.closePosCalls != 1 {
		t.Errorf("OPEN position with armed+retraced ratchet must close once; got %d calls", br.closePosCalls)
	}
}
