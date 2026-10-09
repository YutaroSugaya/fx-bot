package backtest

import (
	"math"
	"testing"
	"time"

	"fx-bot/backend/internal/domain/order"
)

// GMO charges 約定金額 × 0.002% per side. A fixed JPY/trade -fee flag can't
// scale with qty/price. FeeRatePct
// models the real structure: roundtrip fee = (entry + exit notional) × rate%.
// For USD_JPY @156.30 × 1000 units that is ≈6.25 JPY ≈0.7pips — enough to raise
// the breakeven from ≈1pip (spread only) to ≈1.7pips.
func TestMakeTrade_FeeRateRoundtrip(t *testing.T) {
	p := openPosition{side: order.SideBuy, entryPrice: 156.30, quantity: 1000, openedAt: time.Unix(0, 0)}
	costs := CostModel{FeeRatePct: 0.002, quoteJPYRate: 1.0}
	// Exit flat so price PnL = 0 and the fee is isolated.
	tr := makeTrade(p, 156.30, time.Unix(60, 0), "x", 0.01, costs)
	want := -(156.30*1000 + 156.30*1000) * 0.002 / 100.0 // = -6.252 JPY
	if math.Abs(tr.ProfitLossJPY-want) > 0.001 {
		t.Errorf("fee-rate roundtrip PnL = %.4f, want %.4f", tr.ProfitLossJPY, want)
	}
}

func TestMakeTrade_FeeRateZeroIsNoOp(t *testing.T) {
	p := openPosition{side: order.SideBuy, entryPrice: 156.30, quantity: 1000, openedAt: time.Unix(0, 0)}
	tr := makeTrade(p, 156.30, time.Unix(60, 0), "x", 0.01, CostModel{quoteJPYRate: 1.0})
	if tr.ProfitLossJPY != 0 {
		t.Errorf("no fee → flat trade PnL should be 0; got %v", tr.ProfitLossJPY)
	}
}
