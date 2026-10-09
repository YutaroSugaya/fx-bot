package market

import (
	"fmt"
	"strings"
)

// QuoteCurrency は symbol の決済 (quote / counter) 通貨を返す。
//
//	"USD_JPY" → "JPY"   (quote=JPY: 損益はそのまま円)
//	"EUR_USD" → "USD"   (quote=USD: 損益は USD 建て → JPY 換算が必要)
//
// アンダースコアを含まない不正フォーマットは "" を返す (呼び出し側で
// QuoteJPYRate が error にする)。
func QuoteCurrency(symbol string) string {
	_, quote, ok := strings.Cut(symbol, "_")
	if !ok {
		return ""
	}
	return quote
}

// QuoteJPYRate は close PnL を JPY 建てに換算するための倍率を返す。
//
//	quote=JPY → 1.0                (usdJPYRate は無視)
//	quote=USD → usdJPYRate         (EUR/USD の損益[USD] × USD/JPY = JPY)
//	それ以外  → error              (未対応 quote 通貨)
//
// usdJPYRate は quote=USD のとき必須。<=0 (未取得) なら誤った損益を記録
// しないよう error を返す (fail-close)。
func QuoteJPYRate(symbol string, usdJPYRate float64) (float64, error) {
	switch QuoteCurrency(symbol) {
	case "JPY":
		return 1.0, nil
	case "USD":
		if usdJPYRate <= 0 {
			return 0, fmt.Errorf("QuoteJPYRate: %s requires positive USD/JPY rate, got %v", symbol, usdJPYRate)
		}
		return usdJPYRate, nil
	default:
		return 0, fmt.Errorf("QuoteJPYRate: unsupported quote currency for %q", symbol)
	}
}
