package command

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
)

// EUR_USD (the first USD-quote pair) needs the live USD/JPY rate to convert its
// USD-denominated PnL to JPY. The saga resolves that rate UP FRONT, before any
// irreversible action. If the USD/JPY ticker fetch fails transiently, the close
// must:
//   - return ErrCloseRateUnavailable (benign, retried next tick),
//   - NOT trip emergency_stop (a transient ticker blip must not halt the bot),
//   - NOT claim/close the position (it stays OPEN, still protected by its OCO).
//
// JPY-quote pairs are the implicit contrast: every other saga test uses USD_JPY
// and stays green, proving the new step-0 resolve is a no-op (rate=1.0, no
// GetTicker call) for JPY-quote pairs.
func TestExecuteCloseSaga_USDQuote_RateUnavailable_DefersNoEmergencyNoClaim(t *testing.T) {
	ctx := context.Background()
	pos := backtest.NewInMemoryPositionRepo()
	id, err := pos.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "EUR_USD", Side: "BUY", Quantity: 1000, EntryPrice: 1.08000,
			TakeProfitPips: 12, StopLossPips: 6, MaxHoldMinutes: 90,
			StrategyConfigID: "frozen-mompull-eurusd-v1", Status: port.PositionStatusOpen,
			OpenedAt: time.Now().Add(-time.Hour),
		},
		Live: &port.PositionLive{BrokerPositionID: "gmo-pos-1", TPOrderID: "tp-1", SLOrderID: "sl-1"},
	})
	if err != nil {
		t.Fatalf("setup insert: %v", err)
	}

	// USD/JPY ticker fetch fails. closePos would succeed, but we must never reach it.
	br := &fakeLiveBroker{fakeBroker: fakeBroker{tickerErr: errors.New("usdjpy ticker timeout")}}
	flag := tempFlag(t)
	rec := mustGetOpen(t, pos, id)

	res, err := ExecuteCloseSaga(ctx, CloseSagaInput{
		Mode: config.ModeLiveConfig, Symbol: "EUR_USD",
		Broker: br, Positions: pos, Closer: &fakeCloser{ok: true},
		EmergencyFlagPath: flag, Logger: silentLogger(),
	}, rec, "max_hold", 0, time.Now())

	if !errors.Is(err, ErrCloseRateUnavailable) {
		t.Fatalf("want ErrCloseRateUnavailable, got %v", err)
	}
	if res != (CloseSagaResult{}) {
		t.Errorf("expected zero result on deferred close, got %+v", res)
	}
	// Must NOT trip emergency for a transient ticker blip.
	if _, ferr := os.Stat(flag); ferr == nil {
		t.Error("emergency_stop flag written; a transient USD/JPY fetch failure must NOT trip emergency")
	}
	// Position must be untouched (never claimed) → still OPEN, not CLOSING.
	after := mustGetOpen(t, pos, id)
	if after.Status != port.PositionStatusOpen {
		t.Errorf("position status = %q, want OPEN (close must be deferred, not claimed)", after.Status)
	}
}
