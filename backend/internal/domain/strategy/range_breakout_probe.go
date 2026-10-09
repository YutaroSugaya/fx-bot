package strategy

import (
	"fmt"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
)

// RangeBreakoutProbe enters just before a small intraday range is likely to
// break. Unlike BreakoutFollow, this is a pre-breakout probe: it does not wait
// for price to clear the range, and tight TP/SL/early-exit settings must manage
// failed attempts.
type RangeBreakoutProbe struct{}

func (RangeBreakoutProbe) Name() config.StrategyName { return config.StrategyRangeBreakoutProbe }

func (RangeBreakoutProbe) Evaluate(in EvalInput) Signal {
	if in.Config == nil || in.Summary == nil {
		return Signal{Decision: DecisionNone, StrategyName: config.StrategyRangeBreakoutProbe, CreatedAt: in.Now}
	}
	if in.Config.Entry.RequireBreakout {
		return Signal{Decision: DecisionNone, StrategyName: config.StrategyRangeBreakoutProbe, Reason: "require_breakout=true", CreatedAt: in.Now}
	}
	dir := in.Config.Entry.Direction
	if dir == config.DirectionNone {
		return Signal{Decision: DecisionNoTrade, StrategyName: config.StrategyRangeBreakoutProbe, Reason: "direction=none", CreatedAt: in.Now}
	}
	if in.Summary.CurrentRate.SpreadPips > in.Config.Entry.MaxSpreadPips {
		return Signal{Decision: DecisionNone, StrategyName: config.StrategyRangeBreakoutProbe, Reason: "spread_too_wide", CreatedAt: in.Now}
	}

	hi := in.Summary.Summary1h.High
	lo := in.Summary.Summary1h.Low
	if hi == 0 || lo == 0 || hi <= lo {
		hi = in.Summary.Summary6h.High
		lo = in.Summary.Summary6h.Low
	}
	if hi == 0 || lo == 0 || hi <= lo {
		return Signal{Decision: DecisionNone, StrategyName: config.StrategyRangeBreakoutProbe, Reason: "no_range", CreatedAt: in.Now}
	}

	const pip = 0.01
	rangePips := (hi - lo) / pip
	if rangePips < 8 || rangePips > 24 {
		return Signal{Decision: DecisionNone, StrategyName: config.StrategyRangeBreakoutProbe, Reason: "range_not_probeable", CreatedAt: in.Now}
	}

	curAsk := in.Summary.CurrentRate.Ask
	curBid := in.Summary.CurrentRate.Bid
	edge := 2 * pip
	overrun := 1 * pip

	if curAsk > hi+overrun || curBid < lo-overrun {
		return Signal{Decision: DecisionNone, StrategyName: config.StrategyRangeBreakoutProbe, Reason: "already_broken_out", CreatedAt: in.Now}
	}
	if curAsk >= hi-edge && curAsk <= hi+overrun && directionAllows(dir, order.SideBuy) {
		return configExitSignal(in, order.SideBuy, curAsk, config.StrategyRangeBreakoutProbe, fmt.Sprintf("probe_near_resistance_%.3f", hi))
	}
	if curBid <= lo+edge && curBid >= lo-overrun && directionAllows(dir, order.SideSell) {
		return configExitSignal(in, order.SideSell, curBid, config.StrategyRangeBreakoutProbe, fmt.Sprintf("probe_near_support_%.3f", lo))
	}
	return Signal{Decision: DecisionNone, StrategyName: config.StrategyRangeBreakoutProbe, Reason: "inside_range_not_near_edge", CreatedAt: in.Now}
}
