package market

import (
	"testing"
	"time"
)

func TestAggregator_OnTick_OneMinuteCandle(t *testing.T) {
	a := NewAggregator("USD_JPY", map[time.Duration]int{
		time.Minute:     10,
		5 * time.Minute: 10,
	})
	t0 := time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC)
	// 5 ticks within the same minute, prices 150.10 → 150.20
	for i := 0; i < 5; i++ {
		a.OnTick(Ticker{
			Symbol:    "USD_JPY",
			Bid:       150.10 + float64(i)*0.02,
			Ask:       150.10 + float64(i)*0.02 + 0.02,
			Timestamp: t0.Add(time.Duration(i) * time.Second),
		})
	}
	// minute hasn't closed yet — buffer should be empty
	if got := len(a.Candles(time.Minute)); got != 0 {
		t.Fatalf("candles before close: %d", got)
	}

	// Cross into the next minute — previous one should close
	a.OnTick(Ticker{
		Symbol: "USD_JPY",
		Bid:    150.21, Ask: 150.23,
		Timestamp: t0.Add(time.Minute),
	})
	cs := a.Candles(time.Minute)
	if len(cs) != 1 {
		t.Fatalf("expected 1 closed candle, got %d", len(cs))
	}
	if !cs[0].OpenTime.Equal(t0) {
		t.Errorf("OpenTime: %v", cs[0].OpenTime)
	}
	// Open should be mid of first tick (150.10 + 150.12)/2 = 150.11
	if abs(cs[0].Open-150.11) > 1e-9 {
		t.Errorf("Open: %v", cs[0].Open)
	}
	// High should be at the last in-minute tick (150.18 + 150.20)/2 = 150.19
	if abs(cs[0].High-150.19) > 1e-9 {
		t.Errorf("High: %v", cs[0].High)
	}
}

func TestAggregator_FiveMinuteRollup(t *testing.T) {
	a := NewAggregator("USD_JPY", map[time.Duration]int{
		time.Minute:     10,
		5 * time.Minute: 5,
	})
	t0 := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC) // aligned on 5m
	// Feed 6 minutes of ticks — the first 5 minutes form one 5m candle
	for m := 0; m < 6; m++ {
		a.OnTick(Ticker{
			Symbol:    "USD_JPY",
			Bid:       150.10 + float64(m)*0.05,
			Ask:       150.12 + float64(m)*0.05,
			Timestamp: t0.Add(time.Duration(m) * time.Minute),
		})
	}
	// 6th tick is at minute 5 (00:05) — it closes the 5-minute group 00:00-00:04
	if got := len(a.Candles(time.Minute)); got != 5 {
		t.Fatalf("expected 5 closed 1m, got %d", got)
	}
	if got := len(a.Candles(5 * time.Minute)); got != 1 {
		t.Fatalf("expected 1 closed 5m, got %d", got)
	}
	c5 := a.Candles(5 * time.Minute)[0]
	if !c5.OpenTime.Equal(t0) {
		t.Errorf("5m OpenTime: %v", c5.OpenTime)
	}
}

func TestAggregator_FifteenMinuteRollup(t *testing.T) {
	a := NewAggregator("USD_JPY", map[time.Duration]int{
		time.Minute:      30,
		15 * time.Minute: 5,
	})
	t0 := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC) // aligned on 15m
	// 16 minutes of ticks → first 15 minutes form one 15m candle
	for m := 0; m < 16; m++ {
		a.OnTick(Ticker{
			Symbol:    "USD_JPY",
			Bid:       150.10 + float64(m)*0.05,
			Ask:       150.12 + float64(m)*0.05,
			Timestamp: t0.Add(time.Duration(m) * time.Minute),
		})
	}
	if got := len(a.Candles(time.Minute)); got != 15 {
		t.Fatalf("expected 15 closed 1m, got %d", got)
	}
	if got := len(a.Candles(15 * time.Minute)); got != 1 {
		t.Fatalf("expected 1 closed 15m, got %d", got)
	}
	c15 := a.Candles(15 * time.Minute)[0]
	if !c15.OpenTime.Equal(t0) {
		t.Errorf("15m OpenTime: %v", c15.OpenTime)
	}
}

func TestAggregator_HourlyRollup(t *testing.T) {
	a := NewAggregator("USD_JPY", map[time.Duration]int{
		time.Minute: 90,
		time.Hour:   5,
	})
	t0 := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC) // aligned on 1h
	// 61 minutes of ticks → first 60 minutes form one 1h candle
	for m := 0; m < 61; m++ {
		a.OnTick(Ticker{
			Symbol:    "USD_JPY",
			Bid:       150.10 + float64(m)*0.01,
			Ask:       150.12 + float64(m)*0.01,
			Timestamp: t0.Add(time.Duration(m) * time.Minute),
		})
	}
	if got := len(a.Candles(time.Minute)); got != 60 {
		t.Fatalf("expected 60 closed 1m, got %d", got)
	}
	if got := len(a.Candles(time.Hour)); got != 1 {
		t.Fatalf("expected 1 closed 1h, got %d", got)
	}
	cH := a.Candles(time.Hour)[0]
	if !cH.OpenTime.Equal(t0) {
		t.Errorf("1h OpenTime: %v", cH.OpenTime)
	}
}

func TestAggregator_Flush(t *testing.T) {
	a := NewAggregator("USD_JPY", map[time.Duration]int{time.Minute: 10})
	t0 := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	a.OnTick(Ticker{Symbol: "USD_JPY", Bid: 150.10, Ask: 150.12, Timestamp: t0})
	a.OnTick(Ticker{Symbol: "USD_JPY", Bid: 150.11, Ask: 150.13, Timestamp: t0.Add(20 * time.Second)})
	a.Flush()
	cs := a.Candles(time.Minute)
	if len(cs) != 1 {
		t.Errorf("expected 1 candle after flush, got %d", len(cs))
	}
}

func TestAggregator_OnKlineBypassesAggregation(t *testing.T) {
	a := NewAggregator("USD_JPY", map[time.Duration]int{time.Minute: 10})
	t0 := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	a.OnKline(Kline{
		Symbol:   "USD_JPY",
		Interval: "1min",
		OpenTime: t0,
		Open:     150.10, High: 150.20, Low: 150.05, Close: 150.15,
	})
	cs := a.Candles(time.Minute)
	if len(cs) != 1 {
		t.Errorf("expected 1, got %d", len(cs))
	}
	if cs[0].High != 150.20 {
		t.Errorf("high: %v", cs[0].High)
	}
}
