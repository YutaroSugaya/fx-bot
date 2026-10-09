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
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
)

// 不具合: Live entry で ResolveSettleLegs が soft-fail し
// positions_live に TP/SL leg id が記録されないまま GMO 側で SL が約定すると、
// reconcile が「leg id 無し → synthetic 0 PnL close」に倒れて、SL の損失が
// 画面上 0 円表示になっていた。
//
// 恒久対策: broker_position_id を持って /v1/latestExecutions を引き、
// positionId 一致 + 反対 side + opened_at 以降の execution から実 exit price
// を復元する。価格が config の TP/SL pips とマッチすれば close_reason を
// take_profit / stop_loss に分類、そうでなければ broker_close。

// 主要シナリオ: SELL @ 159.201 / SL=15pips。GMO 側で 159.351 (= entry + 15pip)
// の SL fill が発生 → leg id 空でも positionId 経由で実 exit を復元すること。
func TestReconcile_LiveStartup_StaleDB_NoSettleLegs_ResolvesViaPositionLookup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	openedAt := time.Date(2026, 5, 27, 3, 10, 13, 0, time.UTC)
	closedAt := time.Date(2026, 5, 27, 5, 30, 0, 0, time.UTC)

	id, _ := posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "SELL", Quantity: 1000,
			EntryPrice:       159.201,
			TakeProfitPips:   22.0,
			StopLossPips:     15.0,
			MaxHoldMinutes:   240,
			StrategyConfigID: "cfg-live",
			Status:           port.PositionStatusOpen,
			OpenedAt:         openedAt,
		},
		Live: &port.PositionLive{
			BrokerPositionID: "1000002",
			// TPOrderID / SLOrderID empty — the actual bug condition.
		},
	})

	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) {
			return nil, nil
		},
		// /v1/latestExecutions 再現: 該当 positionId の close fill + 関係ない fill 1 件。
		GetLatestExecutionsBySymbolFn: func(_ context.Context, _ string) ([]order.Execution, error) {
			return []order.Execution{
				// 関係ない別 position の execution (filter で落ちる)
				{
					PositionID: "9999999", Symbol: "USD_JPY",
					Side: order.SideSell, Quantity: 1000,
					Price: 160.000, Timestamp: closedAt,
				},
				// 本命: positionId 一致、close side=BUY (= SELL の opposite)、opened_at 以降
				{
					PositionID: "1000002", Symbol: "USD_JPY",
					Side: order.SideBuy, Quantity: 1000,
					Price: 159.351, Timestamp: closedAt,
				},
			}, nil
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
	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Resolved != 1 {
		t.Errorf("Resolved: got %d want 1 (positionId lookup must resolve real exit)", sum.Resolved)
	}
	if sum.Tripped != 0 {
		t.Errorf("Tripped: got %d want 0", sum.Tripped)
	}
	if _, err := os.Stat(flagPath); err == nil {
		t.Errorf("emergency_stop flag must NOT exist on successful resolution")
	}

	trades, _ := tradeRepo.ListSince(ctx, openedAt, 10)
	if len(trades) != 1 {
		t.Fatalf("expected 1 trade row, got %d", len(trades))
	}
	tr := trades[0]
	if tr.ExitPrice != 159.351 {
		t.Errorf("ExitPrice: got %v want 159.351 (real SL fill from latestExecutions)", tr.ExitPrice)
	}
	if tr.CloseReason != "stop_loss" {
		t.Errorf("CloseReason: got %q want stop_loss (159.351 matches entry+15pip SL)", tr.CloseReason)
	}
	if tr.ProfitLossJPY >= 0 {
		t.Errorf("ProfitLossJPY: got %v want negative (SELL closed higher = loss)", tr.ProfitLossJPY)
	}
	_ = id
}

// SELL @ 159.201, TP=22pips → TP price=158.981。
// TP 価格で fill された場合は close_reason=take_profit に分類されること。
func TestReconcile_LiveStartup_StaleDB_PositionLookup_TPPriceClassifiesAsTakeProfit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	openedAt := time.Date(2026, 5, 27, 3, 10, 13, 0, time.UTC)
	closedAt := time.Date(2026, 5, 27, 5, 30, 0, 0, time.UTC)

	_, _ = posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "SELL", Quantity: 1000,
			EntryPrice: 159.201, TakeProfitPips: 22.0, StopLossPips: 15.0,
			StrategyConfigID: "cfg", Status: port.PositionStatusOpen, OpenedAt: openedAt,
		},
		Live: &port.PositionLive{BrokerPositionID: "P-TP"},
	})
	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) { return nil, nil },
		GetLatestExecutionsBySymbolFn: func(_ context.Context, _ string) ([]order.Execution, error) {
			return []order.Execution{
				{PositionID: "P-TP", Side: order.SideBuy, Price: 158.981, Quantity: 1000, Timestamp: closedAt},
			}, nil
		},
	}
	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode: ReconcileModeStartup, LiveMode: config.ModeLiveConfig,
		Closer: backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		Clock:  func() time.Time { return closedAt },
	}
	_, _ = r.Run(ctx)
	trades, _ := tradeRepo.ListSince(ctx, openedAt, 10)
	if len(trades) != 1 {
		t.Fatalf("want 1 trade, got %d", len(trades))
	}
	if trades[0].CloseReason != "take_profit" {
		t.Errorf("CloseReason: got %q want take_profit (158.981 matches TP price)", trades[0].CloseReason)
	}
}

// GMO アプリ手動決済など、TP/SL のどちらの価格にもマッチしない外部決済。
// close_reason は broker_close (新規) として記録され、PnL は実価格で計算されること。
func TestReconcile_LiveStartup_StaleDB_PositionLookup_NonTPSLPriceClassifiesAsBrokerClose(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	openedAt := time.Date(2026, 5, 27, 3, 10, 13, 0, time.UTC)
	closedAt := time.Date(2026, 5, 27, 5, 30, 0, 0, time.UTC)

	_, _ = posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "SELL", Quantity: 1000,
			EntryPrice: 159.201, TakeProfitPips: 22.0, StopLossPips: 15.0,
			StrategyConfigID: "cfg", Status: port.PositionStatusOpen, OpenedAt: openedAt,
		},
		Live: &port.PositionLive{BrokerPositionID: "P-MANUAL"},
	})
	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) { return nil, nil },
		GetLatestExecutionsBySymbolFn: func(_ context.Context, _ string) ([]order.Execution, error) {
			return []order.Execution{
				// 159.100: SELL entry 159.201 から見て +9.9 pips → TP(158.981)/SL(159.351) どちらでもない
				{PositionID: "P-MANUAL", Side: order.SideBuy, Price: 159.100, Quantity: 1000, Timestamp: closedAt},
			}, nil
		},
	}
	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode: ReconcileModeStartup, LiveMode: config.ModeLiveConfig,
		Closer: backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		Clock:  func() time.Time { return closedAt },
	}
	_, _ = r.Run(ctx)
	trades, _ := tradeRepo.ListSince(ctx, openedAt, 10)
	if len(trades) != 1 {
		t.Fatalf("want 1 trade, got %d", len(trades))
	}
	if trades[0].ExitPrice != 159.100 {
		t.Errorf("ExitPrice: got %v want 159.100", trades[0].ExitPrice)
	}
	if trades[0].CloseReason != "broker_close" {
		t.Errorf("CloseReason: got %q want broker_close (price matches neither TP nor SL)", trades[0].CloseReason)
	}
}

// latestExecutions が positionId 一致を返さない場合 (= API 反映遅延 / 取得失敗)、
// startup は即 synthetic せず runtime reconcile に DEFER する。
// API 反映遅延こそ runtime grace で待つべきケースなので、premature 0-PnL を避ける。
func TestReconcile_LiveStartup_StaleDB_PositionLookup_NoMatch_DefersToRuntime(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	openedAt := time.Date(2026, 5, 27, 3, 10, 13, 0, time.UTC)

	_, _ = posRepo.Insert(ctx, port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "SELL", Quantity: 1000,
			EntryPrice: 159.201, TakeProfitPips: 22, StopLossPips: 15,
			StrategyConfigID: "cfg", Status: port.PositionStatusOpen, OpenedAt: openedAt,
		},
		Live: &port.PositionLive{BrokerPositionID: "P-NOMATCH"},
	})
	mb := &broker.MockBroker{
		GetOpenPositionsFn: func(_ context.Context, _ string) ([]position.Position, error) { return nil, nil },
		GetLatestExecutionsBySymbolFn: func(_ context.Context, _ string) ([]order.Execution, error) {
			// 別 positionId だけ返ってきて、対象 positionId は無い (古い execution が落ちた状態)
			return []order.Execution{
				{PositionID: "OTHER", Side: order.SideBuy, Price: 160.0, Quantity: 1000, Timestamp: openedAt.Add(time.Hour)},
			}, nil
		},
	}
	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode: ReconcileModeStartup, LiveMode: config.ModeLiveConfig,
		Closer: backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
	}
	sum, _ := r.Run(ctx)
	if sum.Deferred != 1 {
		t.Errorf("Deferred: got %d want 1 (defer to runtime grace, not synthetic)", sum.Deferred)
	}
	if sum.Resolved != 0 {
		t.Errorf("Resolved: got %d want 0 (no synthetic at startup)", sum.Resolved)
	}
	trades, _ := tradeRepo.ListSince(ctx, openedAt, 10)
	if len(trades) != 0 {
		t.Fatalf("defer must write 0 trade rows; got %d", len(trades))
	}
}
