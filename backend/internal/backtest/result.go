// Package backtest provides a fixed-config replay engine for evaluating
// strategy performance against historical 1-minute candles.
//
// Mode A (this file): single fixed strategy_config is replayed; Claude advisor
// is NOT invoked. Used to measure the bare edge of a strategy without
// hourly config drift. Mode B (advisor-replay) is a future extension.
package backtest

import "time"

// Trade is a closed round-trip recorded by the engine. Pure value type — no
// DB columns, no broker order IDs. Sufficient for metric computation and
// reconstruction of the equity curve.
type Trade struct {
	Side           string  // "BUY" | "SELL"
	EntryPrice     float64 // price at which the position was opened
	ExitPrice      float64 // price at which it was closed
	OpenedAt       time.Time
	ClosedAt       time.Time
	ProfitLossPips float64
	ProfitLossJPY  float64
	// SwapJPY is the simulated overnight swap component,
	// already INCLUDED in ProfitLossJPY (which stays the NET simulated result,
	// consistent with fee/slippage handling). Recorded separately for
	// transparency. 0 when CostModel.SwapTable is nil.
	SwapJPY     float64
	CloseReason string // "take_profit" | "stop_loss" | "max_hold"
}

// EquityPoint is one (time, equity) sample on the cumulative PnL curve.
// Used for drawdown computation.
type EquityPoint struct {
	Time   time.Time
	Equity float64 // cumulative PnL JPY since start
}

// Result bundles everything a Backtest run produces.
type Result struct {
	Trades        []Trade
	Metrics       Metrics
	AmbiguousBars int // bars where both TP and SL would have hit; resolved per ConflictPolicy
}
