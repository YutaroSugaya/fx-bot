package config

import (
	"strings"
	"testing"
	"time"
)

// event_retrigger: on top of the hourly LLM decision
// cycle, re-run the cycle when (a) a position closes and (b) a large price move
// happens — the hourly cadence alone goes stale. The section is config-driven so
// the operator can tune/kill it without a rebuild; omitted = fully OFF (back-compat).
func TestLoadBotConfig_ParsesEventRetrigger(t *testing.T) {
	body := validBotConfigYAML + `
llm_decision:
  enabled: true
  event_retrigger:
    enabled: true
    on_position_close: true
    move_pips: 25
    move_window_minutes: 30
    move_cooldown_minutes: 20
    close_cooldown_minutes: 3
`
	cfg, err := LoadBotConfig(writeTempYAML(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	er := cfg.LLMDecision.EventRetrigger
	if !er.Enabled {
		t.Error("Enabled = false, want true")
	}
	if !er.OnPositionClose {
		t.Error("OnPositionClose = false, want true")
	}
	if er.MovePips != 25 {
		t.Errorf("MovePips = %v, want 25", er.MovePips)
	}
	if got := er.MoveWindow(); got != 30*time.Minute {
		t.Errorf("MoveWindow() = %v, want 30m", got)
	}
	if got := er.MoveCooldown(); got != 20*time.Minute {
		t.Errorf("MoveCooldown() = %v, want 20m", got)
	}
	if got := er.CloseCooldown(); got != 3*time.Minute {
		t.Errorf("CloseCooldown() = %v, want 3m", got)
	}
}

// Omitted section = zero value = feature OFF, and the duration accessors still
// return sane defaults (the wiring uses them unconditionally).
func TestLoadBotConfig_EventRetriggerDefaultOff(t *testing.T) {
	body := validBotConfigYAML + `
llm_decision:
  enabled: true
`
	cfg, err := LoadBotConfig(writeTempYAML(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	er := cfg.LLMDecision.EventRetrigger
	if er.Enabled || er.OnPositionClose || er.MovePips != 0 {
		t.Errorf("omitted event_retrigger should be fully off; got %+v", er)
	}
	if got := er.MoveWindow(); got != 30*time.Minute {
		t.Errorf("default MoveWindow() = %v, want 30m", got)
	}
	if got := er.MoveCooldown(); got != 20*time.Minute {
		t.Errorf("default MoveCooldown() = %v, want 20m", got)
	}
	if got := er.CloseCooldown(); got != 3*time.Minute {
		t.Errorf("default CloseCooldown() = %v, want 3m", got)
	}
}

// Negative numbers are typos, not intents — fail loudly at load time.
func TestLoadBotConfig_RejectsNegativeEventRetrigger(t *testing.T) {
	cases := []struct{ name, field string }{
		{"negative move_pips", "move_pips: -1"},
		{"negative move_window", "move_window_minutes: -5"},
		{"negative move_cooldown", "move_cooldown_minutes: -5"},
		{"negative close_cooldown", "close_cooldown_minutes: -5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := validBotConfigYAML + `
llm_decision:
  enabled: true
  event_retrigger:
    enabled: true
    ` + tc.field + `
`
			_, err := LoadBotConfig(writeTempYAML(t, body))
			if err == nil || !strings.Contains(err.Error(), "event_retrigger") {
				t.Errorf("want load error mentioning event_retrigger, got %v", err)
			}
		})
	}
}
