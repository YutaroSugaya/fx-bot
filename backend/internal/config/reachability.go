package config

import "fmt"

// ReachabilityTPRangeMultiple bounds an enabled trade's take_profit against the
// recent 1h range. A take_profit beyond this multiple of summary_1h.range_pips
// cannot realistically be hit inside the (30–120 min) hold window, so the
// position can only resolve via a full stop_loss or a near-breakeven early_exit
// — an asymmetric churn (small peaks, full stops, ~1:4 realized RR) that loses
// money even when the direction call is right.
//
// prompts/skills/03_tp_sl_rules.md already states "TP ≤ summary_1h.range_pips ×
// 1.3"; keeping the same 1.3 here turns that advisory line into a hard backstop
// the advisor cannot ignore. The dead-market floor falls out for free: with the
// hard_limits TP floor of 8, any 1h range below 8/1.3 ≈ 6.2 pips leaves no legal
// reachable TP, so every enabled trade downgrades to no_trade.
const ReachabilityTPRangeMultiple = 1.3

// UnreachableTPReason reports whether an ENABLED trade config should be
// downgraded to no_trade because its take_profit is unreachable for the given
// recent 1h range (in pips). The second return is false (no downgrade) when:
//   - the config is already a no_trade decision (nothing to judge), or
//   - range1hPips <= 0 — unknown range (missing candles); fail OPEN so an
//     incomplete summary never silently halts trading, or
//   - take_profit <= ReachabilityTPRangeMultiple × range1hPips (reachable).
//
// When it returns true, the string is a human-readable reason for the audit
// trail / no_trade.reason.
func UnreachableTPReason(c *StrategyConfig, range1hPips float64) (string, bool) {
	if c == nil || c.IsNoTradeDecision() {
		return "", false
	}
	if range1hPips <= 0 {
		return "", false
	}
	maxTP := ReachabilityTPRangeMultiple * range1hPips
	if c.Exit.TakeProfitPips <= maxTP {
		return "", false
	}
	return fmt.Sprintf("dead_market: TP %.1f > %.1f reachable (1h range %.1f × %.1f)",
		c.Exit.TakeProfitPips, maxTP, range1hPips, ReachabilityTPRangeMultiple), true
}

// DowngradeToNoTrade rewrites an enabled trade config into a clean no_trade
// decision, preserving identity (config_id) and the validity window so the
// no_trade stays active for the intended window. We DOWNGRADE rather than
// reject: a reject leaves the previous active config in place past its
// valid_until (freezing the bot on a stale decision), whereas a downgrade
// keeps a fresh, correct decision active. CanonicalizeNoTrade then zeroes the
// now-inert entry/exit/risk fields.
func (c *StrategyConfig) DowngradeToNoTrade(reason string) {
	if c == nil {
		return
	}
	c.Enabled = false
	c.Strategy.Name = StrategyNoTrade
	c.NoTrade = NoTradeSection{Enabled: true, Reason: reason}
	c.CanonicalizeNoTrade()
}
