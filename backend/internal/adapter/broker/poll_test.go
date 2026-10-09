package broker

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ResolveExecution / ResolveSettleLegs の
// 「ctx 期限まで一定間隔で polling、条件成立で return」パターンは
// 共通 helper (pollUntilOrCtxDone) に集約している。

func TestPollUntil_ReturnsFirstSuccess(t *testing.T) {
	calls := 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := pollUntilOrCtxDone(ctx, 10*time.Millisecond, func(_ context.Context) (int, bool, error) {
		calls++
		if calls >= 3 {
			return 42, true, nil
		}
		return 0, false, nil
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 42 {
		t.Errorf("got %d want 42", got)
	}
	if calls < 3 {
		t.Errorf("expected ≥3 calls; got %d", calls)
	}
}

func TestPollUntil_ReturnsErrorImmediately(t *testing.T) {
	wantErr := errors.New("boom")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := 0
	_, err := pollUntilOrCtxDone(ctx, 10*time.Millisecond, func(_ context.Context) (int, bool, error) {
		calls++
		return 0, false, wantErr
	})
	if err != wantErr {
		t.Errorf("got %v want %v", err, wantErr)
	}
	if calls != 1 {
		t.Errorf("err must short-circuit; calls=%d", calls)
	}
}

func TestPollUntil_RespectsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := pollUntilOrCtxDone(ctx, 10*time.Millisecond, func(_ context.Context) (int, bool, error) {
		return 0, false, nil // never done
	})
	if err == nil {
		t.Fatal("expected ctx err")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v want DeadlineExceeded", err)
	}
}
