package command

import (
	"context"
	"testing"

	"fx-bot/backend/internal/domain/strategy"
)

// advisor_v2.exclusive: EntriesDisabled must short-circuit the engine-tick entry path
// (no evaluate, no order) and report it, leaving exits (separate path) untouched.
func TestTradingCycle_EntriesDisabled(t *testing.T) {
	tc := &TradingCycle{Evaluator: &EvaluateEntry{}, EntriesDisabled: true}
	res, err := tc.Execute(context.Background(), TradingCycleInput{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Decision != strategy.DecisionNoTrade || res.Executed {
		t.Fatalf("entries-disabled must not enter: %+v", res)
	}
	if res.GateReason != "engine_entries_disabled_v2_exclusive" {
		t.Errorf("GateReason = %q", res.GateReason)
	}
}
