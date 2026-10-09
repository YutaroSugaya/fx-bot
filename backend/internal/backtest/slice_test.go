package backtest

import (
	"strings"
	"testing"
	"time"
)

// helpers --------------------------------------------------------------------

// utc returns a UTC time at the given JST y/m/d h/m. (JST = UTC+9, so we
// subtract 9 hours to get UTC.) This keeps test inputs readable in JST
// while exercising the JST conversion path inside HourKeyJST/WeekdayKeyJST.
func utcFromJST(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h-9, min, 0, 0, time.UTC)
}

func mkTrade(opened time.Time, pnl float64) Trade {
	return Trade{
		OpenedAt:      opened,
		ClosedAt:      opened.Add(time.Hour),
		ProfitLossJPY: pnl,
	}
}

// tests ----------------------------------------------------------------------

func TestHourKeyJST_BucketsByJSTHour(t *testing.T) {
	cases := []struct {
		name string
		t    Trade
		want string
	}{
		{"midnight JST", mkTrade(utcFromJST(2026, 5, 1, 0, 0), 0), "00"},
		{"9am JST", mkTrade(utcFromJST(2026, 5, 1, 9, 0), 0), "09"},
		{"11pm JST", mkTrade(utcFromJST(2026, 5, 1, 23, 0), 0), "23"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HourKeyJST(tc.t); got != tc.want {
				t.Errorf("HourKeyJST(%v): got %q want %q", tc.t.OpenedAt, got, tc.want)
			}
		})
	}
}

func TestWeekdayKeyJST_MondayFirst(t *testing.T) {
	// 2026-05-04 = Monday (UTC). At 10am JST it is still Monday in JST.
	mon := mkTrade(utcFromJST(2026, 5, 4, 10, 0), 0)
	sun := mkTrade(utcFromJST(2026, 5, 10, 10, 0), 0) // Sunday
	if got := WeekdayKeyJST(mon); got != "0-Mon" {
		t.Errorf("Mon: got %q want 0-Mon", got)
	}
	if got := WeekdayKeyJST(sun); got != "6-Sun" {
		t.Errorf("Sun: got %q want 6-Sun", got)
	}
}

func TestSliceBy_GroupsAndSorts(t *testing.T) {
	// 3 trades at JST 09, 23, 09 — PF for "09" should aggregate both wins.
	trades := []Trade{
		mkTrade(utcFromJST(2026, 5, 1, 9, 0), +10),
		mkTrade(utcFromJST(2026, 5, 1, 23, 0), -5),
		mkTrade(utcFromJST(2026, 5, 2, 9, 30), +6),
	}
	got := SliceBy(trades, HourKeyJST)
	if len(got) != 2 {
		t.Fatalf("len: got %d want 2", len(got))
	}
	if got[0].Key != "09" || got[1].Key != "23" {
		t.Errorf("sort order: got %q %q", got[0].Key, got[1].Key)
	}
	if got[0].Metrics.SampleSize != 2 {
		t.Errorf("09 sample size: got %d want 2", got[0].Metrics.SampleSize)
	}
	if got[1].Metrics.SampleSize != 1 {
		t.Errorf("23 sample size: got %d want 1", got[1].Metrics.SampleSize)
	}
}

func TestSliceBy_EmptyInputReturnsNil(t *testing.T) {
	if got := SliceBy(nil, HourKeyJST); got != nil {
		t.Errorf("nil input: got %v want nil", got)
	}
}

func TestPrettySlice_FlagsSmallBuckets(t *testing.T) {
	trades := []Trade{
		mkTrade(utcFromJST(2026, 5, 1, 9, 0), +10), // single trade @ 09
	}
	out := PrettySlice("Hour-of-Day JST", SliceBy(trades, HourKeyJST))
	if !strings.Contains(out, "(n<5)") {
		t.Errorf("expected (n<5) flag in output:\n%s", out)
	}
}
