package command

import (
	"context"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
)

// resolveQuoteJPYRate は close PnL を JPY 建てにするための quote→JPY 倍率を
// 解決する。position.ComputeClosePnL の quoteJPYRate 引数に渡す。
//
//   - JPY quote (USD_JPY / EUR_JPY / GBP_JPY ...) → 1.0。broker は呼ばない。
//   - USD quote (EUR_USD / GBP_USD ...) → broker から USD/JPY を取得し、その
//     mid を倍率にする (EUR/USD の USD 建て損益 × USD/JPY = JPY)。
//   - broker error / レート欠損 → error を伝播 (誤った JPY 損益を記録しない
//     よう fail-close)。
func resolveQuoteJPYRate(ctx context.Context, broker port.Broker, symbol string) (float64, error) {
	if market.QuoteCurrency(symbol) == "JPY" {
		return 1.0, nil
	}
	tk, err := broker.GetTicker(ctx, "USD_JPY")
	if err != nil {
		return 0, err
	}
	return market.QuoteJPYRate(symbol, tk.Mid())
}
