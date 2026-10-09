package market

import "time"

// jstZone is a fixed +9 offset. JST observes no DST, so a fixed offset is exact and
// avoids depending on the host's tzdata being installed.
var jstZone = time.FixedZone("JST", 9*3600)

// IsForexOpen reports whether the spot-FX market is trading at t. It is the CANONICAL
// FX-week window for the whole repo (cmd/spread-calibrate.IsFXMarketClosedJST delegates
// here so the two definitions cannot drift).
//
// The market runs continuously from Monday 05:00 JST (Wellington/GMO week-open quote
// resume) to Saturday 06:00 JST (after the New York Friday close), then is shut over the
// weekend. We gate the autonomous LLM loop on this: when the market is closed there are
// no live prices to act on, so the loop must SKIP rather than burn API calls and surface
// "errors" on dead weekend quotes.
//
// Boundary notes (same reasoning as the original IsFXMarketClosedJST):
//   - Saturday 06:00 JST close: NY closes Sat 06:00 JST in US summer / 07:00 JST in US
//     winter. Using 06:00 means in winter the dead 06:00–06:59 hour at the Friday close
//     is treated as closed — harmless for the loop (that hour is illiquid/wide-spread →
//     no_trade anyway), and it keeps one shared boundary instead of a DST-aware split.
//   - Monday 05:00 JST open: keeps the Tokyo-open band (05–08 JST) live.
//
// The input may be in any zone (the scheduler passes the wall clock); it is converted to
// JST internally. JST has no DST so a fixed +9 offset is exact.
func IsForexOpen(t time.Time) bool {
	jst := t.In(jstZone)
	h := jst.Hour()
	switch jst.Weekday() {
	case time.Saturday:
		// NY Friday session is still winding down until ~06:00 JST; closed after.
		return h < 6
	case time.Sunday:
		// Closed all day (reopens Monday 05:00 JST).
		return false
	case time.Monday:
		// Opens 05:00 JST (Wellington/GMO week-open).
		return h >= 5
	default:
		// Tuesday–Friday: fully open.
		return true
	}
}

// TradingDayStartJST returns the start of the trading day containing t: 06:00 JST, the
// same boundary as GMO's 本日損益 and the dashboard's daily aggregates (fixed +9, no
// DST). Times before 06:00 JST belong to the PREVIOUS day's
// session (an overnight NY position closed at 04:00 JST counts toward yesterday's
// trading day). The LLM decision cycle feeds "today's closed trades" to the decision prompt with this floor
// so the playbook's 同日2敗打ち止め rule has one unambiguous definition of 同日.
func TradingDayStartJST(t time.Time) time.Time {
	jst := t.In(jstZone)
	day := time.Date(jst.Year(), jst.Month(), jst.Day(), 6, 0, 0, 0, jstZone)
	if jst.Before(day) {
		day = day.AddDate(0, 0, -1)
	}
	return day
}
