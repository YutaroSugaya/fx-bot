package market

// spreadSpikeMultiplier is the threshold from prompts/generate_strategy_config.md:
//
//	「current_rate.spread_pips > summary_1h.avg_spread_pips * 2 なら no_trade」
//
// 値の変更は skill 側の閾値と同期させること。
const spreadSpikeMultiplier = 2.0

// IsSpreadSpike reports whether the current spread is more than
// spreadSpikeMultiplier times the 1h average — the same condition the
// orchestrator prompt asks Claude to honour with no_trade.
//
// Callers (AdvisorCycle) emit
// an audit log when this returns true so we can observe whether Claude is
// actually honouring the spike rule (= "通り抜け疑い" 検知)。
//
// Returns false when avg is non-positive (= insufficient sample history) so
// startup ticks don't generate noisy false positives.
func IsSpreadSpike(s *MarketSummary) bool {
	if s == nil {
		return false
	}
	avg := s.Summary1h.AvgSpreadPips
	if avg <= 0 {
		return false
	}
	return s.CurrentRate.SpreadPips > avg*spreadSpikeMultiplier
}
