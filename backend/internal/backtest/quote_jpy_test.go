package backtest

import (
	"math"
	"testing"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
)

// makeTrade must convert USD-quote (EUR_USD) PnL to JPY via the resolved
// quoteJPYRate, while leaving JPY-quote PnL untouched (factor 1.0).
func TestMakeTrade_QuoteJPYConversion(t *testing.T) {
	const usdjpy = 157.0

	// JPY-quote: pip=0.01, +10 pips on 1000 units = 100 JPY. quoteJPYRate=1.0.
	t.Run("JPY quote unchanged", func(t *testing.T) {
		p := openPosition{side: order.SideBuy, entryPrice: 150.000, quantity: 1000}
		pip := market.PipSize("USD_JPY")
		tr := makeTrade(p, 150.100, p.openedAt, "take_profit", pip, CostModel{quoteJPYRate: 1.0})
		if math.Abs(tr.ProfitLossPips-10) > 1e-6 {
			t.Errorf("pips = %v, want 10", tr.ProfitLossPips)
		}
		if math.Abs(tr.ProfitLossJPY-100) > 1e-6 {
			t.Errorf("jpy = %v, want 100", tr.ProfitLossJPY)
		}
	})

	// USD-quote: pip=0.0001, +10 pips on 1000 units = 1.0 USD → ×157 = 157 JPY.
	t.Run("USD quote converted via rate", func(t *testing.T) {
		mul, err := market.QuoteJPYRate("EUR_USD", usdjpy)
		if err != nil {
			t.Fatalf("QuoteJPYRate: %v", err)
		}
		p := openPosition{side: order.SideBuy, entryPrice: 1.08000, quantity: 1000}
		pip := market.PipSize("EUR_USD")
		tr := makeTrade(p, 1.08100, p.openedAt, "take_profit", pip, CostModel{quoteJPYRate: mul})
		if math.Abs(tr.ProfitLossPips-10) > 1e-6 {
			t.Errorf("pips = %v, want 10", tr.ProfitLossPips)
		}
		if math.Abs(tr.ProfitLossJPY-157.0) > 1e-6 {
			t.Errorf("jpy = %v, want 157 (1.0 USD × %.0f)", tr.ProfitLossJPY, usdjpy)
		}
	})

	// Defensive: unset quoteJPYRate (0) falls back to 1.0 (PnL in quote units).
	t.Run("unset rate falls back to 1.0", func(t *testing.T) {
		p := openPosition{side: order.SideBuy, entryPrice: 1.08000, quantity: 1000}
		pip := market.PipSize("EUR_USD")
		tr := makeTrade(p, 1.08100, p.openedAt, "take_profit", pip, CostModel{})
		if math.Abs(tr.ProfitLossJPY-1.0) > 1e-6 { // 1.0 USD left unconverted
			t.Errorf("jpy = %v, want 1.0 (quote units, no rate)", tr.ProfitLossJPY)
		}
	})
}
