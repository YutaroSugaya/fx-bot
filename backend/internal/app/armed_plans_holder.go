package app

import (
	"sync"
	"time"

	"fx-bot/backend/internal/domain/strategy"
)

// ArmedPlans is one symbol's current conditional-entry (armed) scenario set, with its expiry.
// Plans are replaced wholesale by each hourly decision; ExpiresAt is a safety net for the case
// where the next cycle never lands (CLI outage) — a scenario must not outlive the market context
// it was written in by much more than one cycle.
type ArmedPlans struct {
	Plans     []strategy.ArmedPlan
	ExpiresAt time.Time
}

// ArmedPlansHolder maps symbol → current armed plans. Mirrors PlaybookHolder: the per-tick
// watcher reads lock-free-ish via sync.Map; the (infrequent) hourly cycle writes. In-memory
// only ON PURPOSE — a restart clears all scenarios (the next cycle re-arms from fresh context),
// which is the safe default for pre-placed intent.
type ArmedPlansHolder struct {
	m sync.Map // map[string]ArmedPlans
}

// Set replaces symbol's plans wholesale. Empty plans = Clear.
func (h *ArmedPlansHolder) Set(symbol string, plans []strategy.ArmedPlan, expiresAt time.Time) {
	if len(plans) == 0 {
		h.m.Delete(symbol)
		return
	}
	h.m.Store(symbol, ArmedPlans{Plans: plans, ExpiresAt: expiresAt})
}

// Get returns symbol's current plans. ok=false when none are armed. Expiry is the CALLER's
// check (the watcher clears expired sets so the journal can record it once).
func (h *ArmedPlansHolder) Get(symbol string) (ArmedPlans, bool) {
	v, ok := h.m.Load(symbol)
	if !ok {
		return ArmedPlans{}, false
	}
	ap, _ := v.(ArmedPlans)
	return ap, true
}

// Clear removes symbol's plans (position opened / new decision / expiry).
func (h *ArmedPlansHolder) Clear(symbol string) {
	h.m.Delete(symbol)
}
