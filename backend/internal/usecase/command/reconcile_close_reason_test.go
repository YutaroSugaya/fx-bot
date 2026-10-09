package command

import (
	"testing"

	"fx-bot/backend/internal/domain/order"
)

// 例: EUR_JPY BUY entry=185.21 / SL=10pips → SL価格 185.11。
// GMO の OCO SL が 0.5pip 滑って 185.105 で fill すると、旧実装の
// 「±0.5pip 以内で一致」判定が float64 境界で落ち (diff=0.005000000000023874 > 0.005)、
// close_reason が broker_close に誤分類される。
//
// 恒久対策: 閾値の微調整ではなく OCO の執行原理で方向付き分類する。
//   - SL は逆指値 (トリガー後は成行) → SL 価格より「不利側」の fill は全て stop_loss
//     (週末 gap で大きく滑っても SL fill は SL)。
//   - TP は指値 → TP 価格より「有利側」の fill は全て take_profit。
//   - OCO が生きている限り、TP より有利 / SL より不利な価格で手動決済は成立しない
//     (先に OCO が約定してポジションが消える) ので、無制限側は安全。
//   - 中間帯 (TP と SL の間) だけが broker_close (GMO アプリ手動決済など)。
//   - 境界の float 誤差は従来どおり 0.5pip の許容を「有利側」に残して吸収する。
func TestClassifyCloseReason(t *testing.T) {
	cases := []struct {
		name   string
		side   order.Side
		symbol string
		entry  float64
		exit   float64
		tpPips float64
		slPips float64
		want   string
	}{
		// --- float 境界の再現 (BUY / SL10 / 0.5pip 滑り) ---
		{"BUY SL fill slipped 0.5pip (float boundary)", order.SideBuy, "EUR_JPY", 185.21, 185.105, 30, 10, "stop_loss"},
		// BUY: SL ぴったり / 大きく滑った SL (gap 相当) / SL より僅かに有利側 (許容内)
		{"BUY SL exact", order.SideBuy, "EUR_JPY", 185.21, 185.11, 30, 10, "stop_loss"},
		{"BUY SL gap slip 6pips", order.SideBuy, "EUR_JPY", 185.21, 185.05, 30, 10, "stop_loss"},
		{"BUY SL favorable within tol", order.SideBuy, "EUR_JPY", 185.21, 185.114, 30, 10, "stop_loss"},
		// BUY: TP ぴったり / TP より有利 / TP より僅かに不利 (許容内)
		{"BUY TP exact", order.SideBuy, "EUR_JPY", 185.21, 185.51, 30, 10, "take_profit"},
		{"BUY TP better fill", order.SideBuy, "EUR_JPY", 185.21, 185.53, 30, 10, "take_profit"},
		{"BUY TP worse within tol", order.SideBuy, "EUR_JPY", 185.21, 185.506, 30, 10, "take_profit"},
		// BUY: 中間帯 = 手動
		{"BUY manual mid-range", order.SideBuy, "EUR_JPY", 185.21, 185.15, 30, 10, "broker_close"},
		{"BUY manual 1pip above SL", order.SideBuy, "EUR_JPY", 185.21, 185.12, 30, 10, "broker_close"},
		// --- SELL 側 (既存主要シナリオ: entry 159.201 / TP22 / SL15) ---
		{"SELL SL exact", order.SideSell, "USD_JPY", 159.201, 159.351, 22, 15, "stop_loss"},
		{"SELL SL slipped worse", order.SideSell, "USD_JPY", 159.201, 159.40, 22, 15, "stop_loss"},
		{"SELL TP exact", order.SideSell, "USD_JPY", 159.201, 158.981, 22, 15, "take_profit"},
		{"SELL TP better fill", order.SideSell, "USD_JPY", 159.201, 158.95, 22, 15, "take_profit"},
		{"SELL manual mid-range", order.SideSell, "USD_JPY", 159.201, 159.25, 22, 15, "broker_close"},
		// 非円ペア (pip=0.0001)
		{"SELL non-JPY SL slipped", order.SideSell, "GBP_USD", 1.33771, 1.33872, 30, 10, "stop_loss"},
		// TP/SL 未設定は分類しない
		{"no TP/SL set", order.SideBuy, "EUR_JPY", 185.21, 185.05, 0, 0, "broker_close"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCloseReason(tc.side, tc.entry, tc.exit, tc.tpPips, tc.slPips, tc.symbol)
			if got != tc.want {
				t.Errorf("classifyCloseReason(%s %s entry=%v exit=%v tp=%v sl=%v) = %q, want %q",
					tc.side, tc.symbol, tc.entry, tc.exit, tc.tpPips, tc.slPips, got, tc.want)
			}
		})
	}
}
