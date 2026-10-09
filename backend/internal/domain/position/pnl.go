package position

import (
	"fmt"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
)

// ComputeClosePnL は close 時の realized PnL を pips / JPY で返す。
//
// reconcile.go / close_saga.go が共通で使う
// close PnL 計算を domain に集約したもの。挙動:
//
//   - pip = market.PipSize(symbol)。pip == 0 (未知 symbol) なら pnlPips=0 を
//     返すが、jpy は diff*qty*dir でそのまま計算する。
//   - dir = +1 (BUY) / -1 (SELL)。BUY/SELL 以外の Side は error で弾く
//     (silently dir=1 で計算すると、不正な Side で BUY 損益が出てしまうため)。
//
// 引数:
//
//	entry, exit:  約定価格 (絶対値)
//	side:         order.SideBuy or order.SideSell
//	qty:          数量 (損益の倍率)。負・0 も許容 (defensive)
//	symbol:       "USD_JPY" など。市場別 pip 解決に使う
//	quoteJPYRate: quote 通貨→JPY 換算倍率。market.QuoteJPYRate で解決する。
//	              JPY quote (USD_JPY 等) は 1.0、USD quote (EUR_USD 等) は
//	              現在の USD/JPY レート。これにより quote=USD ペアの USD 建て
//	              損益を正しく JPY に換算する。
//
// 返り値:
//
//	pips: pip 単位の損益。pip == 0 のとき 0
//	jpy:  JPY 損益 = 差分*数量*dir*quoteJPYRate
//	err:  Side が BUY/SELL 以外のとき non-nil
func ComputeClosePnL(
	entry, exit float64,
	side order.Side,
	qty int,
	symbol string,
	quoteJPYRate float64,
) (pips, jpy float64, err error) {
	var dir float64
	switch side {
	case order.SideBuy:
		dir = 1
	case order.SideSell:
		dir = -1
	default:
		return 0, 0, fmt.Errorf("invalid side %q (must be BUY or SELL)", side)
	}
	diff := exit - entry
	if pip := market.PipSize(symbol); pip > 0 {
		pips = diff / pip * dir
	}
	jpy = diff * float64(qty) * dir * quoteJPYRate
	return pips, jpy, nil
}
