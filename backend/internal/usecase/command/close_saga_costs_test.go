package command

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/port"
)

// close saga が ResolveExecution の
// fee / settledSwap を捨てずに trades 行へ運ぶことを検証する。
//
// 仕様 (migration 0007 コメントと一致):
//   - trades.fee_jpy = 往復 = entry leg (positions.entry_fee_jpy) + close leg (実報告)
//   - trades.swap_jpy = close fill の settledSwap
//   - fee_estimated = false (両 leg とも broker 実報告) / true (いずれか推定)
//   - profit_loss_jpy は GROSS のまま (fee/swap を混ぜない)
func TestExecuteCloseSaga_Live_RecordsBrokerCostsOnTrade(t *testing.T) {
	pos := backtest.NewInMemoryPositionRepo()
	entryFee := 3.7
	rec := port.PositionRecord{
		Symbol:           "USD_JPY",
		Side:             "BUY",
		Quantity:         1000,
		EntryPrice:       150.0,
		TakeProfitPips:   20,
		StopLossPips:     15,
		MaxHoldMinutes:   240,
		EntryFeeJPY:      &entryFee,
		StrategyConfigID: "cfg-test",
		Status:           port.PositionStatusOpen,
		OpenedAt:         time.Now().Add(-time.Hour),
	}
	id, err := pos.Insert(context.Background(), port.PositionInsertInput{
		Position: rec,
		Live: &port.PositionLive{
			BrokerPositionID: "gmo-pos-1",
			TPOrderID:        "tp-1",
			SLOrderID:        "sl-1",
		},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	rec.ID = id

	br := &fakeLiveBroker{
		fakeBroker:     fakeBroker{closePosResult: &order.Order{OrderID: "cls-1", Price: 0, Status: "ACCEPTED"}},
		resolvePosID:   "gmo-pos-1",
		resolveFillPx:  150.50,
		resolveFeeJPY:  3.8,
		resolveSwapJPY: -12.0,
	}
	closer := &fakeCloser{ok: true}
	in := CloseSagaInput{
		Mode: config.ModeLiveConfig, Symbol: "USD_JPY",
		Broker: br, Positions: pos, Closer: closer,
		EmergencyFlagPath: tempFlag(t), Logger: silentLogger(),
	}
	if _, err := ExecuteCloseSaga(context.Background(), in, rec, "take_profit", 0, time.Now()); err != nil {
		t.Fatalf("saga: %v", err)
	}

	tr := closer.gotTrade
	if !nearly(tr.FeeJPY, 3.7+3.8) {
		t.Errorf("FeeJPY: got %v want %v (entry leg + close leg)", tr.FeeJPY, 3.7+3.8)
	}
	if !nearly(tr.SwapJPY, -12.0) {
		t.Errorf("SwapJPY: got %v want -12.0", tr.SwapJPY)
	}
	if tr.FeeEstimated {
		t.Errorf("FeeEstimated: got true want false (both legs broker-reported)")
	}
	// gross 不変条件: profit_loss_jpy に fee/swap を混ぜない。
	// BUY 150.0→150.5 = +50pips × 1000通貨 × 0.01 = +500円 (gross)。
	if !nearly(tr.ProfitLossJPY, 500.0) {
		t.Errorf("ProfitLossJPY must stay GROSS: got %v want 500.0", tr.ProfitLossJPY)
	}
}

// entry fee 未捕捉 (migration 0008 以前の建玉 = EntryFeeJPY nil) の close は、
// entry leg を 0.002% で推定して合成し fee_estimated=true を立てる。
func TestExecuteCloseSaga_Live_EntryFeeUnknown_EstimatesAndFlags(t *testing.T) {
	pos := backtest.NewInMemoryPositionRepo()
	rec := port.PositionRecord{
		Symbol:           "USD_JPY",
		Side:             "BUY",
		Quantity:         1000,
		EntryPrice:       150.0,
		TakeProfitPips:   20,
		StopLossPips:     15,
		MaxHoldMinutes:   240,
		EntryFeeJPY:      nil, // 0008 以前の行
		StrategyConfigID: "cfg-test",
		Status:           port.PositionStatusOpen,
		OpenedAt:         time.Now().Add(-time.Hour),
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
		resolveFeeJPY: 3.8,
	}
	closer := &fakeCloser{ok: true}
	in := CloseSagaInput{
		Mode: config.ModeLiveConfig, Symbol: "USD_JPY",
		Broker: br, Positions: pos, Closer: closer,
		EmergencyFlagPath: tempFlag(t), Logger: silentLogger(),
	}
	if _, err := ExecuteCloseSaga(context.Background(), in, rec, "take_profit", 0, time.Now()); err != nil {
		t.Fatalf("saga: %v", err)
	}

	tr := closer.gotTrade
	// entry leg 推定 = 150.0 × 1000 × 0.002% = 3.0 円 (JPY-quote なので rate 1.0)
	if !nearly(tr.FeeJPY, 3.0+3.8) {
		t.Errorf("FeeJPY: got %v want %v (estimated entry 3.0 + reported close 3.8)", tr.FeeJPY, 3.0+3.8)
	}
	if !tr.FeeEstimated {
		t.Errorf("FeeEstimated: got false want true (entry leg estimated)")
	}
}

// Paper close は broker 実手数料が存在しないが、Live との PnL 乖離を縮める
// ため往復手数料を 0.002%/leg (gmoFeeRatePerLeg) で推定計上する
// (paper の net/件 が Live と同じコスト床を持つように)。fee_estimated=true で「推定」を可視化。swap は
// session_flatten 運用でロール跨ぎが無い前提のため 0 のまま。
// profit_loss_jpy の GROSS 不変条件は Live と共通。
func TestExecuteCloseSaga_Paper_EstimatesRoundTripFee(t *testing.T) {
	pos := backtest.NewInMemoryPositionRepo()
	rec := openPosition(t, pos, "BUY", 100.0)
	br := &fakeBroker{closePosResult: &order.Order{OrderID: "cls-1", Price: 101.0, Status: "FILLED"}}
	closer := &fakeCloser{ok: true}
	in := CloseSagaInput{
		Mode: config.ModePaperConfig, Symbol: "USD_JPY",
		Broker: br, Positions: pos, Closer: closer,
		EmergencyFlagPath: tempFlag(t), Logger: silentLogger(),
	}
	if _, err := ExecuteCloseSaga(context.Background(), in, rec, "take_profit", 0, time.Now()); err != nil {
		t.Fatalf("saga: %v", err)
	}
	tr := closer.gotTrade
	// entry leg 100.0×100×0.002% = 0.2 / close leg 101.0×100×0.002% = 0.202
	if !nearly(tr.FeeJPY, 0.2+0.202) {
		t.Errorf("FeeJPY: got %v want %v (estimated entry leg + close leg)", tr.FeeJPY, 0.2+0.202)
	}
	if !tr.FeeEstimated {
		t.Errorf("FeeEstimated: got false want true (paper fee is always an estimate)")
	}
	if tr.SwapJPY != 0 {
		t.Errorf("SwapJPY: got %v want 0 (paper does not model swap)", tr.SwapJPY)
	}
	// gross 不変条件: BUY 100.0→101.0 × 100通貨 = +100円 (fee を混ぜない)。
	if !nearly(tr.ProfitLossJPY, 100.0) {
		t.Errorf("ProfitLossJPY must stay GROSS: got %v want 100.0", tr.ProfitLossJPY)
	}
}
