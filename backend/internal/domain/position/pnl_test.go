package position

import (
	"math"
	"testing"

	"fx-bot/backend/internal/domain/order"
)

// ComputeClosePnL は reconcile.go / close_saga.go などで重複していた PnL 計算 (entry/exit/side/qty/symbol → pips,jpy) を
// domain 層に集約したもの。
//
// 仕様:
//   - BUY  → diff = exit - entry, pips = diff/pip,  jpy = diff*qty*quoteJPYRate
//   - SELL → diff = exit - entry, pips = -diff/pip, jpy = -diff*qty*quoteJPYRate
//   - quoteJPYRate は quote 通貨→JPY 換算倍率 (USD_JPY 等 JPY quote は 1.0、
//     EUR_USD 等 USD quote は現在の USD/JPY)。market.QuoteJPYRate で解決する。
//   - 未知 symbol (pip=0) → pips=0, jpy は通常計算で可
//     (旧 reconcile.go の挙動と一致させ pnlPips=0, pnlJPY=diff*qty*dir*rate で返す)
//   - Side が BUY/SELL 以外 → pips=0, jpy=0, error
func TestComputeClosePnL_BuyAndSell(t *testing.T) {
	tests := []struct {
		name         string
		entry        float64
		exit         float64
		side         order.Side
		qty          int
		symbol       string
		quoteJPYRate float64
		wantPips     float64
		wantJPY      float64
		wantError    bool
	}{
		{
			name:  "BUY 100 lots, +10 pips on USD_JPY",
			entry: 150.000, exit: 150.100, side: order.SideBuy, qty: 100, symbol: "USD_JPY", quoteJPYRate: 1.0,
			wantPips: 10.0, wantJPY: 10.0, // 0.100 * 100 = 10
		},
		{
			name:  "BUY 1000 lots, -5 pips on USD_JPY",
			entry: 150.000, exit: 149.950, side: order.SideBuy, qty: 1000, symbol: "USD_JPY", quoteJPYRate: 1.0,
			wantPips: -5.0, wantJPY: -50.0, // -0.050 * 1000 = -50
		},
		{
			name:  "SELL 100 lots, +20 pips on USD_JPY (price dropped)",
			entry: 150.000, exit: 149.800, side: order.SideSell, qty: 100, symbol: "USD_JPY", quoteJPYRate: 1.0,
			wantPips: 20.0, wantJPY: 20.0, // -(-0.200)*100 = 20
		},
		{
			name:  "SELL 1000 lots, -10 pips on USD_JPY (price rose)",
			entry: 150.000, exit: 150.100, side: order.SideSell, qty: 1000, symbol: "USD_JPY", quoteJPYRate: 1.0,
			wantPips: -10.0, wantJPY: -100.0,
		},
		{
			name:  "flat (zero diff)",
			entry: 150.000, exit: 150.000, side: order.SideBuy, qty: 100, symbol: "USD_JPY", quoteJPYRate: 1.0,
			wantPips: 0, wantJPY: 0,
		},
		{
			// EUR_USD: pip=0.0001。diff=0.0010 = +10 pips。
			// 損益[USD] = 0.0010*100 = 0.1 USD → × USD/JPY 157.5 = 15.75 JPY。
			name:  "BUY 100 lots, +10 pips on EUR_USD with USD/JPY=157.5",
			entry: 1.08000, exit: 1.08100, side: order.SideBuy, qty: 100, symbol: "EUR_USD", quoteJPYRate: 157.5,
			wantPips: 10.0, wantJPY: 15.75,
		},
		{
			// SELL EUR_USD 価格上昇 = 損: diff=0.0010, -10 pips。
			// -0.0010*1000 = -1.0 USD → × 157.5 = -157.5 JPY。
			name:  "SELL 1000 lots, -10 pips on EUR_USD with USD/JPY=157.5",
			entry: 1.08000, exit: 1.08100, side: order.SideSell, qty: 1000, symbol: "EUR_USD", quoteJPYRate: 157.5,
			wantPips: -10.0, wantJPY: -157.5,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pips, jpy, err := ComputeClosePnL(tc.entry, tc.exit, tc.side, tc.qty, tc.symbol, tc.quoteJPYRate)
			if (err != nil) != tc.wantError {
				t.Fatalf("err=%v wantError=%v", err, tc.wantError)
			}
			if math.Abs(pips-tc.wantPips) > 1e-9 {
				t.Errorf("pips: got %v want %v", pips, tc.wantPips)
			}
			if math.Abs(jpy-tc.wantJPY) > 1e-9 {
				t.Errorf("jpy: got %v want %v", jpy, tc.wantJPY)
			}
		})
	}
}

func TestComputeClosePnL_UnknownSymbolFallsBackToDefaultPip(t *testing.T) {
	// market.PipSize は未知 symbol に対して 0.0001 (非 JPY 系既定) を返す。
	// pip=0 ガードは defensive で実呼び出しでは発火しないが、jpy 計算は
	// dir 正しく続行されることを確認する。
	pips, jpy, err := ComputeClosePnL(150.0, 150.1, order.SideBuy, 100, "UNKNOWN_XX", 1.0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 0.1 / 0.0001 = 1000 pips (4-decimal pair 想定の fallback)
	if math.Abs(pips-1000) > 1e-6 {
		t.Errorf("unknown symbol pips: got %v want 1000 (default pip=0.0001)", pips)
	}
	if math.Abs(jpy-10.0) > 1e-9 {
		t.Errorf("unknown symbol jpy: got %v want 10", jpy)
	}
}

func TestComputeClosePnL_InvalidSideReturnsError(t *testing.T) {
	tests := []struct {
		name string
		side order.Side
	}{
		{"empty side", order.Side("")},
		{"garbage", order.Side("FOO")},
		{"lowercase buy", order.Side("buy")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pips, jpy, err := ComputeClosePnL(150.0, 150.1, tc.side, 100, "USD_JPY", 1.0)
			if err == nil {
				t.Fatalf("expected error for side=%q, got pips=%v jpy=%v", tc.side, pips, jpy)
			}
			if pips != 0 || jpy != 0 {
				t.Errorf("invalid side should return zeroes; got pips=%v jpy=%v", pips, jpy)
			}
		})
	}
}

func TestComputeClosePnL_ZeroQuantityReturnsZero(t *testing.T) {
	// qty=0 (defensive) → jpy=0、pips は計算可能 (diff/pip)
	pips, jpy, err := ComputeClosePnL(150.0, 150.1, order.SideBuy, 0, "USD_JPY", 1.0)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if math.Abs(pips-10.0) > 1e-9 {
		t.Errorf("pips: got %v want 10", pips)
	}
	if jpy != 0 {
		t.Errorf("jpy with qty=0: got %v want 0", jpy)
	}
}
