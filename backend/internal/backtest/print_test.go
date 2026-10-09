package backtest

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestPrettyResult_IncludesKeyMetrics(t *testing.T) {
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	r := Result{
		Trades: []Trade{
			tradeAt(150, t0),
			tradeAt(-50, t0.Add(time.Minute)),
			tradeAt(200, t0.Add(2*time.Minute)),
		},
	}
	r.Metrics = ComputeMetrics(r.Trades)

	out := PrettyResult(r)
	for _, needle := range []string{"ProfitFactor", "MaxDrawdown", "Expectancy", "WinRate", "SampleSize", "Trades"} {
		if !strings.Contains(out, needle) {
			t.Errorf("pretty output missing %q:\n%s", needle, out)
		}
	}
}

func TestPrettyResult_EmptyResultDoesNotPanic(t *testing.T) {
	out := PrettyResult(Result{})
	if !strings.Contains(out, "SampleSize") {
		t.Errorf("empty result should still print SampleSize: %s", out)
	}
}

func TestPrettyResult_InfiniteProfitFactorRenderedAsInf(t *testing.T) {
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	r := Result{
		Trades:  []Trade{tradeAt(100, t0), tradeAt(50, t0.Add(time.Minute))},
		Metrics: Metrics{},
	}
	r.Metrics = ComputeMetrics(r.Trades)
	if !math.IsInf(r.Metrics.ProfitFactor, 1) {
		t.Fatalf("precondition: PF should be +Inf, got %v", r.Metrics.ProfitFactor)
	}
	out := PrettyResult(r)
	if !strings.Contains(out, "+Inf") && !strings.Contains(out, "inf") {
		t.Errorf("PF=+Inf should render as +Inf or inf: %s", out)
	}
}
