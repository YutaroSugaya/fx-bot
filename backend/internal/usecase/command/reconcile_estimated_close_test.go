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

// estimateStaleClose は「broker が既に閉じた stale ポジの実約定が解決できない」
// 場合に、現在値が OCO の SL/TP 水準を明確に超えていれば その水準で約定したと
// みなして exit/reason を返す純関数。曖昧 (SL と TP の間) なら ok=false で、
// caller は 0-PnL を捏造せず DEFER する (架空利益を作らない)。
func TestEstimateStaleClose(t *testing.T) {
	const pip = 0.01
	cases := []struct {
		name           string
		side           order.Side
		entry, current float64
		tpPips, slPips float64
		wantExit       float64
		wantReason     string
		wantOK         bool
	}{
		{"buy_below_sl", order.SideBuy, 150.00, 149.75, 30, 20, 149.80, "stop_loss", true},
		{"buy_above_tp", order.SideBuy, 150.00, 150.40, 30, 20, 150.30, "take_profit", true},
		{"buy_between_ambiguous", order.SideBuy, 150.00, 150.05, 30, 20, 0, "", false},
		{"sell_above_sl", order.SideSell, 150.00, 150.25, 30, 20, 150.20, "stop_loss", true},
		{"sell_below_tp", order.SideSell, 150.00, 149.60, 30, 20, 149.70, "take_profit", true},
		{"sell_between_ambiguous", order.SideSell, 150.00, 149.95, 30, 20, 0, "", false},
		{"no_sl_level_not_confirmed", order.SideBuy, 150.00, 149.00, 30, 0, 0, "", false},
	}
	for _, c := range cases {
		exit, reason, ok := estimateStaleClose(c.side, c.entry, c.current, c.tpPips, c.slPips, pip)
		if ok != c.wantOK || reason != c.wantReason || (c.wantOK && math.Abs(exit-c.wantExit) > 1e-6) {
			t.Errorf("%s: got (exit=%v reason=%q ok=%v) want (exit=%v reason=%q ok=%v)",
				c.name, exit, reason, ok, c.wantExit, c.wantReason, c.wantOK)
		}
	}
}

// Live RUNTIME で stale DB ポジの実約定が解決できない (GetExecutions 空) が、
// 現在値が SL 水準を下回っている → SL 約定とみなして 実PnL の stop_loss を記録する
// (= 旧挙動の synthetic zero-PnL "reconcile_cold_close" を置換)。再起動と
// broker SL が同時刻に重なっても、損失が 0 円として記録から消えないようにする。
//
// estimate ladder は startup ではなく runtime 側にある (startup は即
// estimate/synthetic せず runtime grace に defer するため)。この
// テストは grace=0 (即 elapsed) の runtime reconcile で estimate が発火し実 SL
// 損を記録することを担保する。startup の defer 挙動は reconcile_c2_test.go 側。
func TestReconcile_LiveRuntime_StaleDB_NoExecution_TickerEstimatesStopLoss(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	openedAt := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	closedAt := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)

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
			// market is below the SL price (149.80) → SL almost certainly fired
			return &market.Ticker{Symbol: "USD_JPY", Bid: 149.74, Ask: 149.76}, nil
		},
	}

	r := &Reconcile{
		Broker: mb, Positions: posRepo, Symbol: "USD_JPY",
		Logger: discardLogger(), EmergencyFlagPath: flagPath,
		Mode: ReconcileModeRuntime, LiveMode: config.ModeLiveConfig,
		Closer: backtest.NewInMemoryPositionCloser(posRepo, tradeRepo),
		Clock:  func() time.Time { return closedAt },
		// StaleGracePeriod unset (0) → grace immediately elapsed, so the runtime
		// estimate ladder runs in this single pass.
	}
	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Resolved != 1 {
		t.Errorf("Resolved: got %d want 1 (estimated close from ticker)", sum.Resolved)
	}
	if sum.Tripped != 0 {
		t.Errorf("Tripped: got %d want 0", sum.Tripped)
	}
	if _, err := os.Stat(flagPath); err == nil {
		t.Errorf("emergency_stop must NOT be tripped")
	}

	trades, _ := tradeRepo.ListSince(ctx, openedAt, 10)
	if len(trades) != 1 {
		t.Fatalf("want 1 trade, got %d", len(trades))
	}
	tr := trades[0]
	if tr.CloseReason != "stop_loss" {
		t.Errorf("CloseReason: got %q want stop_loss (real SL, not reconcile_cold_close)", tr.CloseReason)
	}
	if math.Abs(tr.ExitPrice-149.80) > 1e-6 {
		t.Errorf("ExitPrice: got %v want 149.80 (SL price estimate)", tr.ExitPrice)
	}
	if tr.ProfitLossJPY >= 0 {
		t.Errorf("ProfitLossJPY: got %v want negative (real SL loss must not vanish)", tr.ProfitLossJPY)
	}
}
