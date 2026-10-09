package strategy

import (
	"fmt"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
)

// momentumPipSize assumes a JPY-quote pair (0.01), like breakout_follow.go's
// local `pip := 0.01` and range_breakout_probe.go. NOTE: other strategies use
// the pure market.PipSize(symbol); with this constant the pip math is wrong
// for non-JPY pairs (EUR_USD / GBP_USD), so do not run these strategies there.
const momentumPipSize = 0.01

// MomentumPullback rides the higher-timeframe trend after a short-timeframe
// pullback. Day-trading heuristic:
//   - Trend from Summary6h (主軸); fall back to Summary24h if 6h is flat/empty
//   - Pullback detected on the LAST 5m candle (config.strategy.timeframe = "5m")
//     — using 1m would trigger on minute-scale noise unsuitable for 4-6h holds
//   - Enter in the trend direction after a confirmed 5m pullback bar
type MomentumPullback struct{}

func (MomentumPullback) Name() config.StrategyName { return config.StrategyMomentumPullback }

func (MomentumPullback) Evaluate(in EvalInput) Signal {
	if in.Config == nil || in.Summary == nil {
		return Signal{Decision: DecisionNone, StrategyName: config.StrategyMomentumPullback, CreatedAt: in.Now}
	}
	dir := in.Config.Entry.Direction
	if dir == config.DirectionNone {
		return Signal{Decision: DecisionNoTrade, StrategyName: config.StrategyMomentumPullback, Reason: "direction=none", CreatedAt: in.Now}
	}
	if in.Summary.CurrentRate.SpreadPips > in.Config.Entry.MaxSpreadPips {
		return Signal{Decision: DecisionNone, StrategyName: config.StrategyMomentumPullback, Reason: "spread_too_wide", CreatedAt: in.Now}
	}
	// Day-trading trend: prefer 6h, fall back to 24h. 1h is noise for the trade thesis.
	trend := in.Summary.Summary6h.TrendDirection
	if trend == "" || trend == "flat" {
		trend = in.Summary.Summary24h.TrendDirection
	}
	if trend != "up" && trend != "down" {
		return Signal{Decision: DecisionNone, StrategyName: config.StrategyMomentumPullback, Reason: "no_clear_trend", CreatedAt: in.Now}
	}
	if len(in.Candles5m) < 3 {
		return Signal{Decision: DecisionNone, StrategyName: config.StrategyMomentumPullback, Reason: "insufficient_candles", CreatedAt: in.Now}
	}

	// Pullback check: the last 5m candle should oppose the trend (config.timeframe = 5m).
	last := in.Candles5m[len(in.Candles5m)-1]
	pullback := false
	switch trend {
	case "up":
		pullback = last.Close < last.Open
	case "down":
		pullback = last.Close > last.Open
	}
	if !pullback {
		return Signal{Decision: DecisionNone, StrategyName: config.StrategyMomentumPullback, Reason: "no_pullback", CreatedAt: in.Now}
	}

	side := order.SideBuy
	if trend == "down" {
		side = order.SideSell
	}
	if !directionAllows(dir, side) {
		return Signal{Decision: DecisionNone, StrategyName: config.StrategyMomentumPullback, Reason: "direction_blocked", CreatedAt: in.Now}
	}

	entry := in.Summary.CurrentRate.Ask
	if side == order.SideSell {
		entry = in.Summary.CurrentRate.Bid
	}

	// 追いかけ防止: 直近 ChaseLookbackCandles 本の 5m ベースから entry が
	// MaxChasePips 以上離れていたら「もう走り切った後の順張り」として見送る
	// (両パラメータ > 0 のときだけ有効。0 = 無効で back-compat)。
	if in.Config.Entry.MaxChasePips > 0 && in.Config.Entry.ChaseLookbackCandles > 0 {
		if ext, ok := chaseExtensionPips(in.Candles5m, in.Config.Entry.ChaseLookbackCandles, side, entry); ok && ext > in.Config.Entry.MaxChasePips {
			return Signal{Decision: DecisionNone, StrategyName: config.StrategyMomentumPullback, Reason: "chasing_extended", ConfigID: in.Config.ConfigID, CreatedAt: in.Now}
		}
	}

	return configExitSignal(in, side, entry, config.StrategyMomentumPullback, fmt.Sprintf("trend=%s pullback", trend))
}

// chaseExtensionPips reports how far `entry` has extended from the base of the
// last `lookback` 5m candles, in pips. base = min(Low) for BUY, max(High) for
// SELL (the bottom/top of the recent move). A large extension means we'd be
// buying high / selling low = chasing. ok=false when there is nothing to
// measure (callers then skip the filter and enter as usual).
func chaseExtensionPips(candles []market.Candle, lookback int, side order.Side, entry float64) (float64, bool) {
	if lookback <= 0 || len(candles) == 0 {
		return 0, false
	}
	start := len(candles) - lookback
	if start < 0 {
		start = 0
	}
	window := candles[start:]
	switch side {
	case order.SideBuy:
		base := window[0].Low
		for _, c := range window {
			if c.Low < base {
				base = c.Low
			}
		}
		return (entry - base) / momentumPipSize, true
	case order.SideSell:
		base := window[0].High
		for _, c := range window {
			if c.High > base {
				base = c.High
			}
		}
		return (base - entry) / momentumPipSize, true
	}
	return 0, false
}

func directionAllows(dir config.Direction, side order.Side) bool {
	switch dir {
	case config.DirectionBoth:
		return true
	case config.DirectionBuyOnly:
		return side == order.SideBuy
	case config.DirectionSellOnly:
		return side == order.SideSell
	}
	return false
}
