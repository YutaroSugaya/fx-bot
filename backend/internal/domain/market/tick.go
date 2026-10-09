// Package market holds value types for market data (ticks, candles, summaries).
// These are pure data — no I/O, no external deps beyond stdlib.
package market

import "time"

// Ticker is a single bid/ask snapshot from the broker.
type Ticker struct {
	Symbol    string
	Bid       float64
	Ask       float64
	Timestamp time.Time
}

// Mid returns the mid-price.
func (t Ticker) Mid() float64 { return (t.Bid + t.Ask) / 2 }

// SpreadPips reports the bid/ask spread in pips for the given pip size
// (e.g. 0.01 for USD_JPY).
func (t Ticker) SpreadPips(pipSize float64) float64 {
	if pipSize == 0 {
		return 0
	}
	return (t.Ask - t.Bid) / pipSize
}

// Kline is one OHLCV bar.
type Kline struct {
	Symbol   string
	Interval string // "1min" | "5min" | "1hour"
	OpenTime time.Time
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Volume   float64
}

// PipSize returns the pip size for the symbol.
// JPY-quote pairs (USD_JPY, EUR_JPY, ...) use 0.01; USD-quote pairs
// (EUR_USD, GBP_USD, ...) use 0.0001. See QuoteCurrency / QuoteJPYRate for
// how USD-quote PnL is converted to JPY.
func PipSize(symbol string) float64 {
	switch symbol {
	case "USD_JPY", "EUR_JPY", "GBP_JPY", "AUD_JPY":
		return 0.01
	default:
		// USD-quote and other 4-decimal pairs (EUR_USD, GBP_USD, ...).
		return 0.0001
	}
}
