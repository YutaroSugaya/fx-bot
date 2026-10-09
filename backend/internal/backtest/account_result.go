package backtest

import "sort"

// AccountResult bundles a multi-symbol backtest run: one Result per symbol
// plus the account-wide combined Metrics and equity curve (= cumulative PnL
// across every symbol's trades merged chronologically by ClosedAt).
//
// Use AggregateAccountResult to construct it from a per-symbol Result map;
// the equity curve and Combined metrics are derived, never assigned by
// the caller.
type AccountResult struct {
	Symbols  map[string]Result
	Combined Metrics
	Equity   []EquityPoint
}

// BuildEquityCurve returns (time, cumulative PnL) points in trade order.
// The input is expected chronological; callers that have not pre-sorted
// should use chronological merges (see AggregateAccountResult).
func BuildEquityCurve(trades []Trade) []EquityPoint {
	if len(trades) == 0 {
		return nil
	}
	out := make([]EquityPoint, 0, len(trades))
	var equity float64
	for _, t := range trades {
		equity += t.ProfitLossJPY
		out = append(out, EquityPoint{Time: t.ClosedAt, Equity: equity})
	}
	return out
}

// AggregateAccountResult merges every symbol's trades chronologically by
// ClosedAt and computes account-wide Metrics + equity curve. The per-symbol
// Result map is preserved by reference in AccountResult.Symbols.
func AggregateAccountResult(per map[string]Result) AccountResult {
	out := AccountResult{Symbols: per}
	if len(per) == 0 {
		return out
	}
	var merged []Trade
	for _, r := range per {
		merged = append(merged, r.Trades...)
	}
	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].ClosedAt.Before(merged[j].ClosedAt)
	})
	out.Combined = ComputeMetrics(merged)
	out.Equity = BuildEquityCurve(merged)
	return out
}
