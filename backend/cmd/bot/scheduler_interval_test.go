package main

import (
	"testing"
	"time"

	"fx-bot/backend/internal/app"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
)

// --- bot-wide scheduler interval across all symbols ---
//
// The single scheduler fans out to every bundle on each tick. When any
// active config requests a tighter NextAdvisorRunInMinutes, the bot must
// honour the shortest positive one — otherwise a fast-evaluating symbol
// gets starved waiting for a slow-evaluating sibling. Picking primary's
// value alone (pre-fix) ignored sibling requests entirely.

func TestNextScheduleInterval_MinPositiveAcrossSymbols(t *testing.T) {
	cases := []struct {
		name    string
		configs map[string]int // symbol → NextAdvisorRunInMinutes
		want    time.Duration
	}{
		{
			"single symbol passes its interval through",
			map[string]int{"USD_JPY": 30},
			30 * time.Minute,
		},
		{
			"two symbols: take the shorter positive interval",
			map[string]int{"USD_JPY": 30, "EUR_JPY": 15},
			15 * time.Minute,
		},
		{
			"non-positive entries ignored, smallest positive wins",
			map[string]int{"USD_JPY": 30, "EUR_JPY": 0, "GBP_JPY": 10},
			10 * time.Minute,
		},
		{
			"all zero/negative → 0 (scheduler falls back to default interval)",
			map[string]int{"USD_JPY": 0, "EUR_JPY": -5},
			0,
		},
		{
			"empty holder → 0",
			map[string]int{},
			0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			holder := &app.ActiveConfigHolder{}
			for sym, mins := range tc.configs {
				cfg := &config.StrategyConfig{NextAdvisorRunInMinutes: mins}
				holder.Set(sym, cfg)
			}
			got := nextScheduleInterval(holder, nil)
			if got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

// parse_error 中は config の next_advisor_run_in_minutes より優先して
// 10 分後に再走する。parse_error が続く間 60 分間隔のままだと、何時間も
// stale config で動き続けるため。
func TestNextScheduleInterval_ParseError_OverridesTo10Min(t *testing.T) {
	holder := &app.ActiveConfigHolder{}
	// config 側は 60 分要求しているが、parse_error monitor が flag を上げて
	// いる間は 10 分が優先される。
	holder.Set("USD_JPY", &config.StrategyConfig{NextAdvisorRunInMinutes: 60})
	monitor := app.NewParseErrorMonitor()
	monitor.Record("USD_JPY", port.AdvisorRunStatusParseError, false)

	got := nextScheduleInterval(holder, monitor)
	if got != 10*time.Minute {
		t.Errorf("parse_error active: got %v want 10m", got)
	}
}

// monitor flag が下りていれば (success / nil) 通常 cadence を返す。
func TestNextScheduleInterval_NoParseError_FallsBackToConfig(t *testing.T) {
	holder := &app.ActiveConfigHolder{}
	holder.Set("USD_JPY", &config.StrategyConfig{NextAdvisorRunInMinutes: 30})
	monitor := app.NewParseErrorMonitor()
	monitor.Record("USD_JPY", port.AdvisorRunStatusSuccess, false)

	got := nextScheduleInterval(holder, monitor)
	if got != 30*time.Minute {
		t.Errorf("no parse_error: got %v want 30m (from config)", got)
	}
}

// nil monitor は legacy 配線互換 (オプショナル)。
func TestNextScheduleInterval_NilMonitor_BehavesAsBefore(t *testing.T) {
	holder := &app.ActiveConfigHolder{}
	holder.Set("USD_JPY", &config.StrategyConfig{NextAdvisorRunInMinutes: 15})
	got := nextScheduleInterval(holder, nil)
	if got != 15*time.Minute {
		t.Errorf("nil monitor: got %v want 15m (config passthrough)", got)
	}
}
