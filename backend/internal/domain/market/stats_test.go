package market

import (
	"testing"
	"time"
)

func mkCandles(opens []float64, t0 time.Time, interval time.Duration) []Candle {
	out := make([]Candle, len(opens))
	for i, o := range opens {
		out[i] = Candle{
			Symbol:   "USD_JPY",
			Interval: interval,
			OpenTime: t0.Add(time.Duration(i) * interval),
			Open:     o,
			Close:    o,
			High:     o,
			Low:      o,
		}
	}
	return out
}

func TestBuildWindowSummary_EmptyCandles(t *testing.T) {
	s := BuildWindowSummary(nil, 0.01)
	if s.NumCandles != 0 || s.TrendDirection != "flat" {
		t.Errorf("empty summary: %+v", s)
	}
}

func TestBuildWindowSummary_RangePips_USDJPY(t *testing.T) {
	t0 := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	cs := []Candle{
		{OpenTime: t0, Open: 150.10, High: 150.30, Low: 150.05, Close: 150.20},
		{OpenTime: t0.Add(time.Minute), Open: 150.20, High: 150.35, Low: 150.15, Close: 150.30},
	}
	s := BuildWindowSummary(cs, 0.01)
	// hi=150.35, lo=150.05 → range_pips = 30
	if abs(s.RangePips-30) > 1e-6 {
		t.Errorf("range_pips expected ~30, got %v", s.RangePips)
	}
	if s.High != 150.35 || s.Low != 150.05 {
		t.Errorf("hi/lo: %v/%v", s.High, s.Low)
	}
	if s.NumCandles != 2 {
		t.Errorf("NumCandles: %d", s.NumCandles)
	}
}

// ChangePips is the window's NET directional move (first open → last close, in pips) — the
// lane/veto yardstick. range_position tells WHERE price sits in the range but not
// HOW FAR it has already travelled; exhaustion entries (selling after a −150pip day) are
// invisible without this number.
func TestBuildWindowSummary_ChangePips(t *testing.T) {
	t0 := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	cs := []Candle{
		{OpenTime: t0, Open: 150.50, High: 150.55, Low: 150.20, Close: 150.25},
		{OpenTime: t0.Add(time.Minute), Open: 150.25, High: 150.30, Low: 150.00, Close: 150.05},
	}
	s := BuildWindowSummary(cs, 0.01)
	// first open 150.50 → last close 150.05 = −45 pips (a falling window)
	if abs(s.ChangePips-(-45)) > 1e-6 {
		t.Errorf("change_pips expected -45, got %v", s.ChangePips)
	}

	up := BuildWindowSummary([]Candle{
		{OpenTime: t0, Open: 150.00, High: 150.40, Low: 149.95, Close: 150.38},
	}, 0.01)
	if abs(up.ChangePips-38) > 1e-6 {
		t.Errorf("change_pips expected +38, got %v", up.ChangePips)
	}

	if z := BuildWindowSummary(nil, 0.01); z.ChangePips != 0 {
		t.Errorf("empty window must have change_pips 0, got %v", z.ChangePips)
	}
	if z := BuildWindowSummary(cs, 0); z.ChangePips != 0 {
		t.Errorf("non-positive pipSize must leave change_pips 0, got %v", z.ChangePips)
	}
}

func TestBuildWindowSummary_TrendUp(t *testing.T) {
	t0 := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	cs := mkCandles([]float64{150.10, 150.12, 150.14, 150.16, 150.18}, t0, time.Minute)
	s := BuildWindowSummary(cs, 0.01)
	if s.TrendDirection != "up" {
		t.Errorf("expected up, got %s", s.TrendDirection)
	}
}

func TestBuildWindowSummary_TrendDown(t *testing.T) {
	t0 := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	cs := mkCandles([]float64{150.20, 150.18, 150.16, 150.14, 150.10}, t0, time.Minute)
	s := BuildWindowSummary(cs, 0.01)
	if s.TrendDirection != "down" {
		t.Errorf("expected down, got %s", s.TrendDirection)
	}
}

func TestBuildWindowSummary_TrendFlat(t *testing.T) {
	t0 := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	cs := mkCandles([]float64{150.10, 150.10, 150.10, 150.10, 150.10}, t0, time.Minute)
	s := BuildWindowSummary(cs, 0.01)
	if s.TrendDirection != "flat" {
		t.Errorf("expected flat, got %s", s.TrendDirection)
	}
}

func TestSinceWindow(t *testing.T) {
	t0 := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	cs := mkCandles([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, t0, time.Minute)
	now := t0.Add(10 * time.Minute)
	cutoff := SinceWindow(cs, now, 5*time.Minute)
	if len(cutoff) != 5 {
		t.Errorf("expected 5 candles since cutoff, got %d", len(cutoff))
	}
}

// TestBuildWindowSummary_ATR — ATRPips = window 内の平均 True Range (pips)。
// TR_i = max(high-low, |high-prevClose|, |low-prevClose|)。先頭は prevClose 無しで high-low。
func TestBuildWindowSummary_ATR(t *testing.T) {
	t0 := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	cs := []Candle{
		{OpenTime: t0, Open: 150.10, High: 150.30, Low: 150.05, Close: 150.20},                      // TR = 0.25 (high-low)
		{OpenTime: t0.Add(time.Minute), Open: 150.20, High: 150.35, Low: 150.15, Close: 150.30},     // TR = max(.20,.15,.05)=0.20
		{OpenTime: t0.Add(2 * time.Minute), Open: 150.30, High: 150.50, Low: 150.25, Close: 150.45}, // TR = max(.25,.20,.05)=0.25
	}
	s := BuildWindowSummary(cs, 0.01)
	// mean TR = (0.25+0.20+0.25)/3 = 0.233333 → /0.01 = 23.333 pips
	if abs(s.ATRPips-23.3333) > 1e-3 {
		t.Errorf("ATRPips expected ~23.33, got %v", s.ATRPips)
	}
}

func TestBuildWindowSummary_ATR_Empty(t *testing.T) {
	if got := BuildWindowSummary(nil, 0.01).ATRPips; got != 0 {
		t.Errorf("empty ATRPips expected 0, got %v", got)
	}
}

// TestRangePosition — 0=安値圏, 1=高値圏, 0.5=レンジ無し(degenerate)。
func TestRangePosition(t *testing.T) {
	cases := []struct {
		name             string
		price, low, high float64
		want             float64
	}{
		{"mid", 150.20, 150.00, 150.40, 0.5},
		{"at_low", 150.00, 150.00, 150.40, 0.0},
		{"at_high", 150.40, 150.00, 150.40, 1.0},
		{"quarter_low", 150.10, 150.00, 150.40, 0.25},
		{"degenerate_no_range", 150.00, 150.00, 150.00, 0.5},
	}
	for _, c := range cases {
		if got := RangePosition(c.price, c.low, c.high); abs(got-c.want) > 1e-9 {
			t.Errorf("%s: RangePosition(%v,%v,%v)=%v want %v", c.name, c.price, c.low, c.high, got, c.want)
		}
	}
}

// TestATRPips_Exported — the exported ATRPips must match the value
// BuildWindowSummary computes (it is the same mean-True-Range math, surfaced as
// a standalone function so the ta package can size stops/deviation off ATR
// without importing the unexported helper). Same fixture as TestBuildWindowSummary_ATR.
func TestATRPips_Exported(t *testing.T) {
	t0 := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	cs := []Candle{
		{OpenTime: t0, Open: 150.10, High: 150.30, Low: 150.05, Close: 150.20},
		{OpenTime: t0.Add(time.Minute), Open: 150.20, High: 150.35, Low: 150.15, Close: 150.30},
		{OpenTime: t0.Add(2 * time.Minute), Open: 150.30, High: 150.50, Low: 150.25, Close: 150.45},
	}
	if got := ATRPips(cs, 0.01); abs(got-23.3333) > 1e-3 {
		t.Errorf("ATRPips expected ~23.33, got %v", got)
	}
	if got := ATRPips(cs, 0.01); abs(got-BuildWindowSummary(cs, 0.01).ATRPips) > 1e-9 {
		t.Errorf("ATRPips must match BuildWindowSummary.ATRPips, got %v vs %v", got, BuildWindowSummary(cs, 0.01).ATRPips)
	}
	if got := ATRPips(nil, 0.01); got != 0 {
		t.Errorf("empty ATRPips expected 0, got %v", got)
	}
	if got := ATRPips(cs, 0); got != 0 {
		t.Errorf("non-positive pipSize expected 0, got %v", got)
	}
}
