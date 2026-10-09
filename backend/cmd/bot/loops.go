package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fx-bot/backend/internal/app"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/usecase/command"
)

// llmDecisionMaxParallel caps how many pairs invoke the claude CLI concurrently each
// decision cycle. A multi-pair fan-out otherwise starts one claude process per pair at
// once (more with the 2-agent panel) and can trip the server-side rate limiter. 2 de-bursts the load while still letting a slow pair NOT block the others
// indefinitely (it only holds one of the two slots).
const llmDecisionMaxParallel = 2

// newSignalIDGenerator returns the production SignalID generator —
// crypto/rand-backed hex. Lives at the wiring layer so domain code stays
// deterministic.
func newSignalIDGenerator() func() string {
	return func() string {
		var b [6]byte
		_, _ = rand.Read(b[:])
		return "sig-" + hex.EncodeToString(b[:])
	}
}

// LiveRuntimeReconcileInterval is the period of the Live-only runtime
// reconcile loop. Picked so a GMO-side TP/SL fill is observed within ~30s
// without burning private GET rate-limit budget.
var LiveRuntimeReconcileInterval = 30 * time.Second

// LiveRuntimeStaleGracePeriod defers a runtime stale_db_position trip until a
// position has been continuously stale & unresolvable for this long. Set to ~3
// reconcile cycles so the common case — GMO settles via OCO and its executions
// feed lags one cycle — resolves on a later pass instead of spuriously tripping
// emergency_stop. A genuinely stuck
// position still trips once the window elapses.
var LiveRuntimeStaleGracePeriod = 90 * time.Second

// LiveRuntimeExternalAdoptGrace: a Live naked broker position that cannot be adopted (no active
// config for its symbol) is DEFERRED — not tripped — for this long. Transient broker artifacts
// (e.g. a settle leg right after a close) vanish within a pass or two and never halt the bot; only
// a position that persists unadoptable past this window trips emergency_stop. 3 min ≈ 6 reconcile
// passes (interval 30s).
var LiveRuntimeExternalAdoptGrace = 3 * time.Minute

// LiveRuntimeStaleHardTripPeriod bounds how long a Live runtime
// stale_db_position keeps DEFERRING (after StaleGracePeriod) before it escalates
// to an emergency_stop trip. After grace the bot does NOT fabricate a 0-PnL
// synthetic "reconcile_cold_close" trade (that would destroy the real fill of a
// still-open position whose broker-feed listing lagged). It defers — letting the real fill resolve on a later pass — for this
// much longer window, and only trips (alert a human) for a genuine orphan,
// still WITHOUT fabricating PnL. ~20 reconcile cycles: far longer than any
// observed open-fill / executions-feed lag, yet bounded so a true orphan surely
// surfaces.
var LiveRuntimeStaleHardTripPeriod = 10 * time.Minute

// apiServerRunner abstracts *app.APIServer.Run for testability of
// runAPIServerWithShutdown without spinning up a real HTTP listener.
type apiServerRunner interface {
	Run(context.Context) error
}

// runAPIServerWithShutdown は apiSrv.Run() を呼び、終了したら parent ctx を
// cancel する。これにより API server の異常終了 (fail-closed startup
// failure や ListenAndServe failure) が他の loop の shutdown もトリガする。
//
// logger.Error するだけで cancel しないと、Dashboard が死んでも
// 価格取得 loop が走り続ける (= 可観測性ゼロで稼働継続)。
//
// 二重 cancel は context パッケージの仕様で no-op なので安全。
func runAPIServerWithShutdown(
	ctx context.Context,
	cancel context.CancelFunc,
	apiSrv apiServerRunner,
	logger *slog.Logger,
) {
	defer cancel()
	if apiSrv == nil {
		if logger != nil {
			logger.Error("api_server_nil_skipping_run")
		}
		return
	}
	if err := apiSrv.Run(ctx); err != nil {
		if logger != nil {
			logger.Error("api_server_error", "err", err)
		}
	}
}

// fireAdvisorFn fans out one scheduler tick into per-symbol advisor cycles.
// Defined here to keep loops.go self-contained; main.go closes over the
// bundle map + holder when it constructs the concrete implementation.
type fireAdvisorFn func(ctx context.Context, src port.AdvisorRunSource)

// runLoops launches the long-running goroutines — per-symbol price / minute
// loops (+ the Live-only runtime reconcile loop), the advisor scheduler, the
// API server, the advisor v2 / LLM decision / reflection schedulers, the ops
// alert loop and the optional debug-force-advisor — and blocks until ctx is
// cancelled. Each scheduler logs once and exits when its feature is disabled.
//
// Kept separate from main.go so the runtime sequence is readable
// without scrolling past 50 lines of wiring boilerplate.
//
// runLoops は内部で WithCancel で派生 ctx を作り、
// API server goroutine が終了したら必ず cancel を発火する。これで
// Dashboard が死んだ瞬間に bot 全体が graceful shutdown に入る。
func runLoops(
	ctx context.Context,
	logger *slog.Logger,
	bundles map[string]*app.SymbolBundle,
	sched *app.Scheduler,
	apiSrv *app.APIServer,
	fireAdvisors fireAdvisorFn,
	advisorEnabled bool, // ai_advisor.enabled — false なら scheduler を起動しない
	advisorV2Enabled bool, // advisor_v2.enabled — false なら v2 scheduler を起動しない (既定 false)
	advisorV2Interval time.Duration, // advisor_v2 cycle 間隔 (0 = 既定 1h)
	advisorV2StatusPath string, // v2 cycle 結果の出力先 (空なら書かない)
	llmEnabled bool, // llm_decision.enabled — false なら LLM 判断/反省 scheduler を起動しない (既定 false)
	llmReflectionEnabled bool, // llm_decision.enabled && reflection_enabled — false なら Reflexion scheduler を起動しない (= playbook 自動書換なし)
	llmInterval time.Duration, // LLM 判断 cycle 間隔 (0 = 既定 1h)
	llmReflectionInterval time.Duration, // Reflexion 反省 cycle 間隔 (0 = 既定 24h)
	llmStatusPath string, // LLM cycle 結果の出力先 (空なら書かない)
	opsAlert func(context.Context), // nil 可 (監視 alert ループ)
) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup

	// Per-symbol price + minute loops. Each bundle's worker runs in its own
	// pair of goroutines, panic-protected so one symbol crashing does not
	// take down the others (recover logs + lets the goroutine exit; the
	// scheduler / API server keep running).
	for _, b := range bundles {
		b := b
		wg.Add(2)
		go func() {
			defer wg.Done()
			defer recoverGoroutine(logger, b.Symbol, "price_loop")
			b.Worker.RunPriceLoop(ctx, b.Executor.ActiveConfig)
		}()
		go func() {
			defer wg.Done()
			defer recoverGoroutine(logger, b.Symbol, "minute_loop")
			b.Worker.RunMinuteLoop(ctx, b.Executor.ActiveConfig)
		}()
		if b.RuntimeReconcile != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer recoverGoroutine(logger, b.Symbol, "reconcile_loop")
				runReconcileLoop(ctx, logger.With("symbol", b.Symbol), b.RuntimeReconcile)
			}()
		}
	}

	wg.Add(2)
	go func() {
		defer wg.Done()
		runAdvisorScheduler(ctx, advisorEnabled, sched, fireAdvisors, logger)
	}()
	go func() {
		defer wg.Done()
		runAPIServerWithShutdown(ctx, cancel, apiSrv, logger)
	}()

	// advisor v2 (signature-breakout) scheduler — gated by advisor_v2.enabled (default off).
	// When off it logs once and exits, so the live bot is unchanged until a human enables it.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer recoverGoroutine(logger, "-", "advisor_v2_loop")
		runSignatureV2Scheduler(ctx, advisorV2Enabled, advisorV2Interval, advisorV2StatusPath, bundles, logger)
	}()

	// Autonomous LLM trade loop — gated by llm_decision.enabled (default off).
	// Two schedulers: the decision cycle (chooses trade/no-trade each interval) and the lower-cadence
	// reflection cycle (rewrites the playbook). When disabled each logs once and exits.
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer recoverGoroutine(logger, "-", "llm_decision_loop")
		runLLMDecisionScheduler(ctx, llmEnabled, llmInterval, llmStatusPath, bundles, logger)
	}()
	go func() {
		defer wg.Done()
		defer recoverGoroutine(logger, "-", "llm_reflection_loop")
		runReflectionScheduler(ctx, llmReflectionEnabled, llmReflectionInterval, bundles, logger)
	}()

	// 監視 alert ループ (日次サマリ / 当日DD超 / N時間ノートレード)。nil 可。
	if opsAlert != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer recoverGoroutine(logger, "-", "ops_alert_loop")
			opsAlert(ctx)
		}()
	}

	// Optional: trigger one advisor fire immediately when BOT_DEBUG_FORCE_ADVISOR=1.
	// MUST also respect ai_advisor.enabled — otherwise this env var could invoke the Claude (LLM)
	// advisor even when it is disabled. The LLM advisor never runs automatically unless enabled.
	if os.Getenv("BOT_DEBUG_FORCE_ADVISOR") == "1" {
		if advisorEnabled {
			go func() {
				time.Sleep(2 * time.Second)
				logger.Info("debug_force_advisor")
				fireAdvisors(ctx, port.AdvisorRunSourceManual)
			}()
		} else {
			logger.Warn("debug_force_advisor_ignored_advisor_disabled",
				"note", "ai_advisor.enabled=false; refusing to fire the Claude advisor")
		}
	}

	<-ctx.Done()
	logger.Info("shutdown_signal_received")
	wg.Wait()
	logger.Info("shutdown_complete")
}

// runSignatureV2Scheduler periodically runs each bundle's advisor v2 (signature-breakout) cycle.
// Gated by advisor_v2.enabled: when disabled it logs once and returns, so the live bot is
// unaffected until a human turns it on (at minimum size).
func runSignatureV2Scheduler(
	ctx context.Context,
	enabled bool,
	interval time.Duration,
	statusPath string,
	bundles map[string]*app.SymbolBundle,
	logger *slog.Logger,
) {
	if !enabled {
		logger.Info("advisor_v2_disabled_scheduler_not_started")
		return
	}
	if interval <= 0 {
		interval = time.Hour // default cadence (daily-breakout setups are rare; checks just catch the close sooner)
	}
	logger.Info("advisor_v2_scheduler_started", "interval", interval.String(), "symbols", len(bundles), "status_file", statusPath)

	runAll := func() {
		results := make(map[string]any, len(bundles))
		for _, b := range bundles {
			if b.SignatureCycle == nil {
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			res, err := b.SignatureCycle.Run(cctx)
			cancel()
			if err != nil {
				logger.Warn("advisor_v2_cycle_error", "symbol", b.Symbol, "err", err)
				results[b.Symbol] = map[string]any{"stage": "error", "error": err.Error()}
				continue
			}
			logger.Info("advisor_v2_cycle", "symbol", b.Symbol, "stage", res.Stage,
				"trend", res.State.Trend, "buy_trigger", res.State.BuyTrigger,
				"sell_trigger", res.State.SellTrigger, "dist_to_trigger_pips", res.State.DistToTrigPips)
			// Rich per-symbol snapshot so the UI/file shows WHEN/WHY it fires (trend, trigger, distance).
			results[b.Symbol] = map[string]any{
				"stage":                res.Stage,
				"trend":                res.State.Trend,
				"buy_trigger":          res.State.BuyTrigger,
				"sell_trigger":         res.State.SellTrigger,
				"dist_to_trigger_pips": res.State.DistToTrigPips,
				"atr_pips":             res.State.ATRPips,
				"current_price":        res.State.CurrentPrice,
				"slope_pips":           res.State.SlopePips,
				"next_step":            res.State.NextStep, // "what must happen next to enter" (UI)
				"reason":               res.State.Reason,
				// entry-condition checklist + intraday firing state (TODO-list UI / "armed, waiting today")
				"steps":          res.State.Steps,
				"armed":          res.State.Armed,
				"broke_pips":     res.State.BrokePips,
				"min_slope_pips": res.State.MinSlopePips,
				"body_need_pips": res.State.BodyNeedPips,
				"min_rr":         res.State.MinRR,
			}
		}
		writeV2Status(statusPath, interval, results, logger)
	}

	// Kickoff: one scan ~90s after boot (candle/ticker warmup) so v2 is observable immediately,
	// not only after the first full interval.
	select {
	case <-ctx.Done():
		return
	case <-time.After(90 * time.Second):
	}
	runAll()

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runAll()
		}
	}
}

// writeV2Status persists the last advisor v2 cycle outcome per symbol to a small JSON file so the
// run is observable without reading stdout. Best-effort (a write error is logged, never fatal).
func writeV2Status(path string, interval time.Duration, results map[string]any, logger *slog.Logger) {
	if path == "" {
		return
	}
	payload := map[string]any{
		"updated_at": time.Now().UTC().Format(time.RFC3339),
		"interval":   interval.String(),
		"by_symbol":  results,
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	// Atomic write (temp + rename) so a concurrent dashboard read never sees a
	// truncated/partial JSON — the reader observes either the old or new file, never a
	// half-written one. Same-dir temp keeps the rename on one filesystem (atomic).
	tmp := path + ".tmp"
	if werr := os.WriteFile(tmp, b, 0o644); werr != nil {
		if logger != nil {
			logger.Warn("status_write_failed", "path", tmp, "err", werr)
		}
		return
	}
	if rerr := os.Rename(tmp, path); rerr != nil && logger != nil {
		logger.Warn("status_rename_failed", "path", path, "err", rerr)
	}
}

// runLLMDecisionScheduler periodically runs each bundle's autonomous LLM decision cycle.
// Gated by llm_decision.enabled: disabled → logs once and returns (live bot unaffected until enabled).
// Every decision + outcome is logged and snapshotted to statusPath so the operator can watch it.
func runLLMDecisionScheduler(ctx context.Context, enabled bool, interval time.Duration, statusPath string, bundles map[string]*app.SymbolBundle, logger *slog.Logger) {
	if !enabled {
		logger.Info("llm_decision_disabled_scheduler_not_started")
		return
	}
	if interval <= 0 {
		interval = time.Hour
	}
	logger.Info("llm_decision_scheduler_started", "interval", interval.String(), "status_file", statusPath)

	runAll := func() {
		// Weekend / market-closed gate: when the spot-FX
		// market is shut there are no live prices to act on, so SKIP the LLM entirely
		// (no API burn, no rate-limit churn) and mark the panel "休場" so the operator
		// sees a benign state instead of stale errors. The next open tick resumes.
		if !market.IsForexOpen(time.Now()) {
			results := make(map[string]any, len(bundles))
			for _, b := range bundles {
				if b.LLMDecisionCycle == nil {
					continue
				}
				results[b.Symbol] = map[string]any{"stage": "market_closed"}
			}
			writeV2Status(statusPath, interval, results, logger)
			logger.Info("llm_decision_market_closed_skip")
			return
		}
		// Skip if a cycle (scheduled or a manual /api/llm-decision/trigger) is already
		// running — never run two overlapping cycles for the same pair.
		if !llmCycleRunning.CompareAndSwap(false, true) {
			logger.Info("llm_decision_cycle_skipped_already_running")
			return
		}
		defer llmCycleRunning.Store(false)
		runLLMDecisionCycleAll(ctx, statusPath, interval, bundles, logger)
	}

	select {
	case <-ctx.Done():
		return
	case <-time.After(90 * time.Second): // warmup (candles/ticker) like the v2 scheduler
	}
	runAll()

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runAll()
		}
	}
}

// llmCycleRunning serializes LLM decision cycles: the scheduled tick and a manual
// /api/llm-decision/trigger both CAS this before running, so two cycles never overlap
// for the same pair (the no-nanpin Gate would reject the 2nd anyway, but this avoids
// even attempting it and avoids doubling the claude API load).
var llmCycleRunning atomic.Bool

// runLLMDecisionCycleAll runs every bundle's LLM decision cycle CONCURRENTLY (capped by
// llmDecisionMaxParallel) and writes the per-symbol snapshot to statusPath. It is the
// shared body of both the scheduled tick and the manual trigger; the CALLER owns the
// llmCycleRunning guard and any market-closed gate (the manual trigger intentionally
// bypasses the weekend gate — it is an explicit operator action).
func runLLMDecisionCycleAll(ctx context.Context, statusPath string, interval time.Duration, bundles map[string]*app.SymbolBundle, logger *slog.Logger) {
	results := make(map[string]any, len(bundles))
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Publish "judging" for every pair up front and rewrite the snapshot as EACH pair
	// finishes, so the dashboard shows live progress (🤔 判断中… → 結果) instead of a stale
	// frame for minutes. Distinguishing "判断中 / 結果 / 時間切れ / エラー" is the whole point
	// (a timeout must not be shown as an error).
	for _, b := range bundles {
		if b.LLMDecisionCycle != nil {
			results[b.Symbol] = map[string]any{"stage": "judging"}
		}
	}
	writeV2Status(statusPath, interval, results, logger)

	// Pairs run CONCURRENTLY so one slow pair (e.g. GBP_USD) no longer blocks the others.
	// Per-pair timeout = llmPerPairTimeout (12m): the decision call (single-agent claude -p by default;
	// 2-agent panel market-regime → trade-decider only when decision_single_agent:false)
	// plus rate-limit retries can exceed 8m for slow pairs (GBP); a shorter budget kills them
	// mid-decision and shows a bogus "error". A semaphore caps concurrent claude invocations
	// (llmDecisionMaxParallel) to avoid a multi-pair burst tripping the server-side rate limiter.
	sem := make(chan struct{}, llmDecisionMaxParallel)
	for _, b := range bundles {
		if b.LLMDecisionCycle == nil {
			continue
		}
		b := b
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer recoverGoroutine(logger, b.Symbol, "llm_decision_cycle")
			// Acquire a slot BEFORE starting the per-pair timeout clock so a queued
			// pair does not burn its budget while waiting for a slot.
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, llmPerPairTimeout)
			res, err := b.LLMDecisionCycle.Run(cctx)
			cancel()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				// A TIMEOUT is not a failure — the LLM was still deciding and got cut off; it
				// retries next cycle. Surface it as a distinct "timeout" stage (the UI shows it
				// amber, not a red error). Only genuine transport failures are "error".
				if isTimeoutErr(err) {
					logger.Info("llm_decision_cycle_timeout", "symbol", b.Symbol, "err", err)
					results[b.Symbol] = map[string]any{"stage": "timeout"}
				} else {
					logger.Warn("llm_decision_cycle_error", "symbol", b.Symbol, "err", err)
					results[b.Symbol] = map[string]any{"stage": "error", "error": err.Error()}
				}
				writeV2Status(statusPath, interval, results, logger)
				return
			}
			logger.Info("llm_decision_cycle", "symbol", b.Symbol, "stage", res.Stage,
				"go", res.Decision.Go, "side", string(res.Decision.Side),
				"tp_pips", res.Decision.TPPips, "sl_pips", res.Decision.SLPips, "reason", res.Decision.Reason)
			playbook := ""
			if b.LLMDecisionCycle.Playbook != nil {
				playbook = b.LLMDecisionCycle.Playbook()
			}
			entry := map[string]any{
				"stage": res.Stage, "go": res.Decision.Go, "side": string(res.Decision.Side),
				"tp_pips": res.Decision.TPPips, "sl_pips": res.Decision.SLPips, "reason": res.Decision.Reason,
				"playbook": playbook,
			}
			if res.RejectReason != "" {
				// stage=admission_rejected: surface WHY the risk gate refused (dashboard truth).
				entry["reject_reason"] = res.RejectReason
			}
			if res.Arms != "" {
				// Show the armed conditional plans on the dashboard panel.
				entry["arms"] = res.Arms
			}
			results[b.Symbol] = entry
			writeV2Status(statusPath, interval, results, logger)
		}()
	}
	wg.Wait()
	writeV2Status(statusPath, interval, results, logger)
}

// llmPerPairTimeout bounds one pair's full decision call (single agent by default, or the
// market-regime → trade-decider panel, incl. rate-limit retries). 12m so a slow pair (GBP)
// finishes instead of being killed and mislabelled "error"; cap=2 keeps e.g. a 5-pair cycle's
// worst case (3 waves × 12m) under the hourly interval.
const llmPerPairTimeout = 12 * time.Minute

// isTimeoutErr reports whether a decision error is a deadline cut-off (the LLM was still
// working) rather than a real transport failure. The CLI surfaces both its own and the
// parent-context deadline as "...timed out", and context.DeadlineExceeded is the wrapped cause.
func isTimeoutErr(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timed out")
}

// triggerLLMDecisionCycle runs one full LLM decision cycle for ALL pairs NOW, on demand
// (the dashboard's manual "全ペア再判断" button → POST /api/llm-decision/trigger). It is the
// operator's escape hatch when the hourly loop showed errors (e.g. a transient rate-limit).
//
// It returns immediately: the cycle can take minutes (5 pairs, cap 2), so it runs detached
// in a goroutine and the dashboard's poll picks up the refreshed status file when it lands.
// Returns a non-nil error (→ HTTP 409) when the market is closed or a cycle is already running.
//
// It honours the SAME weekend gate as the scheduled tick: on a closed market there are no live
// prices, so a manual trigger must NOT run the cycle (which could otherwise reach the live order
// path on stale weekend quotes). The panel already shows 休場 from the scheduler.
func triggerLLMDecisionCycle(parent context.Context, statusPath string, interval time.Duration, bundles map[string]*app.SymbolBundle, logger *slog.Logger) error {
	if !market.IsForexOpen(time.Now()) {
		return errors.New("市場が休場中(土日)のため、再判断はスキップしました")
	}
	if !llmCycleRunning.CompareAndSwap(false, true) {
		return errors.New("判断サイクルを実行中です。完了までお待ちください")
	}
	go func() {
		defer llmCycleRunning.Store(false)
		defer recoverGoroutine(logger, "-", "llm_decision_manual_trigger")
		cctx, cancel := context.WithTimeout(parent, 15*time.Minute)
		defer cancel()
		logger.Info("llm_decision_manual_trigger_started")
		runLLMDecisionCycleAll(cctx, statusPath, interval, bundles, logger)
		logger.Info("llm_decision_manual_trigger_done")
	}()
	return nil
}

// runReflectionScheduler periodically runs each bundle's Reflexion learning cycle (rewrites the
// playbook from recent trade outcomes). Lower cadence than the decision cycle; first fire after one
// full interval so trades accumulate first. Disabled → logs once and returns.
func runReflectionScheduler(ctx context.Context, enabled bool, interval time.Duration, bundles map[string]*app.SymbolBundle, logger *slog.Logger) {
	if !enabled {
		logger.Info("llm_reflection_disabled_scheduler_not_started")
		return
	}
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	logger.Info("llm_reflection_scheduler_started", "interval", interval.String())

	runAll := func() {
		// Weekend gate: the reflection
		// panel also calls Claude, so skip it over the weekend too — NO Claude invocation at
		// all while the market is closed. Trades don't accumulate on a closed market anyway,
		// so deferring the weekly review to Monday loses nothing.
		if !market.IsForexOpen(time.Now()) {
			logger.Info("llm_reflection_market_closed_skip")
			return
		}
		for _, b := range bundles {
			if b.ReflectionCycle == nil {
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, 8*time.Minute) // 3-agent reflection panel can be slow
			res, err := b.ReflectionCycle.Run(cctx)
			cancel()
			if err != nil {
				logger.Warn("llm_reflection_cycle_error", "symbol", b.Symbol, "err", err)
				continue
			}
			logger.Info("llm_reflection_cycle", "symbol", b.Symbol, "stage", res.Stage,
				"updated", res.Updated, "n", res.TradeCount)
		}
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runAll()
		}
	}
}

// runAdvisorScheduler starts the advisor scheduler loop — but ONLY when the AI
// advisor is enabled. When ai_advisor.enabled=false the scheduler (and its
// 35-min heartbeat watchdog) is never started, so no advisor cycle fires and a
// hand-seeded fixed active config is never overwritten. Without this gate
// `enabled` would be parsed but not honoured, and the advisor would run every
// 30 min regardless of the flag.
func runAdvisorScheduler(
	ctx context.Context,
	enabled bool,
	sched *app.Scheduler,
	fireAdvisors fireAdvisorFn,
	logger *slog.Logger,
) {
	if !enabled {
		if logger != nil {
			logger.Info("ai_advisor_disabled_scheduler_not_started",
				"hint", "ai_advisor.enabled=false — 固定 active config を使い続ける (churn 停止)")
		}
		return
	}
	sched.Run(ctx, func(c context.Context, wasDefault bool) {
		src := port.AdvisorRunSourceAuto
		if !wasDefault {
			src = port.AdvisorRunSourceEvent
		}
		fireAdvisors(c, src)
	})
}

// recoverGoroutine logs a panic in a per-symbol goroutine and lets the
// goroutine exit cleanly. One symbol's crash should not bring down siblings.
func recoverGoroutine(logger *slog.Logger, symbol, loop string) {
	if r := recover(); r != nil {
		logger.Error("symbol_goroutine_panic",
			"symbol", symbol, "loop", loop, "panic", r)
	}
}

// runReconcileLoop ticks LiveRuntimeReconcileInterval and runs the
// reconciler. A reconcile failure is logged but does not exit the loop —
// the next pass will retry. Any discrepancy detected inside Reconcile.Run
// trips emergency_stop on its own.
func runReconcileLoop(ctx context.Context, logger *slog.Logger, r *command.Reconcile) {
	t := time.NewTicker(LiveRuntimeReconcileInterval)
	defer t.Stop()
	logger.Info("live_runtime_reconcile_loop_started",
		"interval", LiveRuntimeReconcileInterval.String())
	defer logger.Info("live_runtime_reconcile_loop_stopped")
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		sum, err := r.Run(ctx)
		if err != nil {
			logger.Warn("runtime_reconcile_failed", "err", err)
			continue
		}
		if sum.Resolved+sum.Tripped+sum.Deferred > 0 || sum.Adopted+sum.MarkedDone > 0 {
			logger.Info("runtime_reconcile_pass",
				"broker_open", sum.BrokerCount, "db_open", sum.DBCount,
				"resolved", sum.Resolved, "deferred", sum.Deferred, "tripped", sum.Tripped,
				"adopted", sum.Adopted, "marked_done", sum.MarkedDone)
		}
	}
}

// configureScheduler builds the scheduler with NextIntervalFn wired to the
// shortest positive next_advisor_run_in_minutes across every symbol's
// active config. Bot-wide scheduler fans out to all bundles on each tick,
// so honouring the minimum keeps a fast-evaluating symbol from being
// starved by a slow-evaluating sibling.
//
// monitor は nil 許容。non-nil + any symbol が parse_error
// 状態のときは config の値より優先して parseErrorRetryInterval を返す。
//
// HeartbeatTimeout を 35 分に設定。30 分間隔の 1 サイクル
// 超過で watchdog goroutine が自動 fire し、サイレント期間を潰す。
func configureScheduler(botCfg *config.BotConfig, logger *slog.Logger, holder *app.ActiveConfigHolder, monitor *app.ParseErrorMonitor) *app.Scheduler {
	sched := app.NewSchedulerFromConfig(botCfg.Scheduler, botCfg.AIAdvisor.IntervalMinutes, logger, botCfg.Bot.Timezone)
	sched.NextIntervalFn = func() time.Duration { return nextScheduleInterval(holder, monitor) }
	sched.HeartbeatTimeout = 35 * time.Minute
	return sched
}

// parseErrorRetryInterval は直近 advisor 失敗 (parse_error / timeout /
// cli_error) の間、config の next_advisor_run_in_minutes より優先して使う短い
// cadence。10 分 = 素早く再走しつつ CLI 連打にもならない妥協値。
// 失敗後に次サイクルまで長い空白ができるのを防ぐ (parse_error 限定ではなく
// 全失敗が対象)。
const parseErrorRetryInterval = 10 * time.Minute

// nextScheduleInterval returns the next-fire wait time.
//
//   - monitor が non-nil で AnyRecentFailure() なら parseErrorRetryInterval。
//     (= config 由来の cadence を override する。stale config の再発防止)
//   - それ以外は config 由来の最短 NextAdvisorRunInMinutes。
//   - 該当なし → 0 を返し、scheduler は default IntervalMinutes へ fallback。
func nextScheduleInterval(holder *app.ActiveConfigHolder, monitor *app.ParseErrorMonitor) time.Duration {
	if monitor != nil && monitor.AnyRecentFailure() {
		return parseErrorRetryInterval
	}
	var min time.Duration
	for _, cfg := range holder.All() {
		if cfg == nil || cfg.NextAdvisorRunInMinutes <= 0 {
			continue
		}
		d := time.Duration(cfg.NextAdvisorRunInMinutes) * time.Minute
		if min == 0 || d < min {
			min = d
		}
	}
	return min
}
