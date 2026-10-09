package app

import (
	"context"
	"log/slog"
	"time"

	"fx-bot/backend/internal/port"

	"golang.org/x/sync/errgroup"
)

// AdvisorRunner is the per-symbol callable that RunAdvisorCyclesParallel
// dispatches. Production wiring binds this to bundle.AdvisorCycle.Run with
// the source pre-applied; tests can plug in fakes without spinning up a
// full AdvisorCycle.
type AdvisorRunner func(ctx context.Context) error

// AdvisorParallelOpts tunes the parallel runner.
//
//	PerSymbolTimeout — each goroutine derives a context.WithTimeout of this
//	                   duration; if Claude hangs for one symbol the others
//	                   are unaffected.
//	MaxConcurrent    — errgroup.SetLimit cap. Throttles concurrent Claude
//	                   CLI invocations to keep credit spikes and OS process
//	                   count predictable. 0 = unlimited.
//	Logger           — used for the per-symbol error log. nil = silent.
type AdvisorParallelOpts struct {
	PerSymbolTimeout time.Duration
	MaxConcurrent    int
	Logger           *slog.Logger
}

// RunAdvisorCyclesParallel invokes every runner concurrently with the
// configured limits. Per-symbol failures are logged but never propagated —
// returning nil from each goroutine keeps errgroup from cancelling siblings,
// so one symbol's Claude failure cannot starve other symbols from getting
// their cycle.
//
// Blocks until every runner has returned (success, error, or per-symbol
// timeout).
func RunAdvisorCyclesParallel(
	ctx context.Context,
	runners map[string]AdvisorRunner,
	opts AdvisorParallelOpts,
	source port.AdvisorRunSource,
) {
	if len(runners) == 0 {
		return
	}
	g, gctx := errgroup.WithContext(ctx)
	if opts.MaxConcurrent > 0 {
		g.SetLimit(opts.MaxConcurrent)
	}
	for sym, run := range runners {
		sym, run := sym, run
		g.Go(func() error {
			cctx := gctx
			var cancel context.CancelFunc
			if opts.PerSymbolTimeout > 0 {
				cctx, cancel = context.WithTimeout(gctx, opts.PerSymbolTimeout)
				defer cancel()
			}
			if err := run(cctx); err != nil && opts.Logger != nil {
				opts.Logger.Warn("advisor_cycle_failed",
					"symbol", sym, "source", string(source), "err", err)
			}
			return nil
		})
	}
	_ = g.Wait()
}
