package command

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fx-bot/backend/internal/adapter/broker"
	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
)

// Scenario: a freshly-opened live position can be OMITTED from the broker's
// GetOpenPositions feed for ~90s (feed lag on the open), so runtime reconcile
// deems it stale. Its close fill is not yet resolvable (the position is still
// genuinely OPEN at the broker — its SL has not been hit yet) and the current
// price is still inside the OCO band (ambiguous). After the 90s grace, the old
// code booked a 0-PnL synthetic "reconcile_cold_close" trade and flipped the
// row CLOSED — which PERMANENTLY destroyed the real SL fill that resolved
// minutes later.
//
// Fix: a stale, unresolvable live-runtime position whose price is still inside
// its OCO band must NOT be 0-PnL synthetic-closed. Keep DEFERRING so a later
// pass records the real fill. Only after a much longer hard window does it trip
// (alert a human) — still WITHOUT fabricating PnL.

// Core fix: after grace, with an unresolvable fill and an ambiguous (in-band)
// price, reconcile DEFERS (no 0-PnL trade, no trip) — then resolves the real SL
// fill on a later pass once the executions feed catches up.
func TestReconcile_LiveRuntime_StaleDB_AfterGrace_AmbiguousPrice_DefersInsteadOfSyntheticZeroPnL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	openedAt := time.Date(2026, 6, 8, 5, 51, 22, 0, time.UTC)
	pass1 := openedAt.Add(5 * time.Second)   // first stale detection (feed omitted the new position)
	pass2 := openedAt.Add(95 * time.Second)  // grace (90s) elapsed — old code synthetic-closed here
	pass3 := openedAt.Add(160 * time.Second) // real SL fill finally appears

	id, _ := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "EUR_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 184.724,
			TakeProfitPips: 12, StopLossPips: 6,
			StrategyConfigID: "frozen-mompull-eurjpy-v1",
			Status:           port.PositionStatusOpen, OpenedAt: openedAt,
		},
		// No leg ids recorded → resolution goes via positionId lookup.
		Live: &port.PositionLive{BrokerPositionID: "1000001"},
	})

	feedHasClose := false // becomes true on pass 3 (executions feed catches up)
	mb := &broker.MockBroker{
		// The still-open position is missing from the broker feed (the lag bug).
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) { return nil, nil },
		// Ambiguous mid (between SL 184.664 and TP 184.844) → estimate can't fire.
		GetTickerFn: func(_ context.Context, _ string) (*market.Ticker, error) {
			return &market.Ticker{Symbol: "EUR_JPY", Bid: 184.719, Ask: 184.721}, nil
		},
		GetLatestExecutionsBySymbolFn: func(_ context.Context, _ string) ([]order.Execution, error) {
			if feedHasClose {
				// SELL = close of the BUY; at the SL price; after open.
				return []order.Execution{{
					PositionID: "1000001", Side: order.SideSell,
					Price: 184.664, Quantity: 1000, Timestamp: pass3,
				}}, nil
			}
			return nil, nil
		},
	}

	clockNow := pass1
	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "EUR_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode:                ReconcileModeRuntime,
		LiveMode:            config.ModeLiveConfig,
		Closer:              backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		StaleGracePeriod:    90 * time.Second,
		StaleHardTripPeriod: 10 * time.Minute,
		Clock:               func() time.Time { return clockNow },
	}

	// Pass 1: first stale observation → defer (grace not yet elapsed).
	sum1, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run pass1: %v", err)
	}
	if sum1.Deferred != 1 || sum1.Resolved != 0 || sum1.Tripped != 0 {
		t.Fatalf("pass1: want Deferred=1 Resolved=0 Tripped=0; got %+v", sum1)
	}

	// Pass 2: grace elapsed, fill still unresolvable, price ambiguous.
	// MUST defer — must NOT book a 0-PnL synthetic close, must NOT trip.
	clockNow = pass2
	sum2, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run pass2: %v", err)
	}
	if sum2.Resolved != 0 {
		t.Errorf("pass2 Resolved: got %d want 0 (must NOT synthetic-close at 0 PnL)", sum2.Resolved)
	}
	if sum2.Tripped != 0 {
		t.Errorf("pass2 Tripped: got %d want 0 (still within hard window — defer)", sum2.Tripped)
	}
	if sum2.Deferred != 1 {
		t.Errorf("pass2 Deferred: got %d want 1", sum2.Deferred)
	}
	if _, statErr := os.Stat(flagPath); statErr == nil {
		t.Errorf("emergency_stop flag must NOT exist on pass2")
	}
	if trades, _ := tradeRepo.ListSince(ctx, openedAt, 10); len(trades) != 0 {
		t.Fatalf("pass2: no trade may be recorded (0-PnL synthetic fabrication forbidden); got %+v", trades)
	}
	if open, _ := posRepo.ListOpenOrClosing(ctx, "EUR_JPY"); len(open) != 1 {
		t.Errorf("pass2: position must remain open/closing; got %d", len(open))
	}

	// Pass 3: executions feed catches up → resolve the REAL SL fill, real PnL.
	feedHasClose = true
	clockNow = pass3
	sum3, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run pass3: %v", err)
	}
	if sum3.Resolved != 1 {
		t.Errorf("pass3 Resolved: got %d want 1 (real SL fill resolved)", sum3.Resolved)
	}
	if sum3.Tripped != 0 {
		t.Errorf("pass3 Tripped: got %d want 0", sum3.Tripped)
	}
	trades, _ := tradeRepo.ListSince(ctx, openedAt, 10)
	if len(trades) != 1 {
		t.Fatalf("pass3: want exactly 1 trade (the real SL), got %d: %+v", len(trades), trades)
	}
	tr := trades[0]
	if tr.CloseReason != "stop_loss" {
		t.Errorf("CloseReason: got %q want stop_loss (real fill, not reconcile_cold_close)", tr.CloseReason)
	}
	if math.Abs(tr.ExitPrice-184.664) > 1e-6 {
		t.Errorf("ExitPrice: got %v want 184.664 (real SL)", tr.ExitPrice)
	}
	if tr.ProfitLossJPY >= 0 {
		t.Errorf("ProfitLossJPY: got %v want negative (the real loss must not vanish to 0)", tr.ProfitLossJPY)
	}
	_ = id
}

// Escalation: a position that stays stale & unresolvable past the hard window
// trips emergency_stop (alert a human) — but STILL never fabricates a 0-PnL
// synthetic trade row.
func TestReconcile_LiveRuntime_StaleDB_PastHardWindow_TripsWithoutSyntheticZeroPnL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	openedAt := time.Date(2026, 6, 8, 5, 51, 22, 0, time.UTC)
	pass1 := openedAt.Add(5 * time.Second)
	passHard := openedAt.Add(11 * time.Minute) // past the 10m hard window

	_, _ = posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "EUR_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 184.724,
			TakeProfitPips: 12, StopLossPips: 6,
			StrategyConfigID: "frozen-mompull-eurjpy-v1",
			Status:           port.PositionStatusOpen, OpenedAt: openedAt,
		},
		Live: &port.PositionLive{BrokerPositionID: "1000001"},
	})

	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) { return nil, nil },
		GetTickerFn: func(_ context.Context, _ string) (*market.Ticker, error) {
			return &market.Ticker{Symbol: "EUR_JPY", Bid: 184.719, Ask: 184.721}, nil
		},
		GetLatestExecutionsBySymbolFn: func(_ context.Context, _ string) ([]order.Execution, error) { return nil, nil },
	}

	clockNow := pass1
	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "EUR_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode:                ReconcileModeRuntime,
		LiveMode:            config.ModeLiveConfig,
		Closer:              backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		StaleGracePeriod:    90 * time.Second,
		StaleHardTripPeriod: 10 * time.Minute,
		Clock:               func() time.Time { return clockNow },
	}

	if _, err := r.Run(ctx); err != nil { // pass1: defer
		t.Fatalf("Run pass1: %v", err)
	}

	clockNow = passHard
	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run passHard: %v", err)
	}
	if sum.Tripped != 1 {
		t.Errorf("Tripped: got %d want 1 (genuine orphan past hard window)", sum.Tripped)
	}
	if sum.Resolved != 0 {
		t.Errorf("Resolved: got %d want 0", sum.Resolved)
	}
	if _, statErr := os.Stat(flagPath); statErr != nil {
		t.Errorf("emergency_stop flag must exist after hard-window trip")
	}
	// The crucial invariant: NO fabricated 0-PnL trade row, ever.
	if trades, _ := tradeRepo.ListSince(ctx, openedAt, 10); len(trades) != 0 {
		t.Errorf("no synthetic 0-PnL trade may be written even on trip; got %+v", trades)
	}
}
