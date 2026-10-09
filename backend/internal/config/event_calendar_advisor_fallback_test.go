package config

import (
	"testing"
	"time"
)

// When the advisor is DISABLED there is no one to arm/manage a breakout-policy
// event, so it must fall back to freeze. Otherwise a breakout-policy window (e.g.
// NFP) with the advisor off lets trades be decided inside it with no manager —
// exactly the exposure this closes.
func TestInFreezeWindowConsideringAdvisor(t *testing.T) {
	at := time.Date(2026, 6, 5, 21, 30, 0, 0, time.UTC)
	now := at // inside window
	breakout := &EventCalendar{Events: []CalendarEvent{
		{Name: "NFP", At: at, PreMinutes: 5, PostMinutes: 5, Policy: EventPolicyBreakout},
	}}
	freeze := &EventCalendar{Events: []CalendarEvent{
		{Name: "CPI", At: at, PreMinutes: 5, PostMinutes: 5, Policy: EventPolicyFreeze},
	}}

	// breakout + advisor ON → attack mode, no freeze.
	if breakout.InFreezeWindowConsideringAdvisor(now, true) {
		t.Error("advisor ON: breakout event must NOT freeze (attack mode)")
	}
	// breakout + advisor OFF → fall back to freeze.
	if !breakout.InFreezeWindowConsideringAdvisor(now, false) {
		t.Error("advisor OFF: breakout event must fall back to freeze")
	}
	// freeze event always freezes regardless of advisor state.
	if !freeze.InFreezeWindowConsideringAdvisor(now, true) {
		t.Error("freeze event must freeze even with advisor ON")
	}
	if !freeze.InFreezeWindowConsideringAdvisor(now, false) {
		t.Error("freeze event must freeze with advisor OFF")
	}
	// outside any window → no freeze.
	if breakout.InFreezeWindowConsideringAdvisor(at.Add(time.Hour), false) {
		t.Error("outside window must not freeze")
	}
	// nil receiver is safe.
	var nilCal *EventCalendar
	if nilCal.InFreezeWindowConsideringAdvisor(now, false) {
		t.Error("nil calendar must not freeze")
	}
}
