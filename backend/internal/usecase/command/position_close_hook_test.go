package command

import (
	"context"
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

// 決済完了フック (event_retrigger 用): ポジションが CLOSED まで確定した
// 全経路 — bot 側 close saga と reconcile の OCO fill 解決 — が OnClosed/
// OnPositionClosed を 1 回呼ぶこと。LLM 再判断のイベント源なので「決済が確定した
// 後」だけ呼ばれ、失敗・スキップでは絶対に呼ばれない。nil は従来挙動 (no-op)。

func TestExecuteCloseSaga_CallsOnClosedAfterSuccess(t *testing.T) {
	pos := backtest.NewInMemoryPositionRepo()
	rec := port.PositionRecord{
		Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 150.0,
		TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240,
		StrategyConfigID: "cfg-test", Status: port.PositionStatusOpen,
		OpenedAt: time.Now().Add(-time.Hour),
	}
	id, err := pos.Insert(context.Background(), port.PositionInsertInput{
		Position: rec,
		Live:     &port.PositionLive{BrokerPositionID: "gmo-pos-1", TPOrderID: "tp-1", SLOrderID: "sl-1"},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	rec.ID = id

	br := &fakeLiveBroker{
		fakeBroker:    fakeBroker{closePosResult: &order.Order{OrderID: "cls-1", Price: 0, Status: "ACCEPTED"}},
		resolvePosID:  "gmo-pos-1",
		resolveFillPx: 150.50,
	}
	var gotSymbol, gotReason string
	calls := 0
	in := CloseSagaInput{
		Mode: config.ModeLiveConfig, Symbol: "USD_JPY",
		Broker: br, Positions: pos, Closer: &fakeCloser{ok: true},
		EmergencyFlagPath: tempFlag(t), Logger: silentLogger(),
		OnClosed: func(symbol, reason string) { calls++; gotSymbol, gotReason = symbol, reason },
	}
	if _, err := ExecuteCloseSaga(context.Background(), in, rec, "max_hold", 0, time.Now()); err != nil {
		t.Fatalf("saga: %v", err)
	}
	if calls != 1 || gotSymbol != "USD_JPY" || gotReason != "max_hold" {
		t.Errorf("OnClosed: calls=%d symbol=%q reason=%q, want 1/USD_JPY/max_hold", calls, gotSymbol, gotReason)
	}
}

// CloseAndRecord が失敗した saga (決済未確定) ではフックを呼ばない。
func TestExecuteCloseSaga_NoOnClosedOnFailure(t *testing.T) {
	pos := backtest.NewInMemoryPositionRepo()
	rec := port.PositionRecord{
		Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 150.0,
		TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240,
		StrategyConfigID: "cfg-test", Status: port.PositionStatusOpen,
		OpenedAt: time.Now().Add(-time.Hour),
	}
	id, err := pos.Insert(context.Background(), port.PositionInsertInput{
		Position: rec,
		Live:     &port.PositionLive{BrokerPositionID: "gmo-pos-1", TPOrderID: "tp-1", SLOrderID: "sl-1"},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	rec.ID = id

	br := &fakeLiveBroker{
		fakeBroker:    fakeBroker{closePosResult: &order.Order{OrderID: "cls-1", Price: 0, Status: "ACCEPTED"}},
		resolvePosID:  "gmo-pos-1",
		resolveFillPx: 150.50,
	}
	calls := 0
	in := CloseSagaInput{
		Mode: config.ModeLiveConfig, Symbol: "USD_JPY",
		Broker: br, Positions: pos, Closer: &fakeCloser{ok: false}, // finalize race → saga error
		EmergencyFlagPath: tempFlag(t), Logger: silentLogger(),
		OnClosed: func(symbol, reason string) { calls++ },
	}
	if _, err := ExecuteCloseSaga(context.Background(), in, rec, "max_hold", 0, time.Now()); err == nil {
		t.Fatal("want saga error (CloseAndRecord ok=false)")
	}
	if calls != 0 {
		t.Errorf("OnClosed calls = %d, want 0 on failure", calls)
	}
}

// Reconcile の OCO fill 解決経路 (live で最も普通の決済) がフックを呼ぶ。
func TestReconcile_ResolvedStaleDB_CallsOnPositionClosed(t *testing.T) {
	ctx := context.Background()
	flagPath := filepath.Join(t.TempDir(), "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	openedAt := time.Date(2026, 7, 10, 3, 0, 0, 0, time.UTC)
	closedAt := time.Date(2026, 7, 10, 5, 0, 0, 0, time.UTC)

	_, _ = posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "SELL", Quantity: 1000, EntryPrice: 159.201,
			TakeProfitPips: 22, StopLossPips: 15, MaxHoldMinutes: 240,
			StrategyConfigID: "cfg-live", Status: port.PositionStatusOpen, OpenedAt: openedAt,
		},
		Live: &port.PositionLive{BrokerPositionID: "1000002"},
	})

	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) { return nil, nil },
		GetLatestExecutionsBySymbolFn: func(_ context.Context, _ string) ([]order.Execution, error) {
			return []order.Execution{{
				PositionID: "1000002", Symbol: "USD_JPY",
				Side: order.SideBuy, Quantity: 1000, Price: 159.351, Timestamp: closedAt,
			}}, nil
		},
	}

	var gotSymbol, gotReason string
	calls := 0
	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode: ReconcileModeStartup, LiveMode: config.ModeLiveConfig,
		Closer:           backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		Clock:            func() time.Time { return closedAt },
		OnPositionClosed: func(symbol, reason string) { calls++; gotSymbol, gotReason = symbol, reason },
	}
	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Resolved != 1 {
		t.Fatalf("Resolved = %d, want 1", sum.Resolved)
	}
	if calls != 1 || gotSymbol != "USD_JPY" || gotReason != "stop_loss" {
		t.Errorf("OnPositionClosed: calls=%d symbol=%q reason=%q, want 1/USD_JPY/stop_loss", calls, gotSymbol, gotReason)
	}
}

// Reconcile の推定 close 経路 (実 fill 未解決・OCO 水準明確超え) も決済確定なので呼ぶ。
func TestReconcile_EstimatedClose_CallsOnPositionClosed(t *testing.T) {
	ctx := context.Background()
	flagPath := filepath.Join(t.TempDir(), "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	openedAt := time.Date(2026, 7, 10, 3, 0, 0, 0, time.UTC)
	closedAt := time.Date(2026, 7, 10, 5, 0, 0, 0, time.UTC)

	_, _ = posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 150.00,
			TakeProfitPips: 30, StopLossPips: 20,
			StrategyConfigID: "cfg-live", Status: port.PositionStatusOpen, OpenedAt: openedAt,
		},
		Live: &port.PositionLive{BrokerPositionID: "broker-NOEXEC", TPOrderID: "tp-x", SLOrderID: "sl-x"},
	})

	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) { return nil, nil },
		GetExecutionsFn:    func(_ context.Context, _ string) ([]order.Execution, error) { return nil, nil },
		GetTickerFn: func(_ context.Context, _ string) (*market.Ticker, error) {
			return &market.Ticker{Symbol: "USD_JPY", Bid: 149.74, Ask: 149.76}, nil // below SL 149.80
		},
	}

	calls := 0
	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode: ReconcileModeRuntime, LiveMode: config.ModeLiveConfig,
		Closer:           backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		Clock:            func() time.Time { return closedAt },
		OnPositionClosed: func(symbol, reason string) { calls++ },
	}
	if _, err := r.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 1 {
		t.Errorf("OnPositionClosed calls = %d, want 1 (estimated close is a settled close)", calls)
	}
}
