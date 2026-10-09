package command

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
)

// live entry が
//   - entry_fee_jpy  (ResolveExecution の broker 実報告手数料)
//   - entry_spread_pips (発注直前 ticker の実測スプレッド)
//   - entry_slippage_pips (実 fill − 意図価格)
//
// を positions 行へ凍結保存することを検証する。
func TestExecuteOrder_Live_PersistsEntryCosts(t *testing.T) {
	repo := backtest.NewInMemoryPositionRepo()
	br := &fakeLiveBroker{
		fakeBroker: fakeBroker{
			placeOrderResult: &order.Order{OrderID: "ord-1", Price: 0, Status: "ACCEPTED"},
		},
		resolvePosID:  "987654",
		resolveFillPx: 150.135, // ask 150.13 より 0.5pip 不利
		resolveFeeJPY: 3.7,
	}
	u := NewExecuteOrder(br, repo, config.ModeLiveConfig, "USD_JPY", silentLogger())
	u.EmergencyFlagPath = tempFlag(t)
	u.Clock = func() time.Time { return time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC) }

	sig := strategy.Signal{
		Decision:       strategy.DecisionEnter,
		Side:           order.SideBuy,
		TakeProfitPips: 20,
		StopLossPips:   10,
		MaxHoldMinutes: 60,
		Quantity:       1000,
		ConfigID:       "cfg-test",
	}
	tk := &market.Ticker{Symbol: "USD_JPY", Bid: 150.12, Ask: 150.13}
	if err := u.OnSignal(context.Background(), sig, tk); err != nil {
		t.Fatalf("OnSignal: %v", err)
	}

	open, _ := repo.ListOpenOrClosing(context.Background(), "USD_JPY")
	if len(open) != 1 {
		t.Fatalf("want 1 open position, got %d", len(open))
	}
	p := open[0]
	if p.EntryFeeJPY == nil || !nearly(*p.EntryFeeJPY, 3.7) {
		t.Errorf("EntryFeeJPY: got %v want 3.7", deref(p.EntryFeeJPY))
	}
	if p.EntrySpreadPips == nil || !nearly(*p.EntrySpreadPips, 1.0) {
		t.Errorf("EntrySpreadPips: got %v want 1.0", deref(p.EntrySpreadPips))
	}
	if p.EntrySlippagePips == nil || !nearly(*p.EntrySlippagePips, 0.5) {
		t.Errorf("EntrySlippagePips: got %v want +0.5", deref(p.EntrySlippagePips))
	}
}

// Paper entry: broker 手数料は存在しない → entry_fee_jpy は NULL (未捕捉) のまま、
// spread / slippage は ticker から捕捉される。
func TestExecuteOrder_Paper_EntryFeeStaysNull(t *testing.T) {
	repo := backtest.NewInMemoryPositionRepo()
	br := &fakeBroker{
		placeOrderResult: &order.Order{OrderID: "ord-1", Price: 150.13, Status: "FILLED"},
	}
	u := NewExecuteOrder(br, repo, config.ModePaperConfig, "USD_JPY", silentLogger())

	sig := strategy.Signal{
		Decision: strategy.DecisionEnter, Side: order.SideBuy,
		TakeProfitPips: 20, StopLossPips: 10, MaxHoldMinutes: 60,
		Quantity: 1000, ConfigID: "cfg-test",
	}
	tk := &market.Ticker{Symbol: "USD_JPY", Bid: 150.12, Ask: 150.13}
	if err := u.OnSignal(context.Background(), sig, tk); err != nil {
		t.Fatalf("OnSignal: %v", err)
	}
	open, _ := repo.ListOpenOrClosing(context.Background(), "USD_JPY")
	if len(open) != 1 {
		t.Fatalf("want 1 open position, got %d", len(open))
	}
	p := open[0]
	if p.EntryFeeJPY != nil {
		t.Errorf("paper EntryFeeJPY must stay nil (no broker fee); got %v", *p.EntryFeeJPY)
	}
	if p.EntrySpreadPips == nil || !nearly(*p.EntrySpreadPips, 1.0) {
		t.Errorf("EntrySpreadPips: got %v want 1.0", deref(p.EntrySpreadPips))
	}
	var _ port.PositionRecord = p
}
