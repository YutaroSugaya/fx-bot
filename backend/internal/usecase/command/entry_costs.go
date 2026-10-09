package command

import (
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
)

// entryCostSnapshot は migration 0008 の entry 時点コスト捕捉列を計算する純関数。
// admission ロック内 spread と実 fill 価格を焼き込み、live
// フォワードを backtest コストモデル (spread / slippage) の較正データ源にする。
//
//   - spreadPips:   発注直前 ticker の bid/ask 幅。
//   - slippagePips: 符号付き adverse slippage。意図価格 = ticker の BUY:ask /
//     SELL:bid (MARKET 発注が「見えていた価格」)。正 = 不利方向に滑った。
//
// ticker が無い (nil) / pip 不明は (nil, nil) = 「未捕捉」— 0 と区別して NULL で
// 永続化される。
func entryCostSnapshot(side order.Side, tk *market.Ticker, fillPrice, pip float64) (spreadPips, slippagePips *float64) {
	if tk == nil || pip <= 0 {
		return nil, nil
	}
	sp := tk.SpreadPips(pip)
	spreadPips = &sp
	if fillPrice > 0 {
		var slip float64
		switch side {
		case order.SideBuy:
			slip = (fillPrice - tk.Ask) / pip
		case order.SideSell:
			slip = (tk.Bid - fillPrice) / pip
		default:
			return spreadPips, nil
		}
		slippagePips = &slip
	}
	return spreadPips, slippagePips
}
