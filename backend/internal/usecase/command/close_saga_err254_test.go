package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
)

// Settle-leg vs. bot-close race.
//
// When the broker reports the position is already gone during a close (GMO
// ERR-254 "Not found position"), that is NOT a naked position — it's the
// opposite. The settle leg (SL/TP OCO) filled first and already closed the
// position, racing our MaxHold/manual close. The close saga must therefore
// SKIP emergency_stop (and let reconcile record the broker-side fill) instead
// of halting the whole bot.
//
// Typical trigger: a position hits its SL exactly at the MaxHold deadline; GMO
// closes it via the SL OCO; the bot's MaxHold close then finds no position →
// ERR-254. Treating that as naked would trip emergency_stop and halt trading.
func TestExecuteCloseSaga_BrokerPositionNotFound_IsBenign_NoEmergency(t *testing.T) {
	pos := backtest.NewInMemoryPositionRepo()
	id, err := pos.Insert(context.Background(), port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 100.0,
			TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240,
			StrategyConfigID: "cfg-test", Status: port.PositionStatusOpen,
			OpenedAt: time.Now().Add(-time.Hour),
		},
		Live: &port.PositionLive{BrokerPositionID: "gmo-pos-1", TPOrderID: "tp-1", SLOrderID: "sl-1"},
	})
	if err != nil {
		t.Fatalf("setup insert: %v", err)
	}

	// Broker close fails with the GMO "already gone" envelope, wrapped in the
	// sentinel exactly like the real adapter does.
	br := &fakeLiveBroker{fakeBroker: fakeBroker{
		closePosErr: fmt.Errorf("gmo api status=1: Not found position. (ERR-254): %w", port.ErrBrokerPositionNotFound),
	}}
	flag := tempFlag(t)
	cmd := &ClosePositionCommand{
		Mode: config.ModeLiveConfig, Symbol: "USD_JPY", PipSize: 0.01,
		Broker: br, Positions: pos, Closer: &fakeCloser{ok: true},
		Mutex: &sync.Mutex{}, EmergencyFlagPath: flag, Logger: silentLogger(),
	}

	_, err = cmd.Execute(context.Background(), ClosePositionInput{PositionID: id})

	// Benign: manual close maps the saga's ErrPositionAlreadyClosing → ErrPositionNotFound.
	if !errors.Is(err, ErrPositionNotFound) {
		t.Fatalf("want benign ErrPositionNotFound, got %v", err)
	}
	// THE critical assertion: emergency_stop must NOT be written for ERR-254.
	if _, ferr := os.Stat(flag); ferr == nil {
		t.Error("emergency_stop flag was written; ERR-254 (already flat at broker) must NOT trip emergency")
	}
}

// Contrast guard: a DIFFERENT close error (not ERR-254) must STILL trip
// emergency — the fix must be narrow, not blanket-suppress close failures.
func TestExecuteCloseSaga_OtherCloseError_StillTripsEmergency(t *testing.T) {
	pos := backtest.NewInMemoryPositionRepo()
	id, err := pos.Insert(context.Background(), port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 100.0,
			TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240,
			StrategyConfigID: "cfg-test", Status: port.PositionStatusOpen,
			OpenedAt: time.Now().Add(-time.Hour),
		},
		Live: &port.PositionLive{BrokerPositionID: "gmo-pos-1", TPOrderID: "tp-1", SLOrderID: "sl-1"},
	})
	if err != nil {
		t.Fatalf("setup insert: %v", err)
	}
	br := &fakeLiveBroker{fakeBroker: fakeBroker{closePosErr: errors.New("broker 500")}}
	flag := tempFlag(t)
	cmd := &ClosePositionCommand{
		Mode: config.ModeLiveConfig, Symbol: "USD_JPY", PipSize: 0.01,
		Broker: br, Positions: pos, Closer: &fakeCloser{ok: true},
		Mutex: &sync.Mutex{}, EmergencyFlagPath: flag, Logger: silentLogger(),
	}
	if _, err = cmd.Execute(context.Background(), ClosePositionInput{PositionID: id}); err == nil {
		t.Fatal("expected critical error for non-ERR-254 close failure")
	}
	if _, ferr := os.Stat(flag); ferr != nil {
		t.Errorf("emergency_stop flag must still be written for a real close failure; stat err=%v", ferr)
	}
}
