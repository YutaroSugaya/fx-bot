package broker

import (
	"context"
	"sync"
	"time"
)

// TokenBucket is a simple token-bucket rate limiter. Tokens accrue at
// `refillPerSec` until `capacity` is reached. `Wait` blocks (respecting ctx)
// until at least one token is available, then consumes it.
//
// The intended use is to wrap GMO API calls — separate buckets for Public/
// Private GET (6/s) and Private POST (0.5/s by default).
type TokenBucket struct {
	capacity     float64
	refillPerSec float64

	mu         sync.Mutex
	tokens     float64
	lastRefill time.Time
	now        func() time.Time
}

// NewTokenBucket creates a bucket with `capacity` tokens, starting full.
// refillPerSec is the token regeneration rate.
func NewTokenBucket(capacity, refillPerSec float64) *TokenBucket {
	return &TokenBucket{
		capacity:     capacity,
		refillPerSec: refillPerSec,
		tokens:       capacity,
		lastRefill:   time.Now(),
		now:          time.Now,
	}
}

// refillLocked must be called with mu held. It tops up tokens based on
// elapsed time since lastRefill.
func (b *TokenBucket) refillLocked(t time.Time) {
	if !t.After(b.lastRefill) {
		return
	}
	elapsed := t.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * b.refillPerSec
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.lastRefill = t
}

// TryAcquire returns true and consumes one token if one is available.
func (b *TokenBucket) TryAcquire() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(b.now())
	if b.tokens >= 1 {
		b.tokens -= 1
		return true
	}
	return false
}

// Wait blocks until a token can be consumed, or ctx is cancelled.
func (b *TokenBucket) Wait(ctx context.Context) error {
	for {
		b.mu.Lock()
		b.refillLocked(b.now())
		if b.tokens >= 1 {
			b.tokens -= 1
			b.mu.Unlock()
			return nil
		}
		// Compute how long until we have 1 token.
		needed := 1 - b.tokens
		var sleep time.Duration
		if b.refillPerSec > 0 {
			sleep = time.Duration(needed / b.refillPerSec * float64(time.Second))
		} else {
			sleep = time.Second
		}
		b.mu.Unlock()

		// Don't oversleep — at minimum 1ms, cap at 1s for responsiveness.
		if sleep < time.Millisecond {
			sleep = time.Millisecond
		}
		if sleep > time.Second {
			sleep = time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleep):
		}
	}
}
