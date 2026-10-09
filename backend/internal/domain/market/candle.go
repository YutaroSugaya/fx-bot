package market

import (
	"sync"
	"time"
)

// Candle is one OHLCV bar in domain form (independent of the broker payload).
type Candle struct {
	Symbol   string
	Interval time.Duration
	OpenTime time.Time // bucket start (UTC)
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Volume   float64
}

// FromKline converts a broker-side Kline to a domain Candle.
// Interval comes from a string like "1min" / "5min" / "1hour".
func FromKline(k Kline) Candle {
	return Candle{
		Symbol:   k.Symbol,
		Interval: parseIntervalString(k.Interval),
		OpenTime: k.OpenTime,
		Open:     k.Open,
		High:     k.High,
		Low:      k.Low,
		Close:    k.Close,
		Volume:   k.Volume,
	}
}

func parseIntervalString(s string) time.Duration {
	switch s {
	case "1min":
		return time.Minute
	case "5min":
		return 5 * time.Minute
	case "15min":
		return 15 * time.Minute
	case "30min":
		return 30 * time.Minute
	case "1hour":
		return time.Hour
	case "4hour":
		return 4 * time.Hour
	case "8hour":
		return 8 * time.Hour
	case "12hour":
		return 12 * time.Hour
	case "1day":
		return 24 * time.Hour
	default:
		return 0
	}
}

// RingBuffer stores up to `cap` candles, dropping the oldest when full.
// It is safe for concurrent reads/writes.
//
// domain layer mutex exception: see
// docs/architecture/layers/domain.md "例外: domain/market の stateful buffer".
// This and Aggregator are the only domain types allowed to hold a mutex.
type RingBuffer struct {
	mu   sync.RWMutex
	cap  int
	data []Candle
	head int
	full bool
}

// NewRingBuffer creates a buffer with the given capacity. capacity must be > 0.
func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = 1
	}
	return &RingBuffer{cap: capacity, data: make([]Candle, capacity)}
}

// Append pushes a candle, evicting the oldest when full.
func (r *RingBuffer) Append(c Candle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data[r.head] = c
	r.head = (r.head + 1) % r.cap
	if r.head == 0 {
		r.full = true
	}
}

// Snapshot returns all stored candles in chronological order (oldest first).
func (r *RingBuffer) Snapshot() []Candle {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.full {
		out := make([]Candle, r.head)
		copy(out, r.data[:r.head])
		return out
	}
	out := make([]Candle, r.cap)
	copy(out, r.data[r.head:])
	copy(out[r.cap-r.head:], r.data[:r.head])
	return out
}

// Last returns the most recent n candles (or fewer if not enough stored).
func (r *RingBuffer) Last(n int) []Candle {
	snap := r.Snapshot()
	if n >= len(snap) {
		return snap
	}
	return snap[len(snap)-n:]
}

// Since returns candles whose OpenTime is at or after t.
func (r *RingBuffer) Since(t time.Time) []Candle {
	snap := r.Snapshot()
	for i, c := range snap {
		if !c.OpenTime.Before(t) {
			return snap[i:]
		}
	}
	return nil
}

// Resample aggregates a chronologically ordered slice of fine-grained candles
// into bars of width interval. Input candles whose OpenTime does not align to
// the interval boundary are bucketed by truncation (floor). Partial buckets at
// the end (i.e. the last bucket whose bars span less than interval) are
// included — callers that require only complete bars should drop the last
// element themselves.
//
// Designed for backtest use: the caller converts 1m history into 5m bars so
// strategy.EvalInput.Candles5m reflects the same data seen in production.
func Resample(candles []Candle, interval time.Duration) []Candle {
	if len(candles) == 0 || interval <= 0 {
		return nil
	}
	type bucket struct {
		candle Candle
		set    bool
	}
	var buckets []bucket
	var bucketTimes []time.Time
	bucketIndex := map[time.Time]int{}

	for _, c := range candles {
		t := c.OpenTime.Truncate(interval)
		idx, ok := bucketIndex[t]
		if !ok {
			idx = len(buckets)
			bucketIndex[t] = idx
			bucketTimes = append(bucketTimes, t)
			buckets = append(buckets, bucket{})
		}
		b := &buckets[idx]
		if !b.set {
			b.candle = Candle{
				Symbol:   c.Symbol,
				Interval: interval,
				OpenTime: t,
				Open:     c.Open,
				High:     c.High,
				Low:      c.Low,
				Close:    c.Close,
				Volume:   c.Volume,
			}
			b.set = true
		} else {
			if c.High > b.candle.High {
				b.candle.High = c.High
			}
			if c.Low < b.candle.Low {
				b.candle.Low = c.Low
			}
			b.candle.Close = c.Close
			b.candle.Volume += c.Volume
		}
	}

	out := make([]Candle, len(buckets))
	for i, t := range bucketTimes {
		out[i] = buckets[bucketIndex[t]].candle
	}
	return out
}
