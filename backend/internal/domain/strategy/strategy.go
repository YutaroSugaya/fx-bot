package strategy

import (
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
)

// EvalInput is the bundle of inputs passed to each Strategy.Evaluate. The
// engine constructs this from the worker's polled data.
type EvalInput struct {
	Now       time.Time
	Summary   *market.MarketSummary
	Candles1m []market.Candle
	Candles5m []market.Candle
	// Candles1h is the higher-timeframe series the MTF pullback strategy (mtf_pullback)
	// needs for its environment-recognition (1h trend direction). Optional:
	// nil/empty for strategies that don't use it (momentum_pullback etc.).
	Candles1h []market.Candle
	Config    *config.StrategyConfig
}

// trendDir is a market-direction label produced by the trend detectors
// (1h / 5m regression + swing structure, and the 200MA slope). Typed so the
// compiler rejects a stray string literal and the three valid values live in
// one place. Underlying string values are unchanged ("up"/"down"/"flat") so
// any serialized/logged form is identical to the pre-typing code.
type trendDir string

const (
	trendUp   trendDir = "up"
	trendDown trendDir = "down"
	trendFlat trendDir = "flat"
)

// Strategy is the interface every named strategy implements.
//
// Strategy.Name() returns the StrategyName this implementation handles; the
// engine uses it for dispatch. Evaluate must be pure (no I/O, no DB) so the
// engine can call it on every price tick.
type Strategy interface {
	Name() config.StrategyName
	Evaluate(in EvalInput) Signal
}
