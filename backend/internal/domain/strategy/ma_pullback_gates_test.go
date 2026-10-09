package strategy

import (
	"testing"
	"time"

	"fx-bot/backend/internal/domain/market"
)

// firstBlockedGate returns the Key of the first gate with OK=false (the active
// blocker), or "" when every gate passes.
func firstBlockedGate(gates []Gate) string {
	for _, g := range gates {
		if !g.OK {
			return g.Key
		}
	}
	return ""
}

// reasonToGate maps an Evaluate reason to the gate Key it should block on, so
// the diagnostic funnel can't drift from the real decision path.
var reasonToGate = map[string]string{
	"spread_too_wide":         "spread",
	"insufficient_1h_candles": "trend",
	"insufficient_5m_candles": "data",
	"no_trend":                "trend",
	"not_in_ma_zone":          "zone",
	"wrong_side_of_ma":        "side",
	"no_confluence":           "confluence",
	"no_rebound":              "rebound",
}

func TestMAPullback_Gates_FirstBlockMatchesEvaluateReason(t *testing.T) {
	cases := map[string]func() EvalInput{
		"happy":           func() EvalInput { in, _ := maReadyInput(); return in },
		"spread_too_wide": func() EvalInput { in, _ := maReadyInput(); in.Summary.CurrentRate.SpreadPips = 2.0; return in },
		"insufficient_5m_candles": func() EvalInput {
			in, _ := maReadyInput()
			in.Candles5m = in.Candles5m[:maPBMin5mBars-1]
			return in
		},
		"no_trend": func() EvalInput {
			in, _ := maReadyInput()
			t0 := in.Candles1h[0].OpenTime
			flat := make([]market.Candle, len(in.Candles1h))
			for i := range flat {
				flat[i] = market.Candle{Symbol: "USD_JPY", Interval: time.Hour,
					OpenTime: t0.Add(time.Duration(i) * time.Hour),
					Open:     150.00, High: 150.00, Low: 150.00, Close: 150.00}
			}
			in.Candles1h = flat
			return in
		},
		"not_in_ma_zone": func() EvalInput {
			in, sma := maReadyInput()
			mid := sma + 1.0
			in.Summary.CurrentRate = market.CurrentRate{Bid: mid - 0.005, Ask: mid + 0.005, SpreadPips: 1.0}
			return in
		},
		"wrong_side_of_ma": func() EvalInput {
			in, sma := maReadyInput()
			mid := sma - 1.0*maPip // in zone but below the SMA in an uptrend
			in.Summary.CurrentRate = market.CurrentRate{Bid: mid - 0.5*maPip, Ask: mid + 0.5*maPip, SpreadPips: 1.0}
			return in
		},
		"no_confluence": func() EvalInput {
			in, _ := maReadyInput()
			in.Candles5m[230].Low = in.Candles5m[230].Close - maPip
			return in
		},
		"no_rebound": func() EvalInput {
			in, _ := maReadyInput()
			last := len(in.Candles5m) - 1
			in.Candles5m[last].Open = in.Candles5m[last].Close + maPip
			return in
		},
	}

	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			in := build()
			sig := MAPullback{}.Evaluate(in)
			gates := MAPullback{}.Gates(in)
			if len(gates) == 0 {
				t.Fatalf("Gates returned none")
			}
			blocked := firstBlockedGate(gates)
			if name == "happy" {
				if sig.Decision != DecisionEnter {
					t.Fatalf("happy: expected ENTER, got %s (%s)", sig.Decision, sig.Reason)
				}
				if blocked != "" {
					t.Fatalf("happy: expected all gates pass, but %q blocked", blocked)
				}
				return
			}
			wantGate := reasonToGate[sig.Reason]
			if wantGate == "" {
				t.Fatalf("reason %q has no gate mapping", sig.Reason)
			}
			if blocked != wantGate {
				t.Errorf("reason=%s → first blocked gate = %q, want %q", sig.Reason, blocked, wantGate)
			}
		})
	}
}
