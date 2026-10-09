package market

import (
	"testing"
	"time"
)

// IsForexOpen gates the autonomous LLM loop so it does not burn API calls (and show
// "errors") on a CLOSED spot-FX market over the weekend. It is the canonical FX-week
// window (Monday 05:00 JST open → Saturday 06:00 JST close, all Sunday closed); these
// MUST stay the exact inverse of cmd/spread-calibrate.IsFXMarketClosedJST, which now
// delegates here. (JST has no DST so a fixed +9 offset is exact.)
func TestIsForexOpen(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	// 2026-06 calendar: 17=Wed 18=Thu 19=Fri 20=Sat 21=Sun 22=Mon.
	at := func(d, h, m int) time.Time { return time.Date(2026, 6, d, h, m, 0, 0, jst) }

	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"Wed midday — open", at(17, 12, 0), true},
		{"Thu 03:00 — open", at(18, 3, 0), true},
		{"Fri midday — open", at(19, 12, 0), true},
		{"Fri 23:00 — open (NY not yet closed)", at(19, 23, 0), true},
		{"Sat 03:00 JST — still open (NY Friday session)", at(20, 3, 0), true},
		{"Sat 05:59 JST — still open", at(20, 5, 59), true},
		{"Sat 06:00 JST — CLOSED (weekend gap begins)", at(20, 6, 0), false},
		{"Sat midday — CLOSED", at(20, 12, 0), false},
		{"Sun 00:00 — CLOSED", at(21, 0, 0), false},
		{"Sun midday — CLOSED", at(21, 12, 0), false},
		{"Sun 23:00 — CLOSED", at(21, 23, 0), false},
		{"Mon 04:59 JST — still CLOSED (before open)", at(22, 4, 59), false},
		{"Mon 05:00 JST — OPEN (Wellington/GMO week-open)", at(22, 5, 0), true},
		{"Mon midday — open", at(22, 12, 0), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsForexOpen(tc.t); got != tc.want {
				t.Errorf("IsForexOpen(%s) = %v, want %v", tc.t.Format("Mon 15:04 MST"), got, tc.want)
			}
		})
	}
}

// The input may be in any zone (the scheduler passes time.Now() in local/UTC). The
// helper must convert to JST internally.
func TestIsForexOpen_ConvertsInputZone(t *testing.T) {
	// Sunday 2026-06-21 20:00 UTC → Monday 05:00 JST → market opens.
	utcOpen := time.Date(2026, 6, 21, 20, 0, 0, 0, time.UTC)
	if !IsForexOpen(utcOpen) {
		t.Errorf("Sun 20:00 UTC (= Mon 05:00 JST) must be OPEN")
	}
	// Saturday 2026-06-20 00:00 UTC → Saturday 09:00 JST → closed (weekend).
	utcClosed := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	if IsForexOpen(utcClosed) {
		t.Errorf("Sat 00:00 UTC (= Sat 09:00 JST) must be CLOSED")
	}
}

// TradingDayStartJST returns the start of the CURRENT trading day: 06:00 JST (the GMO
// 本日損益 boundary the dashboard also uses). Before 06:00 JST the trading day began at
// 06:00 JST YESTERDAY. The LLM decision cycle uses it to feed "today's closed trades" (the
// 同日2敗打ち止め rule) to the decision prompt with an unambiguous day boundary.
func TestTradingDayStartJST(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"after 6am JST → today 06:00", time.Date(2026, 7, 9, 15, 30, 0, 0, jst), time.Date(2026, 7, 9, 6, 0, 0, 0, jst)},
		{"exactly 06:00 JST → today 06:00", time.Date(2026, 7, 9, 6, 0, 0, 0, jst), time.Date(2026, 7, 9, 6, 0, 0, 0, jst)},
		{"before 6am JST → YESTERDAY 06:00", time.Date(2026, 7, 9, 4, 42, 0, 0, jst), time.Date(2026, 7, 8, 6, 0, 0, 0, jst)},
		{"UTC input converted (7/8 22:30 UTC = 7/9 07:30 JST)", time.Date(2026, 7, 8, 22, 30, 0, 0, time.UTC), time.Date(2026, 7, 9, 6, 0, 0, 0, jst)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TradingDayStartJST(tc.now); !got.Equal(tc.want) {
				t.Errorf("TradingDayStartJST(%v) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}
}
