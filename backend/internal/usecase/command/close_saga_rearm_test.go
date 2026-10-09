package command

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/port"
)

// Scenario: a Live close cancels the OCO settle legs (step 2) and then the
// market-close (step 3) is rejected by GMO with ERR-5218 "The order was
// rejected for trading close" — the weekend-close / restricted window. Without
// a guard the saga trips emergency_stop and leaves the position NAKED (no
// SL/TP) at the broker over the weekend.
//
// Fix: before tripping, RE-ARM the protective OCO (re-place TP/SL). If the
// broker accepts the OCO (a fresh settle order is allowed even when a market-
// close is refused), the position is protected again — do NOT trip; leave the
// row CLOSING so reconcile records the eventual OCO fill (self-healing). Only
// trip when the re-arm ALSO fails (genuinely unprotectable).

func closeRejected5218() error {
	return errors.New("gmo api status=1: The order was rejected for trading close. (ERR-5218)")
}

// Re-arm succeeds → no emergency_stop, OCO re-placed with the original targets,
// position left protected (not CLOSED).
func TestExecuteCloseSaga_CloseRejected_RearmsOCO_NoEmergency(t *testing.T) {
	ctx := context.Background()
	posRepo := backtest.NewInMemoryPositionRepo()
	id, err := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 160.187,
			TakeProfitPips: 12, StopLossPips: 6, MaxHoldMinutes: 60,
			StrategyConfigID: "frozen-mompull-usdjpy-v1",
			Status:           port.PositionStatusOpen,
			OpenedAt:         time.Now().Add(-time.Hour),
		},
		Live: &port.PositionLive{BrokerPositionID: "1000001", TPOrderID: "tp-1", SLOrderID: "sl-1"},
	})
	if err != nil {
		t.Fatalf("setup insert: %v", err)
	}

	br := &fakeLiveBroker{fakeBroker: fakeBroker{closePosErr: closeRejected5218()}}
	// settleOCOErr unset → re-arm succeeds.
	flag := tempFlag(t)
	in := CloseSagaInput{
		Mode: config.ModeLiveConfig, Symbol: "USD_JPY",
		Broker: br, Positions: posRepo, Closer: &fakeCloser{ok: true},
		EmergencyFlagPath: flag, Logger: silentLogger(),
	}
	rec := mustGetOpen(t, posRepo, id)

	_, err = ExecuteCloseSaga(ctx, in, rec, "max_hold", 0, time.Now())

	// Benign sentinel — not a critical close failure.
	if !errors.Is(err, ErrCloseRejectedRearmed) {
		t.Fatalf("want ErrCloseRejectedRearmed, got %v", err)
	}
	// THE critical assertion: emergency_stop must NOT be written when the OCO
	// was successfully re-armed.
	if _, ferr := os.Stat(flag); ferr == nil {
		t.Error("emergency_stop flag was written; a re-armed position is NOT naked")
	}
	// OCO must have been re-placed exactly once, with the original SL/TP.
	if br.settleOCOCalls != 1 {
		t.Fatalf("PlaceSettleOCO must be called once to re-arm; got %d", br.settleOCOCalls)
	}
	got := br.settleOCOLastArgs
	if got.BrokerPositionID != 1000001 {
		t.Errorf("re-arm broker position id: got %v want 1000001", got.BrokerPositionID)
	}
	if got.Side != order.SideSell {
		t.Errorf("re-arm side: got %v want SELL (close side of a BUY)", got.Side)
	}
	// BUY @ 160.187, pip 0.01: TP +12 = 160.307, SL -6 = 160.127.
	if !approxEq(got.TPPrice, 160.307) || !approxEq(got.SLPrice, 160.127) {
		t.Errorf("re-arm TP/SL: got tp=%.3f sl=%.3f want tp=160.307 sl=160.127", got.TPPrice, got.SLPrice)
	}
	// Position must NOT be CLOSED — it is left protected (CLOSING) for reconcile.
	open, _ := posRepo.ListOpenOrClosing(ctx, "USD_JPY")
	if len(open) != 1 {
		t.Errorf("position must remain (protected); got %d open/closing", len(open))
	}
}

// Re-arm ALSO fails → genuinely naked → trip emergency_stop (legacy behaviour).
func TestExecuteCloseSaga_CloseRejected_RearmFails_TripsEmergency(t *testing.T) {
	ctx := context.Background()
	posRepo := backtest.NewInMemoryPositionRepo()
	id, err := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 160.187,
			TakeProfitPips: 12, StopLossPips: 6, MaxHoldMinutes: 60,
			StrategyConfigID: "frozen-mompull-usdjpy-v1",
			Status:           port.PositionStatusOpen,
			OpenedAt:         time.Now().Add(-time.Hour),
		},
		Live: &port.PositionLive{BrokerPositionID: "1000001", TPOrderID: "tp-1", SLOrderID: "sl-1"},
	})
	if err != nil {
		t.Fatalf("setup insert: %v", err)
	}

	br := &fakeLiveBroker{fakeBroker: fakeBroker{closePosErr: closeRejected5218()}}
	br.settleOCOErr = errors.New("oco also rejected (market closed)")
	flag := tempFlag(t)
	in := CloseSagaInput{
		Mode: config.ModeLiveConfig, Symbol: "USD_JPY",
		Broker: br, Positions: posRepo, Closer: &fakeCloser{ok: true},
		EmergencyFlagPath: flag, Logger: silentLogger(),
	}
	rec := mustGetOpen(t, posRepo, id)

	if _, err = ExecuteCloseSaga(ctx, in, rec, "max_hold", 0, time.Now()); err == nil {
		t.Fatal("expected critical error when re-arm also fails")
	}
	if errors.Is(err, ErrCloseRejectedRearmed) {
		t.Fatal("must NOT be the benign sentinel when re-arm failed")
	}
	// emergency_stop MUST be written — the position is genuinely naked.
	if _, ferr := os.Stat(flag); ferr != nil {
		t.Errorf("emergency_stop flag must be written when re-arm fails; stat err=%v", ferr)
	}
}

// Variant: leg ids were never recorded at entry (soft-
// fail), so the close saga cancelled ZERO legs (discovery also failed during
// the closed-market window) — the original OCO is still live at GMO. The close
// was then rejected (ERR-5218). The saga must NOT trip ("naked" is false) and
// must NOT re-arm (that would DUPLICATE the live OCO). Benign skip.
func TestExecuteCloseSaga_CloseRejected_LegsIntact_NoRearm_NoEmergency(t *testing.T) {
	ctx := context.Background()
	posRepo := backtest.NewInMemoryPositionRepo()
	id, err := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 160.187,
			TakeProfitPips: 12, StopLossPips: 6, MaxHoldMinutes: 60,
			StrategyConfigID: "frozen-mompull-usdjpy-v1",
			Status:           port.PositionStatusOpen,
			OpenedAt:         time.Now().Add(-time.Hour),
		},
		// Leg ids EMPTY (soft-fail at entry); broker position id present.
		Live: &port.PositionLive{BrokerPositionID: "1000001"},
	})
	if err != nil {
		t.Fatalf("setup insert: %v", err)
	}

	br := &fakeLiveBroker{fakeBroker: fakeBroker{closePosErr: closeRejected5218()}}
	// Discovery fails (restricted window) → no legs found → cancelled == 0.
	br.settleLegsErr = errors.New("discovery failed: market closed")
	flag := tempFlag(t)
	in := CloseSagaInput{
		Mode: config.ModeLiveConfig, Symbol: "USD_JPY",
		Broker: br, Positions: posRepo, Closer: &fakeCloser{ok: true},
		EmergencyFlagPath: flag, Logger: silentLogger(),
	}
	rec := mustGetOpen(t, posRepo, id)

	_, err = ExecuteCloseSaga(ctx, in, rec, "max_hold", 0, time.Now())

	// Benign — close rejected but protection was never removed.
	if !errors.Is(err, ErrCloseRejectedLegsIntact) {
		t.Fatalf("want ErrCloseRejectedLegsIntact, got %v", err)
	}
	// MUST NOT trip — the position is not naked.
	if _, ferr := os.Stat(flag); ferr == nil {
		t.Error("emergency_stop flag written; cancelled 0 legs = protection intact, not naked")
	}
	// MUST NOT re-arm — that would duplicate the still-live OCO.
	if br.settleOCOCalls != 0 {
		t.Errorf("PlaceSettleOCO must NOT be called when no legs were cancelled; got %d", br.settleOCOCalls)
	}
}

func mustGetOpen(t *testing.T, repo port.PositionRepository, id int64) port.PositionRecord {
	t.Helper()
	open, err := repo.ListOpenOrClosing(context.Background(), "")
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	for _, r := range open {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("position %d not found open", id)
	return port.PositionRecord{}
}
