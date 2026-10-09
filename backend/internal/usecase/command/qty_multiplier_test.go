package command

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/port"
)

// seedAlternatingSLs は同日の SL を BUY/SELL 1 つずつ seed する。
// 連敗系 cooldown のテストで同方向 SL 2 回の direction block を
// 同時に踏まないようにする (consec=2 だが各 side は 1 SL なので block はかからない)。
func seedAlternatingSLs(t *testing.T, repo *backtest.InMemoryTradeRepo, now time.Time) {
	t.Helper()
	specs := []struct {
		side string
		ago  time.Duration
	}{
		{"BUY", 30 * time.Minute},
		{"SELL", 10 * time.Minute},
	}
	for i, s := range specs {
		if err := repo.Insert(context.Background(), port.TradeRecord{
			Symbol: "USD_JPY", Side: s.side, ProfitLossJPY: -50,
			CloseReason: "stop_loss",
			OpenedAt:    now.Add(-s.ago - 5*time.Minute),
			ClosedAt:    now.Add(-s.ago),
		}); err != nil {
			t.Fatalf("seed trade %d (%s): %v", i, s.side, err)
		}
	}
}

// applyQtyMultiplier scales sig.Quantity by the risk gate's
// QtyMultiplier, with a broker-floor that prevents the halved value from
// dropping below MinQuantity (= GMO 1000 通貨). At qty=1000 the floor makes
// halving a no-op; at qty>=2000 it bites.
func TestApplyQtyMultiplier(t *testing.T) {
	cases := []struct {
		name    string
		sigQty  int
		mul     float64
		minQty  int
		wantQty int
	}{
		{"no_multiplier_zero_passthrough", 3000, 0, 1000, 3000},
		{"no_multiplier_one_passthrough", 3000, 1.0, 1000, 3000},
		{"halve_above_floor", 3000, 0.5, 1000, 1500},
		{"halve_below_floor_keeps_original", 1000, 0.5, 1000, 1000},
		{"halve_exactly_at_floor", 2000, 0.5, 1000, 1000},
		{"no_floor_when_min_zero", 1000, 0.5, 0, 500},
		{"negative_multiplier_passthrough", 1000, -0.1, 1000, 1000},
		{"multiplier_above_one_passthrough", 1000, 1.5, 1000, 1000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := applyQtyMultiplier(c.sigQty, c.mul, c.minQty)
			if got != c.wantQty {
				t.Errorf("applyQtyMultiplier(%d, %v, %d) = %d, want %d",
					c.sigQty, c.mul, c.minQty, got, c.wantQty)
			}
		})
	}
}
