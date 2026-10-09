package position

import "fx-bot/backend/internal/domain/order"

// ComputeTPSLPrices は entry/side/pips/pip_size から TP/SL の絶対価格を計算する。
//
// 規約:
//
//	BUY  → TP = entry + tpPips*pip,  SL = entry - slPips*pip
//	SELL → TP = entry - tpPips*pip,  SL = entry + slPips*pip
//
// pips が 0 のときの戻り値は entry 自身 (= TP/SL 未設定の paper モードで
// "0 を返す" 形に意味を寄せず、呼び出し側に判定責任を残す)。
func ComputeTPSLPrices(side order.Side, entry, tpPips, slPips, pip float64) (tpPrice, slPrice float64) {
	if side == order.SideBuy {
		return entry + tpPips*pip, entry - slPips*pip
	}
	return entry - tpPips*pip, entry + slPips*pip
}
