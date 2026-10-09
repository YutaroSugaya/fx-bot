package advisor

import "testing"

func TestParseBreakoutDecision_GoBuy(t *testing.T) {
	out := []byte("```yaml\n" + `
axes:
  htf_trend: green
  rr_gate: green
decision:
  go: true
  label: trend_continuation
  side: buy
  level: 150.25
  atr_pips: 80
  invalidation: 149.45
reason_jp: 上位足上昇 + クリーンな水準ブレイク + RR3.2
` + "\n```")
	d, err := ParseBreakoutDecision(out)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !d.Go || d.Label != LabelTrendContinuation || d.Side != "BUY" {
		t.Fatalf("got %+v, want go buy trend_continuation", d)
	}
	if d.Level != 150.25 || d.ATRPips != 80 || d.Invalidation != 149.45 {
		t.Errorf("geometry mismatch: %+v", d)
	}
}

func TestParseBreakoutDecision_NoTrade(t *testing.T) {
	out := []byte("decision:\n  go: false\n  label: no_trade\n  side: none\nreason_jp: 多軸不一致\n")
	d, err := ParseBreakoutDecision(out)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if d.Go || d.Label != LabelNoTrade {
		t.Errorf("got %+v, want no-trade", d)
	}
}

func TestParseBreakoutDecision_InconsistentGoButNoTradeLabel(t *testing.T) {
	// go=true but label says no_trade -> fail-safe to no-trade.
	out := []byte("decision:\n  go: true\n  label: no_trade\n  side: buy\n  level: 150\n  atr_pips: 50\n  invalidation: 149\n")
	d, _ := ParseBreakoutDecision(out)
	if d.Go {
		t.Errorf("contradiction must collapse to no-trade, got %+v", d)
	}
}

func TestParseBreakoutDecision_GoButBadInvalidation(t *testing.T) {
	// BUY with invalidation ABOVE level is geometrically wrong -> no-trade.
	out := []byte("decision:\n  go: true\n  label: trend_continuation\n  side: buy\n  level: 150\n  atr_pips: 50\n  invalidation: 151\n")
	d, _ := ParseBreakoutDecision(out)
	if d.Go {
		t.Errorf("bad invalidation must reject, got %+v", d)
	}
}

func TestParseBreakoutDecision_GoMissingGeometry(t *testing.T) {
	out := []byte("decision:\n  go: true\n  label: trend_continuation\n  side: sell\n  level: 0\n  atr_pips: 0\n  invalidation: 0\n")
	d, _ := ParseBreakoutDecision(out)
	if d.Go {
		t.Errorf("missing geometry must reject, got %+v", d)
	}
}

func TestParseBreakoutDecision_Garbage(t *testing.T) {
	d, err := ParseBreakoutDecision([]byte("   "))
	if err == nil || d.Go {
		t.Errorf("garbage must be no-trade+err, got %+v err=%v", d, err)
	}
}

func TestParseBreakoutDecision_RealAgentOutput(t *testing.T) {
	// Verbatim shape returned by the breakout-advisor subagent in a smoke test,
	// incl. axes block + a trailing line after reason_jp. Must parse to a go BUY.
	out := []byte(`axes:
  htf_trend: green
  level_quality: green
  breakout_confirmed: green
  rr_gate: green
  alignment: green
  catalyst_clear: green
  session_ok: green
decision:
  go: true
  label: trend_continuation
  side: buy
  level: 150.50
  atr_pips: 80
  invalidation: 149.70
reason_jp: 全8軸緑でgo。
`)
	d, err := ParseBreakoutDecision(out)
	if err != nil {
		t.Fatalf("real agent output must parse, err=%v", err)
	}
	if !d.Go || d.Side != "BUY" || d.Label != LabelTrendContinuation {
		t.Fatalf("want go BUY trend_continuation, got %+v", d)
	}
	v := d.Verdict()
	if !v.Go {
		t.Errorf("verdict should be go: %+v", v)
	}
}

func TestParseBreakoutDecision_SellOK(t *testing.T) {
	out := []byte("decision:\n  go: true\n  label: trend_continuation\n  side: SELL\n  level: 1.2500\n  atr_pips: 60\n  invalidation: 1.2560\n")
	d, _ := ParseBreakoutDecision(out)
	if !d.Go || d.Side != "SELL" {
		t.Errorf("want go SELL, got %+v", d)
	}
}
