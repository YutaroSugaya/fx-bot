package usecase

import (
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
)

func TestBuildEventContext(t *testing.T) {
	at := time.Date(2026, 6, 5, 13, 0, 0, 0, time.UTC)
	cal := &config.EventCalendar{
		Events: []config.CalendarEvent{
			{Name: "NFP", At: at, PreMinutes: 30, PostMinutes: 30, Policy: config.EventPolicyBreakout},
		},
	}
	horizon := 60 * time.Minute

	// 窓内 (25 分前)
	ec := BuildEventContext(cal, at.Add(-25*time.Minute), horizon)
	if ec == nil || ec.Name != "NFP" || !ec.InWindow || ec.Policy != "breakout" || ec.MinutesUntil != 25 {
		t.Fatalf("in-window: got %+v", ec)
	}
	// horizon 外 → nil
	if got := BuildEventContext(cal, at.Add(-120*time.Minute), horizon); got != nil {
		t.Errorf("outside horizon should be nil, got %+v", got)
	}
	// nil calendar → nil
	if got := BuildEventContext(nil, at, horizon); got != nil {
		t.Errorf("nil calendar should be nil, got %+v", got)
	}
}

func TestBuildRecentDecisions(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	recs := []port.StrategyConfigRecordWithMeta{
		{StrategyConfigRecord: port.StrategyConfigRecord{
			MarketRegimeType: "trend_up", StrategyName: "momentum_pullback",
		}, CreatedAt: now.Add(-10 * time.Minute)},
		{StrategyConfigRecord: port.StrategyConfigRecord{
			MarketRegimeType: "range", StrategyName: "no_trade",
		}, CreatedAt: now.Add(-40 * time.Minute)},
	}
	out := BuildRecentDecisions(recs, now)
	if len(out) != 2 {
		t.Fatalf("got %d want 2", len(out))
	}
	if out[0].RegimeType != "trend_up" || out[0].StrategyName != "momentum_pullback" || out[0].AgeMinutes != 10 {
		t.Errorf("row0: %+v", out[0])
	}
	if out[1].AgeMinutes != 40 {
		t.Errorf("row1 age: %+v", out[1])
	}
	// nil-safe
	if got := BuildRecentDecisions(nil, now); got != nil {
		t.Errorf("nil records should yield nil, got %+v", got)
	}
}
