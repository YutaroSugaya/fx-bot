package backtest

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// SliceMetrics groups trades by a string key (e.g. "09" for hour-of-day-JST,
// "Mon" for weekday) and computes Metrics for each bucket. The result is a
// slice of (key, metrics) pairs sorted by key ascending.
//
// Used by the backtest CLI's -slice flag to surface time-of-day / weekday
// edge that an aggregate PF hides.
type SliceMetrics struct {
	Key     string
	Metrics Metrics
}

// jst is the +09:00 fixed offset for slice keys. Backtest trades carry
// OpenedAt in UTC (DB) — we want human-readable JST buckets.
var jst = time.FixedZone("JST", 9*60*60)

// HourKeyJST returns "00".."23" — entry hour-of-day in JST.
func HourKeyJST(t Trade) string {
	return fmt.Sprintf("%02d", t.OpenedAt.In(jst).Hour())
}

// WeekdayKeyJST returns "Mon".."Sun" with a numeric prefix so the default
// string sort puts Mon→Sun in order.
func WeekdayKeyJST(t Trade) string {
	wd := t.OpenedAt.In(jst).Weekday()
	// time.Weekday: Sunday=0, Monday=1, ..., Saturday=6.
	// We want Monday first because FX trades Mon-Fri.
	idx := (int(wd) + 6) % 7 // Mon=0, ..., Sun=6
	names := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	return fmt.Sprintf("%d-%s", idx, names[idx])
}

// SliceBy groups trades by keyFn and returns sorted (key, metrics) pairs.
// Empty buckets are not emitted — only keys with at least one trade appear.
func SliceBy(trades []Trade, keyFn func(Trade) string) []SliceMetrics {
	if len(trades) == 0 {
		return nil
	}
	buckets := map[string][]Trade{}
	for _, t := range trades {
		k := keyFn(t)
		buckets[k] = append(buckets[k], t)
	}
	out := make([]SliceMetrics, 0, len(buckets))
	for k, ts := range buckets {
		out = append(out, SliceMetrics{Key: k, Metrics: ComputeMetrics(ts)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// PrettySlice renders a SliceMetrics list as an aligned table. The label is
// the slice's display name ("Hour-of-Day JST", "Weekday JST"). Buckets with
// fewer than 5 trades are flagged with "(n<5)" so readers do not over-trust
// the PF.
func PrettySlice(label string, ss []SliceMetrics) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("\n--- By %s ---\n", label))
	b.WriteString(fmt.Sprintf("%-8s %7s %7s %9s %10s\n", "key", "trades", "win%", "PF", "expect"))
	for _, s := range ss {
		flag := ""
		if s.Metrics.SampleSize < 5 {
			flag = " (n<5)"
		}
		b.WriteString(fmt.Sprintf("%-8s %7d %6.1f%% %9s %10s%s\n",
			s.Key,
			s.Metrics.SampleSize,
			s.Metrics.WinRate*100,
			formatFloatNoNaN(s.Metrics.ProfitFactor),
			formatFloatNoNaN(s.Metrics.Expectancy),
			flag,
		))
	}
	return b.String()
}

// formatFloatNoNaN is a slice-table-friendly variant: empty buckets show "-"
// instead of "0.0000" / "NaN" which crowds the column.
func formatFloatNoNaN(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "-"
	default:
		return fmt.Sprintf("%.3f", v)
	}
}
