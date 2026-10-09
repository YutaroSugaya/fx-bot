package app

import (
	"testing"
	"time"

	"fx-bot/backend/internal/port"
)

func TestDailyDDAlert(t *testing.T) {
	// DailyDDAlert takes NET daily realized loss (positive,
	// = the gate's SumClosedLossJPYSince measure = Σ|net-negative close PnL, fee-inclusive|)
	// so it can never contradict the entry-gate brake.
	cases := []struct {
		name      string
		dailyLoss float64 // positive = JPY lost today (net of fee, gross of wins)
		maxLoss   int
		wantAlert bool
	}{
		{"disabled (max 0)", 9999, 0, false},
		{"within cap", 1500, 2000, false},
		{"exactly at cap (breach)", 2000, 2000, true},
		{"beyond cap", 2500, 2000, true},
		{"no loss", 0, 2000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DailyDDAlert("USD_JPY", tc.dailyLoss, tc.maxLoss)
			if (got != nil) != tc.wantAlert {
				t.Fatalf("alert=%v want %v (got %+v)", got != nil, tc.wantAlert, got)
			}
			if got != nil && got.Level != port.LevelWarn {
				t.Errorf("DD alert should be warn, got %s", got.Level)
			}
		})
	}
}

func TestNoTradeAlert(t *testing.T) {
	cases := []struct {
		name      string
		ago       time.Duration
		threshold time.Duration
		wantAlert bool
	}{
		{"disabled (threshold 0)", 10 * time.Hour, 0, false},
		{"never traded (ago<0)", -1, 6 * time.Hour, false},
		{"within threshold", 3 * time.Hour, 6 * time.Hour, false},
		{"at threshold", 6 * time.Hour, 6 * time.Hour, true},
		{"beyond threshold", 9 * time.Hour, 6 * time.Hour, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NoTradeAlert("USD_JPY", tc.ago, tc.threshold)
			if (got != nil) != tc.wantAlert {
				t.Fatalf("alert=%v want %v (got %+v)", got != nil, tc.wantAlert, got)
			}
		})
	}
}

func TestDailySummaryEvent(t *testing.T) {
	// 逆RR (RewardRisk<1) のときは body に ⚠逆RR を含める。
	ev := DailySummaryEvent(OpsSummary{
		Symbol: "USD_JPY", TradeCount: 8, WinRatePct: 87.5,
		ProfitFactor: 2.27, RewardRisk: 0.32, ExpectancyJPY: 23.875,
		NetPnLJPY: 191, DailyPnLJPY: 191,
	})
	if ev.Title != "daily_summary" || ev.Level != port.LevelInfo {
		t.Fatalf("unexpected title/level: %+v", ev)
	}
	if !contains(ev.Body, "逆RR") {
		t.Errorf("body should flag 逆RR when reward_risk<1: %q", ev.Body)
	}
	if !contains(ev.Body, "USD_JPY") {
		t.Errorf("body should include symbol: %q", ev.Body)
	}

	// RR>=1 のときは ⚠逆RR を付けない。
	ev2 := DailySummaryEvent(OpsSummary{Symbol: "EUR_JPY", TradeCount: 5, RewardRisk: 1.5, ProfitFactor: 1.8})
	if contains(ev2.Body, "逆RR") {
		t.Errorf("body should NOT flag 逆RR when reward_risk>=1: %q", ev2.Body)
	}

	// N/A (RR=0, PF=0) は "—" 表記。
	ev3 := DailySummaryEvent(OpsSummary{Symbol: "USD_JPY", TradeCount: 0})
	if !contains(ev3.Body, "—") {
		t.Errorf("body should show — for N/A metrics: %q", ev3.Body)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
