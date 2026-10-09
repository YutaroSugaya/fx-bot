package config

import (
	"strings"
	"testing"
	"time"
)

// ReflectionSince computes the lower time bound for trades fed to the Reflexion
// loop: the later of (now-lookback) and the configured ReflectionStartAt. This
// keeps the loop from "learning" off trades that predate the LLM loop going
// live (they belong to other strategies and would poison the playbook).
func TestLLMDecisionSection_ReflectionSince(t *testing.T) {
	now := time.Date(2026, 6, 22, 3, 0, 0, 0, time.UTC)
	lookback := 30 * 24 * time.Hour
	floor := now.Add(-lookback) // 2026-05-23T03:00:00Z

	cases := []struct {
		name    string
		startAt string
		want    time.Time
	}{
		{"empty → lookback floor (back-compat)", "", floor},
		{"start after floor → start", "2026-06-19T16:00:00Z", time.Date(2026, 6, 19, 16, 0, 0, 0, time.UTC)},
		{"start before floor → floor", "2026-01-01T00:00:00Z", floor},
		{"malformed → floor (load-time validate rejects it first)", "not-a-time", floor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := LLMDecisionSection{ReflectionStartAt: tc.startAt}
			if got := s.ReflectionSince(now, lookback); !got.Equal(tc.want) {
				t.Errorf("ReflectionSince(%q) = %v, want %v", tc.startAt, got, tc.want)
			}
		})
	}
}

// StartAtTime parses reflection_start_at into a time (ok=false on empty/malformed).
// Reused by both the reflection floor and the dashboard P&L epoch.
func TestLLMDecisionSection_StartAtTime(t *testing.T) {
	if _, ok := (LLMDecisionSection{}).StartAtTime(); ok {
		t.Error("empty ReflectionStartAt should return ok=false")
	}
	if _, ok := (LLMDecisionSection{ReflectionStartAt: "not-a-time"}).StartAtTime(); ok {
		t.Error("malformed ReflectionStartAt should return ok=false")
	}
	got, ok := (LLMDecisionSection{ReflectionStartAt: "2026-06-19T16:00:00Z"}).StartAtTime()
	if !ok || !got.Equal(time.Date(2026, 6, 19, 16, 0, 0, 0, time.UTC)) {
		t.Errorf("StartAtTime = %v, ok=%v; want 2026-06-19T16:00:00Z, true", got, ok)
	}
}

const llmDecisionReflectionBlock = `
llm_decision:
  enabled: true
  interval_minutes: 60
  quantity: 1000
  symbols: [USD_JPY]
  reflection_start_at: "REFLECTION_START_AT"
`

func TestLoadBotConfig_RejectsMalformedReflectionStartAt(t *testing.T) {
	body := validBotConfigYAML + strings.Replace(llmDecisionReflectionBlock, "REFLECTION_START_AT", "2026-06-19 16:00", 1)
	path := writeTempYAML(t, body)
	if _, err := LoadBotConfig(path); err == nil || !strings.Contains(err.Error(), "reflection_start_at") {
		t.Fatalf("expected reflection_start_at validation error, got %v", err)
	}
}

func TestLoadBotConfig_AcceptsWellFormedReflectionStartAt(t *testing.T) {
	body := validBotConfigYAML + strings.Replace(llmDecisionReflectionBlock, "REFLECTION_START_AT", "2026-06-19T16:00:00Z", 1)
	path := writeTempYAML(t, body)
	cfg, err := LoadBotConfig(path)
	if err != nil {
		t.Fatalf("well-formed reflection_start_at should load: %v", err)
	}
	if cfg.LLMDecision.ReflectionStartAt != "2026-06-19T16:00:00Z" {
		t.Errorf("ReflectionStartAt: got %q", cfg.LLMDecision.ReflectionStartAt)
	}
}

// ReflectionOn gates the Reflexion learning loop (playbook auto-rewrite). When the playbook is
// human-approved text only, the config sets reflection_enabled: false and the scheduler never starts. Pointer-bool: omitted → TRUE (back-compat with configs predating the
// flag); explicit false turns the loop off for good.
func TestLLMDecisionSection_ReflectionOn(t *testing.T) {
	tru, fls := true, false
	if !(LLMDecisionSection{}).ReflectionOn() {
		t.Error("omitted (nil) must default to reflection=on (back-compat)")
	}
	if !(LLMDecisionSection{ReflectionEnabled: &tru}).ReflectionOn() {
		t.Error("explicit true must be reflection=on")
	}
	if (LLMDecisionSection{ReflectionEnabled: &fls}).ReflectionOn() {
		t.Error("explicit false must turn the Reflexion loop OFF")
	}
}

func TestLoadBotConfig_ParsesReflectionEnabledFalse(t *testing.T) {
	body := validBotConfigYAML + `
llm_decision:
  enabled: true
  interval_minutes: 60
  quantity: 1000
  symbols: [USD_JPY]
  reflection_enabled: false
`
	path := writeTempYAML(t, body)
	cfg, err := LoadBotConfig(path)
	if err != nil {
		t.Fatalf("config with reflection_enabled:false should load: %v", err)
	}
	if cfg.LLMDecision.ReflectionOn() {
		t.Error("reflection_enabled:false must parse to ReflectionOn()==false")
	}
}

// SingleAgentDecision collapses the per-cycle decision to ONE claude agent (no market-regime/
// trade-decider sub-agent panel). It defaults to TRUE (single-agent is the default);
// an explicit false reverts to the panel. Pointer-bool so "omitted" is distinguishable from "false".
func TestLLMDecisionSection_SingleAgentDecision(t *testing.T) {
	tru, fls := true, false
	if !(LLMDecisionSection{}).SingleAgentDecision() {
		t.Error("omitted (nil) must default to single-agent=true")
	}
	if !(LLMDecisionSection{DecisionSingleAgent: &tru}).SingleAgentDecision() {
		t.Error("explicit true must be single-agent=true")
	}
	if (LLMDecisionSection{DecisionSingleAgent: &fls}).SingleAgentDecision() {
		t.Error("explicit false must REVERT to panel (single-agent=false)")
	}
}
