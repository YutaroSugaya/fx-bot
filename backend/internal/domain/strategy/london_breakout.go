package strategy

import (
	"fmt"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
)

// London-breakout probe windows + exit, anchored in UTC. UTC anchoring keeps the
// window fixed across BST (a fixed-JST window would smear: the London open sits
// at 07:00 UTC in summer / 08:00 UTC in winter, both inside [07:00,10:00)).
// FIXED for the probe and NOT swept — sweeping the window/range definition over
// the data is the exact overfit the pre-registration forbids.
const (
	lbAsianStartUTC = 0   // Tokyo/Asian range = [00:00, 07:00) UTC (= 09:00-16:00 JST)
	lbAsianEndUTC   = 7   //
	lbEntryStartUTC = 7   // breakout entry window = [07:00, 10:00) UTC (London open + first hours)
	lbEntryEndUTC   = 10  //
	lbSLRangeFrac   = 0.5 // SL = 0.5 × Asian range width (volatility-scaled, structural)
	lbTPRangeFrac   = 1.0 // TP = 1.0 × Asian range width (≈ 2R)
	lbMaxHoldMin    = 480 // force-close (covers London → NY)
)

// LondonBreakout (probe) trades the first London-open break of the Tokyo/Asian
// range. The thesis is a volatility-regime transition — Tokyo compression giving
// way to London expansion in the break direction — a session-structure (flow)
// cause, not a chart pattern. It is a causal (flow) candidate like gotobi.
// Deliberately RAW: no range-width band, no 5m-close/retest
// confirmation (those are refinements to add only IF the raw effect survives —
// filters trim, they do not create edge). Exit is strategy-computed (SL/TP from
// the range width), so the config's exit fields are validator placeholders.
//
// This is a FALSIFIER on ~2.6y of one pair, not a validation: the
// pre-registered decision rule is asymmetric (a clear negative kills it; a
// positive only earns a longer OOS/holdout test).
type LondonBreakout struct{}

func (LondonBreakout) Name() config.StrategyName { return config.StrategyLondonBreakout }

func (LondonBreakout) Evaluate(in EvalInput) Signal {
	name := config.StrategyLondonBreakout
	none := func(reason string) Signal {
		return Signal{Decision: DecisionNone, StrategyName: name, Reason: reason, CreatedAt: in.Now}
	}
	if in.Config == nil || in.Summary == nil {
		return Signal{Decision: DecisionNone, StrategyName: name, CreatedAt: in.Now}
	}
	dir := in.Config.Entry.Direction
	if dir == config.DirectionNone {
		return Signal{Decision: DecisionNoTrade, StrategyName: name, Reason: "direction=none", CreatedAt: in.Now}
	}
	// Live spread guard. Frictionless in backtest (cost from the CLI cost flags).
	if in.Summary.CurrentRate.SpreadPips > in.Config.Entry.MaxSpreadPips {
		return none("spread_too_wide")
	}
	pip := market.PipSize(in.Config.Symbol)
	if pip <= 0 {
		return none("no_pip")
	}
	now := in.Now.UTC()
	if h := now.Hour(); h < lbEntryStartUTC || h >= lbEntryEndUTC {
		return none("outside_entry_window")
	}
	hi, lo, ok := asianRange(in.Candles1m, now)
	if !ok {
		return none("no_asian_range")
	}
	rangeWidthPips := (hi - lo) / pip
	if rangeWidthPips <= 0 {
		return none("degenerate_range")
	}
	// One trade per day: skip if an earlier bar in today's entry window already
	// closed beyond the range (this is not the FIRST break). Keeps the probe to
	// one London break per day without per-call strategy state — after an early
	// stop the engine goes flat, but this scan still sees the prior break and
	// declines, so there is no same-day re-entry.
	for _, c := range in.Candles1m {
		t := c.OpenTime.UTC()
		if t.Year() != now.Year() || t.Month() != now.Month() || t.Day() != now.Day() {
			continue
		}
		if hh := t.Hour(); hh < lbEntryStartUTC || hh >= lbEntryEndUTC {
			continue
		}
		if !t.Before(now) {
			continue
		}
		if c.Close > hi || c.Close < lo {
			return none("already_broke_today")
		}
	}
	mid := (in.Summary.CurrentRate.Bid + in.Summary.CurrentRate.Ask) / 2
	var side order.Side
	switch {
	case mid > hi:
		side = order.SideBuy
	case mid < lo:
		side = order.SideSell
	default:
		return none("inside_range")
	}
	if !directionAllows(dir, side) {
		return none("direction_blocked")
	}
	entry := in.Summary.CurrentRate.Ask
	if side == order.SideSell {
		entry = in.Summary.CurrentRate.Bid
	}
	return Signal{
		Decision:       DecisionEnter,
		Side:           side,
		EntryPrice:     entry,
		TakeProfitPips: lbTPRangeFrac * rangeWidthPips,
		StopLossPips:   lbSLRangeFrac * rangeWidthPips,
		MaxHoldMinutes: lbMaxHoldMin,
		Quantity:       in.Config.Risk.Quantity,
		Reason:         fmt.Sprintf("london breakout %s range=%.0fpips", side, rangeWidthPips),
		ConfigID:       in.Config.ConfigID,
		StrategyName:   name,
		CreatedAt:      in.Now,
	}
}

// asianRange returns the high/low of the [00:00,07:00) UTC Tokyo session on the
// same UTC date as now. ok=false when no Asian-session bar is present.
func asianRange(candles []market.Candle, now time.Time) (hi, lo float64, ok bool) {
	y, mo, d := now.UTC().Date()
	for _, c := range candles {
		t := c.OpenTime.UTC()
		cy, cm, cd := t.Date()
		if cy != y || cm != mo || cd != d {
			continue
		}
		if h := t.Hour(); h < lbAsianStartUTC || h >= lbAsianEndUTC {
			continue
		}
		if !ok {
			hi, lo, ok = c.High, c.Low, true
			continue
		}
		if c.High > hi {
			hi = c.High
		}
		if c.Low < lo {
			lo = c.Low
		}
	}
	return
}
