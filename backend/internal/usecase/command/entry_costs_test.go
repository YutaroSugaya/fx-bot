package command

import (
	"testing"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
)

// entry 時点の spread / slippage 捕捉の純関数仕様。
func TestEntryCostSnapshot(t *testing.T) {
	tk := &market.Ticker{Symbol: "USD_JPY", Bid: 150.00, Ask: 150.01}

	t.Run("BUY adverse fill above ask is positive slippage", func(t *testing.T) {
		spread, slip := entryCostSnapshot(order.SideBuy, tk, 150.015, 0.01)
		if spread == nil || !nearly(*spread, 1.0) {
			t.Fatalf("spread: got %v want 1.0", deref(spread))
		}
		if slip == nil || !nearly(*slip, 0.5) {
			t.Fatalf("slippage: got %v want +0.5 (filled 0.5pip above ask)", deref(slip))
		}
	})

	t.Run("SELL adverse fill below bid is positive slippage", func(t *testing.T) {
		_, slip := entryCostSnapshot(order.SideSell, tk, 149.995, 0.01)
		if slip == nil || !nearly(*slip, 0.5) {
			t.Fatalf("slippage: got %v want +0.5 (filled 0.5pip below bid)", deref(slip))
		}
	})

	t.Run("favourable fill is negative slippage", func(t *testing.T) {
		_, slip := entryCostSnapshot(order.SideBuy, tk, 150.005, 0.01)
		if slip == nil || !nearly(*slip, -0.5) {
			t.Fatalf("slippage: got %v want -0.5 (price improvement)", deref(slip))
		}
	})

	t.Run("nil ticker means not-captured (NULL), not zero", func(t *testing.T) {
		spread, slip := entryCostSnapshot(order.SideBuy, nil, 150.0, 0.01)
		if spread != nil || slip != nil {
			t.Fatalf("want nil/nil for missing ticker; got %v/%v", deref(spread), deref(slip))
		}
	})

	t.Run("zero fill price captures spread only", func(t *testing.T) {
		spread, slip := entryCostSnapshot(order.SideBuy, tk, 0, 0.01)
		if spread == nil || slip != nil {
			t.Fatalf("want spread!=nil, slip==nil; got %v/%v", deref(spread), deref(slip))
		}
	})
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
