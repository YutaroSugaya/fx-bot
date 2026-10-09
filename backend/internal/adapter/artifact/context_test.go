package artifact

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// artifact adapter は cancellation を無視しない。File I/O は通常速いとはいえ、
// 呼出側が既に cancel した状態で重い op を始めるのは無駄。各メソッドは
// 入口で ctx.Err() をチェックし short-circuit する。

func TestFileMarketSummaryStore_RespectsCancelledContext(t *testing.T) {
	dir := t.TempDir()
	s := NewFileMarketSummaryStore(filepath.Join(dir, "summary.json"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 事前に cancel

	if err := s.WriteLatestSummary(ctx, []byte("{}")); !errors.Is(err, context.Canceled) {
		t.Errorf("WriteLatestSummary: got %v want context.Canceled", err)
	}
	if _, err := s.ReadLatestSummary(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ReadLatestSummary: got %v want context.Canceled", err)
	}
}

func TestFileStrategyConfigStore_RespectsCancelledContext(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStrategyConfigStore(
		filepath.Join(dir, "next.yaml"),
		filepath.Join(dir, "active.yaml"),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.PromoteActive(ctx, []byte("foo: bar")); !errors.Is(err, context.Canceled) {
		t.Errorf("PromoteActive: got %v want context.Canceled", err)
	}
	if _, err := s.ReadActive(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ReadActive: got %v want context.Canceled", err)
	}
}
