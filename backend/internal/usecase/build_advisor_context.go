package usecase

import (
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
)

// BuildEventContext translates the nearest active/upcoming calendar event into
// the prompt-facing market.EventContext. Returns nil when there is no event in
// [now, now+horizon] (and none active).
func BuildEventContext(cal *config.EventCalendar, now time.Time, horizon time.Duration) *market.EventContext {
	if cal == nil {
		return nil
	}
	ev, mins, inWindow, ok := cal.ActiveOrUpcoming(now, horizon)
	if !ok || ev == nil {
		return nil
	}
	policy := ev.Policy
	if policy == "" {
		policy = config.EventPolicyFreeze
	}
	return &market.EventContext{
		Name:         ev.Name,
		Policy:       policy,
		MinutesUntil: mins,
		InWindow:     inWindow,
	}
}

// BuildRecentDecisions maps recent strategy-config records into the compact
// prompt view so Claude can see its own past decisions (regime/strategy + how
// long ago) and decide whether to extend or replace the current plan. Records
// are expected most-recent-first (StrategyConfigRepository.ListRecent order).
// Returns nil for nil/empty input.
func BuildRecentDecisions(records []port.StrategyConfigRecordWithMeta, now time.Time) []market.RecentDecision {
	if len(records) == 0 {
		return nil
	}
	out := make([]market.RecentDecision, 0, len(records))
	for _, r := range records {
		age := int(now.Sub(r.CreatedAt).Round(time.Minute) / time.Minute)
		if age < 0 {
			age = 0
		}
		out = append(out, market.RecentDecision{
			RegimeType:   r.MarketRegimeType,
			StrategyName: r.StrategyName,
			AgeMinutes:   age,
		})
	}
	return out
}
