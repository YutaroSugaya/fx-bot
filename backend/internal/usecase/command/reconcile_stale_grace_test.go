package command

import (
	"context"
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

// Race: GMO settles a position via its OCO SL leg, but the bot's runtime
// reconcile can run in the ~30s window before the GMO executions feed returns
// that fill. resolveAndRecordClose finds no fill, the estimated/synthetic
// fallbacks don't apply, and (without a grace period) reconcile trips
// emergency_stop ("reconcile:stale_db_position:<id>"). One reconcile cycle
// later the real SL fill is resolvable and the trade can be recorded correctly
// — so tripping immediately is premature.
//
// Fix: a grace period. Within StaleGracePeriod of first observing a stale,
// unresolvable runtime position, reconcile must DEFER — neither synthetic-close
// nor trip — so the real fill has a chance to resolve on a later pass.

// Within the grace window, a stale-but-unresolvable live runtime position must
// NOT trip emergency_stop and must NOT be synthetic-closed. It is deferred.
func TestReconcile_LiveRuntime_StaleDBPosition_DefersTripWithinGracePeriod(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	openedAt := time.Date(2026, 6, 5, 12, 6, 0, 0, time.UTC)
	now := openedAt.Add(24 * time.Minute) // first stale detection

	_, _ = posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "SELL", Quantity: 1000, EntryPrice: 159.855,
			TakeProfitPips: 12, StopLossPips: 6,
			StrategyConfigID: "frozen-mompull-usdjpy-v1",
			Status:           port.PositionStatusOpen, OpenedAt: openedAt,
		},
		Live: &port.PositionLive{BrokerPositionID: "broker-1", SLOrderID: "sl-1"},
	})

	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
			return nil, nil // broker already settled — position is gone
		},
		GetExecutionsFn: func(_ context.Context, _ string) ([]order.Execution, error) {
			return nil, nil // executions feed has not caught up yet (the race)
		},
		// Ambiguous mid (between SL and TP) so recordEstimatedClose can't fire.
		GetTickerFn: func(_ context.Context, _ string) (*market.Ticker, error) {
			return &market.Ticker{Bid: 159.855, Ask: 159.855}, nil
		},
	}

	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode:             ReconcileModeRuntime,
		LiveMode:         config.ModeLiveConfig,
		Closer:           backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		StaleGracePeriod: 90 * time.Second,
		Clock:            func() time.Time { return now },
	}
	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Tripped != 0 {
		t.Errorf("Tripped: got %d want 0 (must defer within grace period)", sum.Tripped)
	}
	if sum.Resolved != 0 {
		t.Errorf("Resolved: got %d want 0 (must not synthetic-close within grace)", sum.Resolved)
	}
	if sum.Deferred != 1 {
		t.Errorf("Deferred: got %d want 1", sum.Deferred)
	}
	if _, statErr := os.Stat(flagPath); statErr == nil {
		t.Errorf("emergency_stop flag must NOT exist within grace period")
	}
	// Position must remain open/closing — not synthetic-closed.
	open, _ := posRepo.ListOpenOrClosing(ctx, "USD_JPY")
	if len(open) != 1 {
		t.Errorf("position should remain open within grace; got %d open/closing", len(open))
	}
	// No trade row written.
	trades, _ := tradeRepo.ListSince(ctx, openedAt, 10)
	if len(trades) != 0 {
		t.Errorf("no trade should be recorded within grace; got %d", len(trades))
	}
}

// The expected happy path: pass 1 defers (feed lagging), pass 2 (one
// cycle later) resolves the REAL SL fill — no trip, real PnL recorded.
func TestReconcile_LiveRuntime_StaleDBPosition_ResolvesOnRetryAfterDefer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	openedAt := time.Date(2026, 6, 5, 12, 6, 0, 0, time.UTC)
	pass1 := openedAt.Add(24 * time.Minute)
	pass2 := pass1.Add(30 * time.Second) // next reconcile cycle

	_, _ = posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "SELL", Quantity: 1000, EntryPrice: 159.855,
			TakeProfitPips: 12, StopLossPips: 6,
			StrategyConfigID: "frozen-mompull-usdjpy-v1",
			Status:           port.PositionStatusOpen, OpenedAt: openedAt,
		},
		Live: &port.PositionLive{BrokerPositionID: "broker-1", SLOrderID: "sl-1"},
	})

	feedReady := false // becomes true on pass 2
	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
			return nil, nil
		},
		GetExecutionsFn: func(_ context.Context, orderID string) ([]order.Execution, error) {
			if feedReady && orderID == "sl-1" {
				return []order.Execution{{Price: 159.915, Quantity: 1000, Timestamp: pass1}}, nil
			}
			return nil, nil
		},
		GetTickerFn: func(_ context.Context, _ string) (*market.Ticker, error) {
			return &market.Ticker{Bid: 159.855, Ask: 159.855}, nil
		},
	}

	clockNow := pass1
	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode:             ReconcileModeRuntime,
		LiveMode:         config.ModeLiveConfig,
		Closer:           backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		StaleGracePeriod: 90 * time.Second,
		Clock:            func() time.Time { return clockNow },
	}

	// Pass 1: feed lagging → defer.
	sum1, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run pass1: %v", err)
	}
	if sum1.Deferred != 1 || sum1.Tripped != 0 || sum1.Resolved != 0 {
		t.Fatalf("pass1: want Deferred=1 Tripped=0 Resolved=0; got %+v", sum1)
	}

	// Pass 2: feed caught up → resolve real SL fill, no trip.
	feedReady = true
	clockNow = pass2
	sum2, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run pass2: %v", err)
	}
	if sum2.Resolved != 1 {
		t.Errorf("pass2 Resolved: got %d want 1", sum2.Resolved)
	}
	if sum2.Tripped != 0 {
		t.Errorf("pass2 Tripped: got %d want 0", sum2.Tripped)
	}
	if _, statErr := os.Stat(flagPath); statErr == nil {
		t.Errorf("emergency_stop flag must NOT exist after clean resolution")
	}
	trades, _ := tradeRepo.ListSince(ctx, openedAt, 10)
	if len(trades) != 1 || trades[0].CloseReason != "stop_loss" || trades[0].ExitPrice != 159.915 {
		t.Errorf("expected 1 stop_loss trade @ 159.915; got %+v", trades)
	}
}
