package position

import (
	"testing"

	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/testutil"
)

func TestComputeTPSLPrices(t *testing.T) {
	cases := []struct {
		name               string
		side               order.Side
		entry, tp, sl, pip float64
		wantTP, wantSL     float64
	}{
		{"buy", order.SideBuy, 100.00, 20, 15, 0.01, 100.20, 99.85},
		{"sell mirrors buy", order.SideSell, 100.00, 20, 15, 0.01, 99.80, 100.15},
		{"zero pips returns entry", order.SideBuy, 100.00, 0, 0, 0.01, 100.00, 100.00},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tp, sl := ComputeTPSLPrices(tc.side, tc.entry, tc.tp, tc.sl, tc.pip)
			if !testutil.Nearly(tp, tc.wantTP, 1e-9) {
				t.Errorf("TP: got %v want %v", tp, tc.wantTP)
			}
			if !testutil.Nearly(sl, tc.wantSL, 1e-9) {
				t.Errorf("SL: got %v want %v", sl, tc.wantSL)
			}
		})
	}
}
