package command

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"fx-bot/backend/internal/domain/market"
)

// moveCheckInterval throttles the per-symbol big-move evaluation: the 1s price
// loop calls OnPriceTick every tick, but scanning the 1m candle window that
// often is pointless — a 30m move does not change meaningfully within 10s.
const moveCheckInterval = 10 * time.Second

// LLMEventRetrigger fires an extra all-pairs LLM decision cycle on specific events,
// on top of the hourly scheduler (the hourly judgment
// goes stale; a freed slot or a hard move deserves an immediate re-judgment).
//
// It NEVER runs a cycle itself: Trigger is the same path as the dashboard's manual
// 全ペア再判断 button (weekend gate + llmCycleRunning run-guard), so a cycle already
// in flight simply refuses the trigger — the scheduled hourly run always wins and two
// cycles never overlap. The strategy (playbook) is untouched; only WHEN the
// unchanged checklist is re-judged changes.
//
// Cooldowns are global across pairs (a fired cycle re-judges every pair, so a second
// event moments later has nothing new to ask) and are armed on every fire ATTEMPT,
// including refused ones, so a persistent condition cannot hammer the trigger.
type LLMEventRetrigger struct {
	OnPositionClose bool          // fire when any position finishes closing
	MovePips        float64       // fire when |mid − mid MoveWindow ago| ≥ this (0 = off)
	MoveWindow      time.Duration // lookback for the move (config default 30m)
	MoveCooldown    time.Duration // min spacing between move fire attempts (config default 20m)
	CloseCooldown   time.Duration // min spacing between close fire attempts (config default 3m)

	// Trigger runs one all-pairs decision cycle NOW (wired to the manual-button path).
	// A non-nil error means "refused" (already running / market closed) — logged, not retried;
	// the cooldown is armed regardless. nil-tolerant (startup wiring order).
	Trigger func(reason string) error
	Now     func() time.Time // nil → time.Now
	Logger  *slog.Logger

	mu            sync.Mutex
	nextMoveCheck map[string]time.Time // per-symbol evaluation throttle
	moveReadyAt   time.Time            // global move-trigger cooldown gate
	closeReadyAt  time.Time            // global close-trigger cooldown gate
}

func (r *LLMEventRetrigger) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// OnPriceTick evaluates the big-move trigger for one symbol. candles1m is a getter
// (not a slice) so the throttled fast path never pays the aggregator snapshot copy.
// Called from each symbol's 1s price loop goroutine; must stay cheap and non-blocking.
func (r *LLMEventRetrigger) OnPriceTick(symbol string, mid float64, candles1m func() []market.Candle) {
	if r == nil || r.MovePips <= 0 {
		return
	}
	pip := market.PipSize(symbol)
	if pip <= 0 || mid <= 0 {
		return
	}
	now := r.now()

	r.mu.Lock()
	if now.Before(r.nextMoveCheck[symbol]) {
		r.mu.Unlock()
		return
	}
	if r.nextMoveCheck == nil {
		r.nextMoveCheck = make(map[string]time.Time)
	}
	r.nextMoveCheck[symbol] = now.Add(moveCheckInterval)
	if now.Before(r.moveReadyAt) {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	ref, ok := closeAtOrBefore(candles1m(), now.Add(-r.MoveWindow))
	if !ok {
		return // 起動直後など window 分の履歴が無い — 誤発火しない (fail closed)
	}
	movePips := (mid - ref) / pip
	if movePips < r.MovePips && movePips > -r.MovePips {
		return
	}

	// Re-check + arm the cooldown under the lock (two symbols' loops can pass the
	// threshold in the same second; only one may fire).
	r.mu.Lock()
	if now.Before(r.moveReadyAt) {
		r.mu.Unlock()
		return
	}
	r.moveReadyAt = now.Add(r.MoveCooldown)
	r.mu.Unlock()

	reason := fmt.Sprintf("big_move:%s %+.1fpips/%.0fm", symbol, movePips, r.MoveWindow.Minutes())
	r.fire(reason)
}

// OnPositionClosed fires a re-judgment after any position finished closing (broker
// OCO fill via reconcile, or a bot-side MaxHold/ratchet/manual close). Called from
// the reconcile / close-saga goroutines.
func (r *LLMEventRetrigger) OnPositionClosed(symbol, closeReason string) {
	if r == nil || !r.OnPositionClose {
		return
	}
	now := r.now()
	r.mu.Lock()
	if now.Before(r.closeReadyAt) {
		r.mu.Unlock()
		return
	}
	r.closeReadyAt = now.Add(r.CloseCooldown)
	r.mu.Unlock()

	r.fire(fmt.Sprintf("position_closed:%s %s", symbol, closeReason))
}

func (r *LLMEventRetrigger) fire(reason string) {
	if r.Trigger == nil {
		return
	}
	// Both outcomes log at WARN, not INFO: under LOG_LEVEL=warn these two lines
	// are the only proof of whether an event fired or was refused — at INFO they
	// vanish from the log entirely.
	if err := r.Trigger(reason); err != nil {
		// Refused (cycle already running / market closed) — benign; the running or
		// next scheduled cycle covers it. Cooldown stays armed so we don't hammer.
		if r.Logger != nil {
			r.Logger.Warn("llm_event_retrigger_refused", "reason", reason, "cause", err.Error())
		}
		return
	}
	if r.Logger != nil {
		r.Logger.Warn("llm_event_retrigger_fired", "reason", reason)
	}
}

// closeAtOrBefore returns the Close of the NEWEST candle whose OpenTime ≤ target.
// ok=false when the history does not reach back to target (candles are ascending).
func closeAtOrBefore(candles []market.Candle, target time.Time) (float64, bool) {
	for i := len(candles) - 1; i >= 0; i-- {
		if !candles[i].OpenTime.After(target) {
			return candles[i].Close, true
		}
	}
	return 0, false
}
