package market

import (
	"testing"
	"time"
)

func TestRingBuffer_AppendAndSnapshotChronological(t *testing.T) {
	b := NewRingBuffer(3)
	t0 := time.Now().UTC()
	for i := 0; i < 3; i++ {
		b.Append(Candle{OpenTime: t0.Add(time.Duration(i) * time.Minute)})
	}
	snap := b.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("len=%d", len(snap))
	}
	for i := 0; i < 3; i++ {
		want := t0.Add(time.Duration(i) * time.Minute)
		if !snap[i].OpenTime.Equal(want) {
			t.Errorf("snap[%d]: got %v want %v", i, snap[i].OpenTime, want)
		}
	}
}

func TestRingBuffer_OverflowEvictsOldest(t *testing.T) {
	b := NewRingBuffer(3)
	t0 := time.Now().UTC()
	for i := 0; i < 5; i++ {
		b.Append(Candle{OpenTime: t0.Add(time.Duration(i) * time.Minute)})
	}
	snap := b.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("len=%d", len(snap))
	}
	// expect minutes 2, 3, 4
	for i, m := range []int{2, 3, 4} {
		want := t0.Add(time.Duration(m) * time.Minute)
		if !snap[i].OpenTime.Equal(want) {
			t.Errorf("snap[%d]: got %v want %v", i, snap[i].OpenTime, want)
		}
	}
}

func TestRingBuffer_LastReturnsTailSlice(t *testing.T) {
	b := NewRingBuffer(5)
	t0 := time.Now().UTC()
	for i := 0; i < 5; i++ {
		b.Append(Candle{OpenTime: t0.Add(time.Duration(i) * time.Minute)})
	}
	last2 := b.Last(2)
	if len(last2) != 2 {
		t.Fatalf("last2 len=%d", len(last2))
	}
	if !last2[0].OpenTime.Equal(t0.Add(3 * time.Minute)) {
		t.Errorf("last2[0]: %v", last2[0].OpenTime)
	}
	if !last2[1].OpenTime.Equal(t0.Add(4 * time.Minute)) {
		t.Errorf("last2[1]: %v", last2[1].OpenTime)
	}
}

func TestRingBuffer_LastOversize(t *testing.T) {
	b := NewRingBuffer(5)
	t0 := time.Now().UTC()
	for i := 0; i < 3; i++ {
		b.Append(Candle{OpenTime: t0.Add(time.Duration(i) * time.Minute)})
	}
	last := b.Last(10)
	if len(last) != 3 {
		t.Errorf("len=%d", len(last))
	}
}

func TestRingBuffer_Since(t *testing.T) {
	b := NewRingBuffer(5)
	t0 := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		b.Append(Candle{OpenTime: t0.Add(time.Duration(i) * time.Minute)})
	}
	since := b.Since(t0.Add(2 * time.Minute))
	if len(since) != 3 {
		t.Errorf("len=%d", len(since))
	}
}

func TestPipSize(t *testing.T) {
	cases := map[string]float64{
		"USD_JPY": 0.01,
		"EUR_JPY": 0.01,
		"GBP_JPY": 0.01,
		"AUD_JPY": 0.01,
		"EUR_USD": 0.0001,
		"GBP_USD": 0.0001,
	}
	for sym, want := range cases {
		if got := PipSize(sym); got != want {
			t.Errorf("PipSize(%q) = %v, want %v", sym, got, want)
		}
	}
}

func TestTicker_SpreadPips(t *testing.T) {
	tk := Ticker{Bid: 150.121, Ask: 150.124}
	s := tk.SpreadPips(0.01)
	if abs(s-0.3) > 1e-9 {
		t.Errorf("spread expected 0.3 pips, got %v", s)
	}
	// zero pip size guard
	if tk.SpreadPips(0) != 0 {
		t.Errorf("zero pipSize should yield 0 spread")
	}
}

func TestTicker_Mid(t *testing.T) {
	tk := Ticker{Bid: 150.10, Ask: 150.20}
	if abs(tk.Mid()-150.15) > 1e-9 {
		t.Errorf("Mid: %v", tk.Mid())
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
