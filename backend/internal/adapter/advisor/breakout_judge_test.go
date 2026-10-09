package advisor

import (
	"encoding/json"
	"strings"
	"testing"

	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
)

func TestBuildJudgePayload(t *testing.T) {
	p := strategy.BreakoutProposal{
		Found: true, Label: strategy.BreakoutTrendCont, Side: order.SideBuy,
		Level: 150.50, Entry: 150.55, Invalidation: 149.70, ATRPips: 80, RR: 3.1,
	}
	out, err := BuildJudgePayload("USD_JPY", p, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "breakout-advisor") {
		t.Errorf("payload must instruct the breakout-advisor subagent")
	}
	if !strings.Contains(s, "no_trade") {
		t.Errorf("payload must carry the no_trade default doctrine")
	}
	// the JSON input must be present and parseable
	idx := strings.Index(s, "{")
	if idx < 0 {
		t.Fatalf("no JSON block in payload")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(s[idx:strings.LastIndex(s, "}")+1]), &parsed); err != nil {
		t.Fatalf("embedded input not valid JSON: %v", err)
	}
	if parsed["symbol"] != "USD_JPY" {
		t.Errorf("symbol missing in payload JSON: %v", parsed["symbol"])
	}
}

func TestBreakoutJudge_VerdictRoundTrip(t *testing.T) {
	// ParseBreakoutDecision -> Verdict mapping (the boundary the judge relies on).
	dec, _ := ParseBreakoutDecision([]byte("decision:\n  go: true\n  label: trend_continuation\n  side: buy\n  level: 150\n  atr_pips: 50\n  invalidation: 149\n"))
	v := dec.Verdict()
	if !v.Go || v.Label != strategy.BreakoutTrendCont || v.Side != order.SideBuy {
		t.Errorf("verdict mapping wrong: %+v", v)
	}
}
