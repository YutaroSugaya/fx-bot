package broker

import (
	"context"
	"sync"
	"testing"
	"time"
)

func newTestBucket(capacity, refillPerSec float64, start time.Time) (*TokenBucket, *time.Time) {
	now := start
	b := NewTokenBucket(capacity, refillPerSec)
	b.lastRefill = now
	b.tokens = capacity
	b.now = func() time.Time { return now }
	return b, &now
}

func TestTokenBucket_AllowsBurstUpToCapacity(t *testing.T) {
	b, _ := newTestBucket(6, 6, time.Now())
	for i := 0; i < 6; i++ {
		if !b.TryAcquire() {
			t.Fatalf("token %d should be available", i)
		}
	}
	if b.TryAcquire() {
		t.Errorf("7th acquire should fail (bucket empty)")
	}
}

func TestTokenBucket_Refills(t *testing.T) {
	start := time.Now()
	b, now := newTestBucket(6, 6, start)
	// drain
	for i := 0; i < 6; i++ {
		b.TryAcquire()
	}
	// advance 500ms — 6/s refill * 0.5s = 3 tokens
	*now = start.Add(500 * time.Millisecond)
	for i := 0; i < 3; i++ {
		if !b.TryAcquire() {
			t.Fatalf("refill %d should yield a token", i)
		}
	}
	if b.TryAcquire() {
		t.Errorf("should be empty again")
	}
}

func TestTokenBucket_RefillCapsAtCapacity(t *testing.T) {
	start := time.Now()
	b, now := newTestBucket(6, 6, start)
	*now = start.Add(10 * time.Second) // refill way past capacity
	for i := 0; i < 6; i++ {
		if !b.TryAcquire() {
			t.Fatalf("should still have only capacity tokens, lost at %d", i)
		}
	}
	if b.TryAcquire() {
		t.Errorf("over-refilled past capacity")
	}
}

func TestTokenBucket_WaitContextCancel(t *testing.T) {
	// real-clock bucket with very slow refill (1 token/10s)
	b := NewTokenBucket(0, 0.1)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := b.Wait(ctx)
	if err == nil {
		t.Fatalf("expected context cancellation")
	}
	if !contextDeadlineOrCanceled(err) {
		t.Errorf("got %v", err)
	}
}

func TestTokenBucket_WaitAcquiresAfterRefill(t *testing.T) {
	// real-clock bucket: capacity 1, 100/s refill → after we drain it, a
	// fresh token becomes available within ~10ms.
	b := NewTokenBucket(1, 100)
	if !b.TryAcquire() {
		t.Fatalf("seed token should exist")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := b.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestTokenBucket_ConcurrentTryAcquireIsSafe(t *testing.T) {
	b := NewTokenBucket(100, 0) // no refill
	var wg sync.WaitGroup
	got := make(chan bool, 200)
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got <- b.TryAcquire()
		}()
	}
	wg.Wait()
	close(got)
	count := 0
	for ok := range got {
		if ok {
			count++
		}
	}
	if count != 100 {
		t.Errorf("expected exactly 100 acquires, got %d", count)
	}
}

func contextDeadlineOrCanceled(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return msg == "context deadline exceeded" || msg == "context canceled"
}
