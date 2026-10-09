package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"fx-bot/backend/internal/port"
)

// Regression: if ai_advisor.enabled is not wired into the scheduler start, the
// advisor fires every 30 min (plus a 35-min heartbeat watchdog) regardless of
// the flag and overwrites a hand-seeded fixed active config. runAdvisorScheduler must NOT start the scheduler — and must
// therefore never fire an advisor cycle — when enabled=false.
func TestRunAdvisorScheduler_DisabledNeverFires(t *testing.T) {
	var fired atomic.Int32
	fire := func(_ context.Context, _ port.AdvisorRunSource) { fired.Add(1) }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	done := make(chan struct{})
	go func() {
		// sched is nil on purpose: when disabled it must never be dereferenced.
		runAdvisorScheduler(context.Background(), false, nil, fire, logger)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runAdvisorScheduler(enabled=false) did not return promptly")
	}
	if got := fired.Load(); got != 0 {
		t.Fatalf("advisor fired %d times while disabled, want 0", got)
	}
}

// runLoops が apiSrv.Run() の error を logger.Error するだけで parent ctx を
// cancel しないと、Dashboard が死んでも価格取得 loop だけ走り続けて可観測性ゼロ
// になる。runAPIServerWithShutdown が API server 終了で必ず
// cancel を発火することを検証する。

type fakeAPIServer struct {
	runCalled atomic.Int32
	returnErr error
	// blockUntilCtxDone=true なら Run() は ctx.Done() まで待つ。
	// false なら returnErr を即返す。
	blockUntilCtxDone bool
}

func (f *fakeAPIServer) Run(ctx context.Context) error {
	f.runCalled.Add(1)
	if f.blockUntilCtxDone {
		<-ctx.Done()
		return nil
	}
	return f.returnErr
}

func TestRunAPIServerWithShutdown_ImmediateErrorCancelsParent(t *testing.T) {
	apiErr := errors.New("listen: address in use")
	srv := &fakeAPIServer{returnErr: apiErr}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	done := make(chan struct{})
	go func() {
		runAPIServerWithShutdown(ctx, cancel, srv, logger)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runAPIServerWithShutdown did not return")
	}

	// helper 終了後、ctx は cancel されているはず (= parent loop も止まる)。
	select {
	case <-ctx.Done():
	default:
		t.Fatal("ctx was NOT cancelled after API server returned error")
	}
}

func TestRunAPIServerWithShutdown_NormalShutdownAlsoCancels(t *testing.T) {
	// Run() が nil を返した (= ctx cancel された) ケースでも cancel が呼ばれる。
	// idempotent なので二重 cancel は問題ない。
	srv := &fakeAPIServer{blockUntilCtxDone: true}

	parent, parentCancel := context.WithCancel(context.Background())
	ctx, cancel := context.WithCancel(parent)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	done := make(chan struct{})
	go func() {
		runAPIServerWithShutdown(ctx, cancel, srv, logger)
		close(done)
	}()

	// 外側からシャットダウンを発火
	time.Sleep(20 * time.Millisecond)
	parentCancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("did not return on parent cancel")
	}
	if srv.runCalled.Load() != 1 {
		t.Errorf("Run called %d times, want 1", srv.runCalled.Load())
	}
}

func TestRunAPIServerWithShutdown_NilApiServer_DoesNotPanic(t *testing.T) {
	// nil 渡されても panic ではなく早期 return + cancel する (defensive)。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked: %v", r)
		}
	}()
	runAPIServerWithShutdown(ctx, cancel, nil, logger)
	select {
	case <-ctx.Done():
	default:
		t.Error("expected ctx to be cancelled even on nil apiSrv")
	}
}
