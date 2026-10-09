package backtest

import (
	"fmt"
	"math"
	"strings"
)

// PrettyResult renders a Result as a human-readable multi-line string for the
// backtest CLI's default output mode. JSON callers should use encoding/json
// directly on Result instead.
func PrettyResult(r Result) string {
	var b strings.Builder
	b.WriteString("==================== Backtest Result ====================\n")
	b.WriteString(fmt.Sprintf("SampleSize    : %d trades\n", r.Metrics.SampleSize))
	b.WriteString(fmt.Sprintf("ProfitFactor  : %s\n", formatFloat(r.Metrics.ProfitFactor)))
	b.WriteString(fmt.Sprintf("Expectancy    : %s JPY / trade\n", formatFloat(r.Metrics.Expectancy)))
	b.WriteString(fmt.Sprintf("MaxDrawdown   : %s JPY\n", formatFloat(r.Metrics.MaxDrawdown)))
	b.WriteString(fmt.Sprintf("WinRate       : %.1f%%\n", r.Metrics.WinRate*100))
	b.WriteString(fmt.Sprintf("AmbiguousBars : %d (TP and SL hit in same bar)\n", r.AmbiguousBars))

	if len(r.Trades) > 0 {
		b.WriteString("\n--- Trades (first 10) ---\n")
		for i, tr := range r.Trades {
			if i >= 10 {
				b.WriteString(fmt.Sprintf("... and %d more\n", len(r.Trades)-10))
				break
			}
			b.WriteString(fmt.Sprintf("  [%2d] %-4s entry=%.3f exit=%.3f pips=%+6.2f jpy=%+8.2f close=%-12s opened=%s\n",
				i+1, tr.Side, tr.EntryPrice, tr.ExitPrice,
				tr.ProfitLossPips, tr.ProfitLossJPY, tr.CloseReason,
				tr.OpenedAt.Format("2006-01-02 15:04"),
			))
		}
	} else {
		b.WriteString("\n(no trades recorded — strategy may need adjustment, or candle window is too short)\n")
	}
	b.WriteString("=========================================================\n")
	return b.String()
}

// PrettyAccount renders an AccountResult's combined section + a short equity
// curve tail for the multi-symbol CLI output. Per-symbol blocks are printed
// separately by the caller using PrettyResult on each AccountResult.Symbols
// entry.
func PrettyAccount(a AccountResult) string {
	var b strings.Builder
	b.WriteString("\n==================== Account (combined) ====================\n")
	b.WriteString(fmt.Sprintf("Symbols       : %d\n", len(a.Symbols)))
	b.WriteString(fmt.Sprintf("SampleSize    : %d trades\n", a.Combined.SampleSize))
	b.WriteString(fmt.Sprintf("ProfitFactor  : %s\n", formatFloat(a.Combined.ProfitFactor)))
	b.WriteString(fmt.Sprintf("Expectancy    : %s JPY / trade\n", formatFloat(a.Combined.Expectancy)))
	b.WriteString(fmt.Sprintf("MaxDrawdown   : %s JPY (chronological merge across symbols)\n",
		formatFloat(a.Combined.MaxDrawdown)))
	b.WriteString(fmt.Sprintf("WinRate       : %.1f%%\n", a.Combined.WinRate*100))
	if n := len(a.Equity); n > 0 {
		final := a.Equity[n-1]
		b.WriteString(fmt.Sprintf("EquityPoints  : %d (final equity %s JPY at %s)\n",
			n, formatFloat(final.Equity), final.Time.Format("2006-01-02 15:04")))
	}
	b.WriteString("============================================================\n")
	return b.String()
}

// formatFloat renders a float64 with Inf/NaN handled explicitly so the CLI
// output never shows raw "+Inf" garbled across platforms.
func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	default:
		return fmt.Sprintf("%.4f", v)
	}
}
