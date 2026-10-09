package strategy

import (
	"fmt"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
)

// BreakoutFollow enters when current price breaks above (BUY) or below (SELL)
// the recent intraday range, confirming the move. Day-trading heuristic:
//   - require Config.Entry.RequireBreakout=true (else NoTrade-ish NONE)
//   - rangeHigh = Summary6h.High (intraday range, not 1h noise)
//   - rangeLow  = Summary6h.Low
//   - 3 pip cushion to filter intraday noise (TP/SL is 30+ pips, so the filter is cheap)
//   - if current Ask > rangeHigh + 3 pips → BUY breakout
//   - if current Bid < rangeLow  - 3 pips → SELL breakout
type BreakoutFollow struct{}

func (BreakoutFollow) Name() config.StrategyName { return config.StrategyBreakoutFollow }

func (BreakoutFollow) Evaluate(in EvalInput) Signal {
	if in.Config == nil || in.Summary == nil {
		return Signal{Decision: DecisionNone, StrategyName: config.StrategyBreakoutFollow, CreatedAt: in.Now}
	}
	if !in.Config.Entry.RequireBreakout {
		return Signal{Decision: DecisionNone, StrategyName: config.StrategyBreakoutFollow, Reason: "require_breakout=false", CreatedAt: in.Now}
	}
	dir := in.Config.Entry.Direction
	if dir == config.DirectionNone {
		return Signal{Decision: DecisionNoTrade, StrategyName: config.StrategyBreakoutFollow, Reason: "direction=none", CreatedAt: in.Now}
	}
	if in.Summary.CurrentRate.SpreadPips > in.Config.Entry.MaxSpreadPips {
		return Signal{Decision: DecisionNone, StrategyName: config.StrategyBreakoutFollow, Reason: "spread_too_wide", CreatedAt: in.Now}
	}

	// Day-trading: use the 6h intraday range. Fall back to 1h if 6h is empty
	// (warm-up after restart).
	hi := in.Summary.Summary6h.High
	lo := in.Summary.Summary6h.Low
	if hi == 0 || lo == 0 || hi <= lo {
		hi = in.Summary.Summary1h.High
		lo = in.Summary.Summary1h.Low
	}
	if hi == 0 || lo == 0 || hi <= lo {
		return Signal{Decision: DecisionNone, StrategyName: config.StrategyBreakoutFollow, Reason: "no_range", CreatedAt: in.Now}
	}

	// 3-pip cushion to avoid noise. With TP 30+ pips a 3-pip filter is cheap.
	pip := 0.01
	cushion := 3 * pip

	curAsk := in.Summary.CurrentRate.Ask
	curBid := in.Summary.CurrentRate.Bid

	if curAsk > hi+cushion && directionAllows(dir, order.SideBuy) {
		return configExitSignal(in, order.SideBuy, curAsk, config.StrategyBreakoutFollow, fmt.Sprintf("breakout_above_%.3f", hi))
	}
	if curBid < lo-cushion && directionAllows(dir, order.SideSell) {
		return configExitSignal(in, order.SideSell, curBid, config.StrategyBreakoutFollow, fmt.Sprintf("breakout_below_%.3f", lo))
	}
	return Signal{Decision: DecisionNone, StrategyName: config.StrategyBreakoutFollow, Reason: "inside_range", CreatedAt: in.Now}
}
