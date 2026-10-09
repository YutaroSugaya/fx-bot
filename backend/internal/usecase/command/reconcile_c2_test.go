package command

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fx-bot/backend/internal/adapter/broker"
	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
)

// Live startup must not blind-MarkClosed a stale DB position. The
// position likely closed via broker-side TP/SL while the bot was down — we
// must consult the recorded settle-leg executions and record a real trade
// (with fill price + close reason) when possible. Only paper startup may use
// the synthetic zero-PnL trade fallback.

func TestReconcile_LiveStartup_StaleDB_WithTPExecution_RecordsRealTrade(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	openedAt := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	closedAt := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)

	id, _ := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 150.00,
			StrategyConfigID: "cfg-live", Status: port.PositionStatusOpen, OpenedAt: openedAt,
		},
		Live: &port.PositionLive{
			BrokerPositionID: "broker-LIVE-Z",
			TPOrderID:        "tp-1",
			SLOrderID:        "sl-1",
		},
	})

	// Broker says position is gone (broker-side TP fired while bot was down).
	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
			return nil, nil
		},
		// TP fill found; SL has no fill (didn't fire).
		GetExecutionsFn: func(_ context.Context, orderID string) ([]order.Execution, error) {
			if orderID == "tp-1" {
				return []order.Execution{{Price: 150.30, Quantity: 100, Timestamp: closedAt}}, nil
			}
			return nil, nil
		},
	}

	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode:     ReconcileModeStartup,
		LiveMode: config.ModeLiveConfig, // C.2: Live mode triggers resolution path
		Closer:   backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		Clock:    func() time.Time { return closedAt },
	}
	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Resolved != 1 {
		t.Errorf("Resolved: got %d want 1 (Live startup must resolve via TP execution)", sum.Resolved)
	}
	if sum.MarkedDone != 0 {
		t.Errorf("MarkedDone: got %d want 0 (no synthetic close in Live)", sum.MarkedDone)
	}
	if sum.Tripped != 0 {
		t.Errorf("Tripped: got %d want 0 (resolution succeeded)", sum.Tripped)
	}

	// Trade row must reflect REAL fill price (150.30), not synthetic zero PnL.
	trades, _ := tradeRepo.ListSince(ctx, openedAt, 10)
	if len(trades) != 1 {
		t.Fatalf("expected 1 trade row, got %d", len(trades))
	}
	tr := trades[0]
	if tr.ExitPrice != 150.30 {
		t.Errorf("ExitPrice: got %v want 150.30 (real TP fill)", tr.ExitPrice)
	}
	if tr.CloseReason != "take_profit" {
		t.Errorf("CloseReason: got %q want take_profit", tr.CloseReason)
	}
	if tr.ProfitLossJPY == 0 {
		t.Errorf("ProfitLossJPY should be non-zero (real fill); got 0 — likely synthetic close")
	}

	// emergency_stop must NOT be tripped on successful Live resolution.
	if _, err := os.Stat(flagPath); err == nil {
		t.Errorf("emergency_stop should NOT be tripped on successful resolution")
	}

	_ = id
}

// Live + unresolvable stale-CLOSING must NOT trip emergency_stop: that would
// leave the bot un-startable after a partial close failure, and the position is
// already gone at broker (no active risk).
// At startup it must NOT synthetic-close either (premature 0-PnL) — it DEFERS to
// the runtime reconcile loop, whose grace window gives the GMO executions feed
// time to catch up so the real exit price/PnL is recorded. An immediate
// synthetic close would throw the real P&L away.
func TestReconcile_LiveStartup_StaleDB_NoExecutionFound_DefersToRuntime(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	openedAt := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)

	id, _ := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 150.00,
			StrategyConfigID: "cfg-live", Status: port.PositionStatusOpen, OpenedAt: openedAt,
		},
		Live: &port.PositionLive{
			BrokerPositionID: "broker-LIVE-Y",
			TPOrderID:        "tp-2",
			SLOrderID:        "sl-2",
		},
	})

	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
			return nil, nil
		},
		// Neither leg shows a fill — fill price unknown (feed not yet caught up).
		GetExecutionsFn: func(_ context.Context, _ string) ([]order.Execution, error) {
			return nil, nil
		},
	}

	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode:     ReconcileModeStartup,
		LiveMode: config.ModeLiveConfig,
		Closer:   backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
	}
	sum, _ := r.Run(ctx)
	if sum.Deferred != 1 {
		t.Errorf("Deferred: got %d want 1 (startup defers unresolvable stale to runtime grace)", sum.Deferred)
	}
	if sum.Resolved != 0 {
		t.Errorf("Resolved: got %d want 0 (must NOT synthetic-close at startup)", sum.Resolved)
	}
	if sum.Tripped != 0 {
		t.Errorf("Tripped: got %d want 0 (defer, do not trip)", sum.Tripped)
	}
	if _, err := os.Stat(flagPath); err == nil {
		t.Errorf("emergency_stop flag must NOT exist — deferred, not tripped")
	}
	// No trade row may be written — deferring must not book a premature 0-PnL trade.
	seen, _ := tradeRepo.ListSince(ctx, openedAt, 10)
	if len(seen) != 0 {
		t.Fatalf("defer must write 0 trade rows; got %d", len(seen))
	}
	// Position stays OPEN for the runtime loop to resolve/close.
	still, _ := posRepo.ListOpenOrClosing(ctx, "USD_JPY")
	found := false
	for _, p := range still {
		if p.ID == id && p.Status == port.PositionStatusOpen {
			found = true
		}
	}
	if !found {
		t.Errorf("position %d must remain OPEN after startup defer", id)
	}
}

func TestReconcile_LiveStartup_StaleDB_SLExecution_RecordsStopLoss(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	openedAt := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	closedAt := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)

	_, _ = posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 150.00,
			StrategyConfigID: "cfg-live", Status: port.PositionStatusOpen, OpenedAt: openedAt,
		},
		Live: &port.PositionLive{
			BrokerPositionID: "broker-LIVE-X",
			TPOrderID:        "tp-3",
			SLOrderID:        "sl-3",
		},
	})

	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
			return nil, nil
		},
		GetExecutionsFn: func(_ context.Context, orderID string) ([]order.Execution, error) {
			if orderID == "sl-3" {
				return []order.Execution{{Price: 149.80, Quantity: 100, Timestamp: closedAt}}, nil
			}
			return nil, nil
		},
	}

	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode:     ReconcileModeStartup,
		LiveMode: config.ModeLiveConfig,
		Closer:   backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		Clock:    func() time.Time { return closedAt },
	}
	sum, _ := r.Run(ctx)
	if sum.Resolved != 1 {
		t.Errorf("Resolved: got %d want 1", sum.Resolved)
	}

	trades, _ := tradeRepo.ListSince(ctx, openedAt, 10)
	if len(trades) != 1 || trades[0].CloseReason != "stop_loss" {
		t.Errorf("expected 1 stop_loss trade; got %+v", trades)
	}
	if trades[0].ExitPrice != 149.80 {
		t.Errorf("ExitPrice: got %v want 149.80 (real SL fill)", trades[0].ExitPrice)
	}
}

// Paper startup retains the legacy blind-close behaviour (no execution feed
// to consult — paper mode has no broker-side TP/SL fills to look up).
func TestReconcile_PaperStartup_StaleDB_UsesSyntheticClose(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	openedAt := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)

	_, _ = posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 150.00,
			StrategyConfigID: "cfg-paper", Status: port.PositionStatusOpen, OpenedAt: openedAt,
		},
		Live: &port.PositionLive{BrokerPositionID: "broker-paper"},
	})

	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
			return nil, nil
		},
	}

	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode:     ReconcileModeStartup,
		LiveMode: config.ModePaperConfig, // paper → legacy synthetic close
		Closer:   backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
	}
	sum, _ := r.Run(ctx)
	if sum.MarkedDone != 1 {
		t.Errorf("Paper startup must still synthetic-close; MarkedDone got %d want 1", sum.MarkedDone)
	}
	if sum.Tripped != 0 {
		t.Errorf("Paper startup synthetic close must NOT trip; got %+v", sum)
	}
}

// Live startup without recorded
// settle legs (so the fill can't be looked up by leg id) must DEFER to the
// runtime reconcile loop rather than synthetic-close at 0 PnL. Runtime's grace
// window + positionId lookup gives the real fill time to resolve; only after
// grace does runtime try the estimate (never a 0-PnL synthetic). The position is gone at broker (no
// active risk), so deferring a few cycles is safe and preserves real PnL.
func TestReconcile_LiveStartup_StaleDB_NoSettleLegs_DefersToRuntime(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()

	id, _ := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 150.00,
			StrategyConfigID: "cfg-live", Status: port.PositionStatusOpen,
			OpenedAt: time.Now(),
		},
		Live: &port.PositionLive{
			BrokerPositionID: "broker-noLegs",
			// TPOrderID / SLOrderID empty
		},
	})
	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) { return nil, nil },
	}
	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode: ReconcileModeStartup, LiveMode: config.ModeLiveConfig,
		Closer: backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
	}
	sum, _ := r.Run(ctx)
	if sum.Deferred != 1 {
		t.Errorf("Deferred: got %d want 1 (defer to runtime grace)", sum.Deferred)
	}
	if sum.Resolved != 0 {
		t.Errorf("Resolved: got %d want 0 (no synthetic close at startup)", sum.Resolved)
	}
	if sum.Tripped != 0 {
		t.Errorf("Tripped: got %d want 0", sum.Tripped)
	}
	if _, err := os.Stat(flagPath); err == nil {
		t.Errorf("emergency_stop flag must NOT exist")
	}
	seen, _ := tradeRepo.ListSince(ctx, time.Now().Add(-time.Hour), 10)
	if len(seen) != 0 {
		t.Errorf("defer must write 0 trade rows; got %d", len(seen))
	}
	_ = id
}

// Build-time sanity probe: the production InMemoryTradeRepo exposes whatever
// listing API the test uses to verify "no trade row inserted". Adding
// (*InMemoryTradeRepo).GetAll if missing is part of this refactor.
var _ = errors.New
