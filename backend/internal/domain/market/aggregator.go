package market

import (
	"sync"
	"time"
)

// Aggregator turns a stream of ticks (and explicit kline pushes) into
// 1m / 5m / 15m / 1h candles, kept in ring buffers.
//
//	OnTick:  call from the price loop on every ticker fetch
//	OnKline: call when bootstrapping from broker.GetKlines
//
// The aggregator is goroutine-safe.
//
// domain layer mutex exception: see
// docs/architecture/layers/domain.md "例外: domain/market の stateful buffer".
// This and RingBuffer are the only domain types allowed to hold a mutex.
type Aggregator struct {
	symbol  string
	buffers map[time.Duration]*RingBuffer

	mu sync.Mutex
	// in-flight 1m candle being assembled from ticks
	current *Candle
}

// NewAggregator initialises ring buffers per interval. The capacities map
// should typically be:
//
//	1*time.Minute   → 24*60 = 1440
//	5*time.Minute   → 24*12 = 288
//	15*time.Minute  → 24*4  = 96
//	1*time.Hour     → 24
func NewAggregator(symbol string, capacities map[time.Duration]int) *Aggregator {
	bufs := make(map[time.Duration]*RingBuffer, len(capacities))
	for d, cap := range capacities {
		bufs[d] = NewRingBuffer(cap)
	}
	return &Aggregator{symbol: symbol, buffers: bufs}
}

// Candles returns a snapshot of stored candles for the given interval.
func (a *Aggregator) Candles(d time.Duration) []Candle {
	b := a.buffers[d]
	if b == nil {
		return nil
	}
	return b.Snapshot()
}

// OnTick folds one ticker into the in-flight 1m candle, closing the previous
// minute when its boundary is crossed and rolling up the freshly-closed
// minute into the 5m / 15m / 1h series (when the boundary aligns).
//
// Mid-price = (bid + ask) / 2 is used as the tick price.
func (a *Aggregator) OnTick(t Ticker) {
	if a.buffers[time.Minute] == nil {
		return
	}
	price := t.Mid()
	bucket := t.Timestamp.Truncate(time.Minute)

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.current == nil || !a.current.OpenTime.Equal(bucket) {
		if a.current != nil {
			a.closeMinuteLocked(*a.current)
		}
		a.current = &Candle{
			Symbol:   a.symbol,
			Interval: time.Minute,
			OpenTime: bucket,
			Open:     price,
			High:     price,
			Low:      price,
			Close:    price,
		}
		return
	}

	// update in-flight candle
	if price > a.current.High {
		a.current.High = price
	}
	if price < a.current.Low {
		a.current.Low = price
	}
	a.current.Close = price
}

// Flush closes the current in-flight 1m candle (e.g. on shutdown). It also
// rolls into 5m / 15m / 1h whenever the just-closed minute lands on those
// boundaries.
func (a *Aggregator) Flush() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current != nil {
		a.closeMinuteLocked(*a.current)
		a.current = nil
	}
}

func (a *Aggregator) closeMinuteLocked(c Candle) {
	a.buffers[time.Minute].Append(c)
	end := c.OpenTime.Add(time.Minute) // close of this 1m
	a.maybeRollupLocked(end, 5*time.Minute)
	a.maybeRollupLocked(end, 15*time.Minute)
	a.maybeRollupLocked(end, time.Hour)
}

// maybeRollupLocked materialises a higher-timeframe candle from the 1m buffer
// when the just-closed minute aligns with the boundary of `interval`.
func (a *Aggregator) maybeRollupLocked(end time.Time, interval time.Duration) {
	if a.buffers[interval] == nil {
		return
	}
	bucket := end.Add(-1).Truncate(interval)
	if end.Sub(bucket) != interval {
		return
	}
	a.buildRollupLocked(bucket, interval)
}

// buildRollupLocked walks the 1m buffer for candles whose OpenTime is in
// [bucket, bucket+interval) and appends an aggregated candle to the matching buffer.
func (a *Aggregator) buildRollupLocked(bucket time.Time, interval time.Duration) {
	end := bucket.Add(interval)
	snap := a.buffers[time.Minute].Snapshot()
	var bars []Candle
	for i := len(snap) - 1; i >= 0; i-- {
		if snap[i].OpenTime.Before(bucket) {
			break
		}
		if snap[i].OpenTime.Before(end) {
			bars = append([]Candle{snap[i]}, bars...)
		}
	}
	if len(bars) == 0 {
		return
	}
	rolled := Candle{
		Symbol:   a.symbol,
		Interval: interval,
		OpenTime: bucket,
		Open:     bars[0].Open,
		High:     bars[0].High,
		Low:      bars[0].Low,
		Close:    bars[len(bars)-1].Close,
	}
	for _, b := range bars {
		if b.High > rolled.High {
			rolled.High = b.High
		}
		if b.Low < rolled.Low {
			rolled.Low = b.Low
		}
		rolled.Volume += b.Volume
	}
	a.buffers[interval].Append(rolled)
}

// OnKline pushes a finalised kline directly into the matching buffer. Useful
// for bootstrapping the buffers from broker.GetKlines at startup.
func (a *Aggregator) OnKline(k Kline) {
	c := FromKline(k)
	if c.Interval == 0 {
		return
	}
	b := a.buffers[c.Interval]
	if b == nil {
		return
	}
	b.Append(c)
}
