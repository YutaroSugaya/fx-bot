package app

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

// fakeAdvisorRunner implements just enough of AdvisorCycle.Run for testing
// RunAdvisorCyclesParallel. The real *command.AdvisorCycle has many other
// dependencies; the runner takes a callback type so tests can plug in
// behaviour without spinning up a full AdvisorCycle.
//
// RunAdvisorCyclesParallel accepts a callable per bundle (extracted in
// production wiring from bundle.AdvisorCycle.Run) so this seam stays test-
// friendly.

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRunAdvisorCyclesParallel_RunsAllSymbols(t *testing.T) {
	var calls atomic.Int32
	runners := map[string]AdvisorRunner{
		"USD_JPY": func(ctx context.Context) error { calls.Add(1); return nil },
		"EUR_JPY": func(ctx context.Context) error { calls.Add(1); return nil },
		"GBP_JPY": func(ctx context.Context) error { calls.Add(1); return nil },
	}
	RunAdvisorCyclesParallel(context.Background(), runners,
		AdvisorParallelOpts{PerSymbolTimeout: time.Second, MaxConcurrent: 2, Logger: discardLogger()},
		port.AdvisorRunSourceAuto)

	if got := calls.Load(); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

func TestRunAdvisorCyclesParallel_OneSymbolFailureDoesNotKillOthers(t *testing.T) {
	var ok atomic.Int32
	runners := map[string]AdvisorRunner{
		"USD_JPY": func(ctx context.Context) error { ok.Add(1); return nil },
		"EUR_JPY": func(ctx context.Context) error { return errors.New("claude blew up") },
		"GBP_JPY": func(ctx context.Context) error { ok.Add(1); return nil },
	}
	RunAdvisorCyclesParallel(context.Background(), runners,
		AdvisorParallelOpts{PerSymbolTimeout: time.Second, MaxConcurrent: 3, Logger: discardLogger()},
		port.AdvisorRunSourceAuto)

	if got := ok.Load(); got != 2 {
		t.Errorf("successful runs = %d, want 2 (EUR_JPY failure should not kill others)", got)
	}
}

func TestRunAdvisorCyclesParallel_PerSymbolTimeoutIsIndependent(t *testing.T) {
	// One slow symbol must not stall the others past its own timeout — and
	// each goroutine sees its own deadline (ctx.Done() fires per goroutine).
	deadlineFiredForSlow := make(chan struct{})
	fastDone := make(chan struct{})
	runners := map[string]AdvisorRunner{
		"USD_JPY": func(ctx context.Context) error {
			close(fastDone)
			return nil
		},
		"EUR_JPY": func(ctx context.Context) error {
			<-ctx.Done() // wait until per-symbol timeout fires
			close(deadlineFiredForSlow)
			return ctx.Err()
		},
	}
	start := time.Now()
	RunAdvisorCyclesParallel(context.Background(), runners,
		AdvisorParallelOpts{PerSymbolTimeout: 100 * time.Millisecond, MaxConcurrent: 2, Logger: discardLogger()},
		port.AdvisorRunSourceAuto)

	select {
	case <-fastDone:
	default:
		t.Error("fast runner did not complete")
	}
	select {
	case <-deadlineFiredForSlow:
	default:
		t.Error("slow runner did not see its deadline")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("total wait %v, want close to per-symbol timeout (100ms)", elapsed)
	}
}

func TestRunAdvisorCyclesParallel_RespectsMaxConcurrent(t *testing.T) {
	var inFlight, maxSeen atomic.Int32
	runners := map[string]AdvisorRunner{}
	for _, s := range []string{"USD_JPY", "EUR_JPY", "GBP_JPY", "AUD_JPY"} {
		runners[s] = func(ctx context.Context) error {
			cur := inFlight.Add(1)
			defer inFlight.Add(-1)
			for {
				if old := maxSeen.Load(); cur <= old || maxSeen.CompareAndSwap(old, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			return nil
		}
	}
	RunAdvisorCyclesParallel(context.Background(), runners,
		AdvisorParallelOpts{PerSymbolTimeout: time.Second, MaxConcurrent: 2, Logger: discardLogger()},
		port.AdvisorRunSourceAuto)

	if got := maxSeen.Load(); got > 2 {
		t.Errorf("observed max in-flight = %d, want <= 2", got)
	}
}
