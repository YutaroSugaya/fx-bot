package strategy

import (
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
)

// gotobiJST is the fixed +09:00 zone used to bucket the gotobi calendar/clock.
// FX runs 24/5 and Japan has no DST, so a fixed offset is exact (unlike the
// London-session strategies, which must track BST).
var gotobiJST = time.FixedZone("JST", 9*60*60)

// gotobi probe entry/exit constants. FIXED for the probe and intentionally NOT
// swept — picking the best entry minute / SL over the data would be the exact
// multiple-comparison overfit the pre-registration forbids.
const (
	gotobiEntryHourJST = 9     // buy at 09:00 JST — after the 05-08 JST spread-spike window
	gotobiEntryMinJST  = 0     //
	gotobiMaxHoldMin   = 55    // hold to the 09:55 JST fix, then close (the real exit)
	gotobiSLPips       = 30.0  // protective tail guard; rarely binding in a 55-min window
	gotobiTPPips       = 500.0 // effectively no TP — TP=0 would exit instantly at entry price
)

// GotobiFix (probe) buys USD/JPY at 09:00 JST on gotobi days and exits at the
// 09:55 JST fixing via MaxHold. The thesis is a real-demand FLOW — Japanese
// importer settlement concentrated on 5/10 dates buying USD into the Tokyo
// fixing — not a chart pattern. It is a "causal" (flow) edge candidate, as
// opposed to the chart-pattern pullback family, which showed no gross edge above
// the cost floor. This is a FALSIFIER on ~2.6y of data, not a validation: a clearly
// negative result kills the hypothesis; a positive one only earns a data
// extension + a proper OOS/holdout test (pre-registered decision rule).
//
// Exit is strategy-computed (the Signal carries MaxHold/SL/TP), so the config's
// exit fields are validator placeholders — same contract as ma_pullback.
type GotobiFix struct{}

func (GotobiFix) Name() config.StrategyName { return config.StrategyGotobiFix }

func (GotobiFix) Evaluate(in EvalInput) Signal {
	name := config.StrategyGotobiFix
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
	// Live spread guard. Frictionless in backtest (ticker bid=ask, SpreadPips=0),
	// where the real cost is modeled by the CLI cost flags (fee-rate/spread/slip).
	if in.Summary.CurrentRate.SpreadPips > in.Config.Entry.MaxSpreadPips {
		return none("spread_too_wide")
	}
	// Calendar + clock gate: only the gotobi-day 09:00 JST bar fires. The engine
	// then holds the single position until MaxHold (09:55), so it can re-arm only
	// on the next gotobi day — no double entries.
	j := in.Now.In(gotobiJST)
	if !isGotobiTradingDay(j) {
		return none("not_gotobi_day")
	}
	if j.Hour() != gotobiEntryHourJST || j.Minute() != gotobiEntryMinJST {
		return none("not_entry_minute")
	}
	side := order.SideBuy // importer USD demand into the 09:55 fix lifts USD/JPY
	if !directionAllows(dir, side) {
		return none("direction_blocked")
	}
	return Signal{
		Decision:       DecisionEnter,
		Side:           side,
		EntryPrice:     in.Summary.CurrentRate.Ask,
		TakeProfitPips: gotobiTPPips,
		StopLossPips:   gotobiSLPips,
		MaxHoldMinutes: gotobiMaxHoldMin,
		Quantity:       in.Config.Risk.Quantity,
		Reason:         "gotobi 09:00->09:55 fix buy",
		ConfigID:       in.Config.ConfigID,
		StrategyName:   name,
		CreatedAt:      in.Now,
	}
}

// isGotobiCalendarDay reports whether d's day-of-month is a gotobi date:
// 5/10/15/20/25 or the last calendar day of the month.
func isGotobiCalendarDay(d time.Time) bool {
	switch d.Day() {
	case 5, 10, 15, 20, 25:
		return true
	}
	return d.AddDate(0, 0, 1).Day() == 1 // tomorrow rolls into next month ⇒ d is month-end
}

// isGotobiTradingDay reports whether t (any zone; converted to JST) is a day the
// gotobi flow should be traded: a gotobi calendar day, with a gotobi day landing
// on a weekend shifted back to the prior business day (Friday). Weekends never
// trade. JP bank holidays are NOT handled (declared scope of the probe).
func isGotobiTradingDay(t time.Time) bool {
	d := t.In(gotobiJST)
	switch d.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	case time.Friday:
		if isGotobiCalendarDay(d) {
			return true
		}
		// A gotobi day on the upcoming Sat/Sun shifts back to this Friday.
		return isGotobiCalendarDay(d.AddDate(0, 0, 1)) || isGotobiCalendarDay(d.AddDate(0, 0, 2))
	default:
		return isGotobiCalendarDay(d)
	}
}
