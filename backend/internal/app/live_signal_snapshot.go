package app

import (
	"time"

	"fx-bot/backend/internal/app/livesignal"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/domain/ta"
	"fx-bot/backend/internal/usecase/command"
)

// BuildLiveSignalSnapshot maps one TradingCycle evaluation (strategy + risk
// gate) plus the live market summary / active config into a dashboard-facing
// Snapshot. This is the bot's REAL per-tick decision, so the UI can show
// "entry met" only when the bot would actually enter. candles5m is the same 5m
// window the engine evaluated, used to surface the ma_pullback 200MAs (nil/short = MAs
// left 0).
func BuildLiveSignalSnapshot(symbol string, now time.Time, res command.TradingCycleResult,
	summary *market.MarketSummary, cfg *config.StrategyConfig, candles5m, candles1h []market.Candle) *livesignal.Snapshot {

	s := &livesignal.Snapshot{
		Symbol:         symbol,
		EvaluatedAt:    now,
		StrategyName:   string(res.Signal.StrategyName),
		Reason:         res.Signal.Reason,
		Executed:       res.Executed,
		EntryPrice:     res.Signal.EntryPrice,
		TakeProfitPips: res.Signal.TakeProfitPips,
		StopLossPips:   res.Signal.StopLossPips,
	}

	// The two 200MAs the ma_pullback strategy reads on 5m. Computed here (not in the pure
	// strategy) so every strategy's snapshot can show them as context.
	if closes := closesFromCandles(candles5m); len(closes) >= 200 {
		if sma, ok := ta.SMA(closes, 200); ok {
			s.Sma5m200 = sma
		}
		if ema, ok := ta.EMA(closes, 200); ok {
			s.Ema5m200 = ema
		}
	}

	// ma_pullback: surface the per-gate entry funnel so the UI can show how
	// far the setup is and what's still needed. Only for the ma_pullback strategy
	// (other strategies have a different funnel); driven off the same inputs the
	// engine evaluated this tick.
	if cfg != nil && cfg.Strategy.Name == config.StrategyMAPullback {
		for _, g := range (strategy.MAPullback{}).Gates(strategy.EvalInput{
			Now: now, Summary: summary, Candles5m: candles5m, Candles1h: candles1h, Config: cfg,
		}) {
			s.Gates = append(s.Gates, livesignal.Gate{Key: g.Key, OK: g.OK, Detail: g.Detail})
		}
	}

	switch res.Decision {
	case strategy.DecisionEnter:
		s.Decision = "enter"
	case strategy.DecisionNoTrade:
		s.Decision = "no_trade"
	default:
		s.Decision = "none"
	}

	if res.Signal.Side.Valid() {
		s.Side = string(res.Signal.Side)
	}

	// The strategy said ENTER but no order was placed → a risk gate blocked it.
	if res.Decision == strategy.DecisionEnter && !res.Executed {
		s.GateBlocked = true
		s.GateReason = res.GateReason
	}

	if summary != nil {
		s.Trend6h = summary.Summary6h.TrendDirection
		s.Trend24h = summary.Summary24h.TrendDirection
		s.SpreadPips = summary.CurrentRate.SpreadPips
		s.Atr6hPips = summary.Summary6h.ATRPips
		s.Price = (summary.CurrentRate.Bid + summary.CurrentRate.Ask) / 2
	}
	if cfg != nil {
		s.MaxSpreadPips = cfg.Entry.MaxSpreadPips
		if s.StrategyName == "" {
			s.StrategyName = string(cfg.Strategy.Name)
		}
	}
	return s
}

// closesFromCandles extracts the Close series (order preserved). nil-safe.
func closesFromCandles(candles []market.Candle) []float64 {
	out := make([]float64, len(candles))
	for i, c := range candles {
		out[i] = c.Close
	}
	return out
}
