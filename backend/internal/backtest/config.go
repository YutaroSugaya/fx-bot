package backtest

import "fx-bot/backend/internal/config"

// EngineConfig is the immutable bundle passed to NewEngine. The Engine never
// mutates fields — strategy_config drift across the replay is a Mode B
// concern handled by the caller.
type EngineConfig struct {
	Symbol         string
	StrategyConfig *config.StrategyConfig

	// Costs is added to the simulated fill in adverse direction (BUY adds
	// slippage to ask, SELL subtracts from bid). FeeJPYPerTrade is subtracted
	// from realised PnL at close. Zero-value = frictionless backtest.
	Costs CostModel

	// ConflictPolicy decides who wins when one bar's High/Low brackets both
	// TP and SL. Default is PessimisticSLFirst.
	ConflictPolicy ConflictPolicy

	// MaxHistoryBars caps the look-back window fed to the strategy each bar (replay is O(n×window)).
	// 0 = default (20000, tuned for 1m). Daily-replay mode sets it small (e.g. 300 daily bars cover
	// a 200-day SMA + Donchian-55); 20000 daily bars would be ~80 years.
	MaxHistoryBars int

	// AssumedUSDJPYRate converts a USD-quote pair's (e.g. EUR_USD) USD-denominated
	// PnL into JPY for the ProfitLossJPY column. A single-symbol candle backtest
	// has no USD/JPY series, so the caller supplies a representative constant
	// (e.g. 157.0). IGNORED for JPY-quote pairs (their PnL is already JPY → factor
	// 1.0). If left 0 for a USD-quote backtest, ProfitLossJPY stays in QUOTE
	// currency (USD) and ProfitLossPips remains the currency-agnostic truth.
	AssumedUSDJPYRate float64
}

// CostModel captures per-trade execution costs the backtest simulates.
// Both fields default to 0 (= idealised replay).
type CostModel struct {
	SlippagePips   float64
	FeeJPYPerTrade float64

	// SpreadModel models the time-of-day bid/ask spread
	// (normal ~0.5pip + Tokyo-open spike ~10pips). When set, half the modeled
	// spread is added to the per-leg adverse fill, on top of SlippagePips. nil =
	// constant SlippagePips only (back-compat). See spread_model.go.
	SpreadModel SpreadModel

	// FeeRatePct models GMO's 約定金額 × rate% commission:
	// roundtrip fee = (entry notional + exit notional) × FeeRatePct/100, converted
	// to JPY via quoteJPYRate. GMO's published rate is 0.002 (= 0.002%). 0 = off
	// (back-compat with the fixed FeeJPYPerTrade). Both are subtracted if set.
	FeeRatePct float64

	// SwapTable models the overnight swap: symbol → side
	// ("BUY"/"SELL") → signed JPY per night per 1,000 units (negative = pay).
	// Nights = 21:00 UTC crossings between open and close, Wednesday (UTC) ×3
	// (weekend roll). nil = swap disabled (exact back-compat). See swap_model.go.
	SwapTable SwapTable

	// quoteJPYRate is the resolved quote→JPY multiplier for the run's symbol,
	// set once by the engine (Replay) from the symbol + EngineConfig.
	// AssumedUSDJPYRate. JPY-quote pairs → 1.0 (PnL already JPY). <=0 is treated
	// as 1.0 (= PnL left in quote currency). Unexported: callers configure it via
	// EngineConfig.AssumedUSDJPYRate, not directly.
	quoteJPYRate float64

	// symbol is the run's symbol, resolved once by the engine (Replay) like
	// quoteJPYRate. makeTrade uses it for the SwapTable lookup.
	// Unexported: callers configure it via EngineConfig.Symbol, not directly.
	symbol string
}

// ConflictPolicy controls TP/SL same-bar resolution.
type ConflictPolicy int

const (
	// PessimisticSLFirst: when a single bar's High/Low touches both levels,
	// assume SL fires first (worst-case for the position). Default.
	PessimisticSLFirst ConflictPolicy = iota
	// OptimisticTPFirst: assume TP fires first. Use only for sensitivity tests.
	OptimisticTPFirst
	// SkipAmbiguous: do not close the position this bar; carry to next bar.
	// Result.AmbiguousBars counts the skipped bars.
	SkipAmbiguous
)
