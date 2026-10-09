package command

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/risk"
	"fx-bot/backend/internal/domain/strategy"
)

// TradingCycle is one decision cycle: strategy evaluation → risk gate →
// optional order execution. Kept separate from worker.priceTick so any signal source — REST tick, WebSocket push, manual /
// scheduled trigger — can drive a trade through the same code path. Worker
// keeps only timer / I/O orchestration concerns.
//
// Stateless and side-effect free relative to its inputs (the underlying
// Evaluator records rejections; the Executor places orders). Callers
// assemble inputs once per tick and invoke Execute.
type TradingCycle struct {
	Evaluator *EvaluateEntry
	Executor  *ExecuteOrder
	Logger    *slog.Logger
	// EntriesDisabled, when true, skips the engine-tick ENTRY evaluation entirely (no new orders
	// from the fixed active config). Exit management runs on a separate path and is unaffected.
	// Set by advisor_v2.exclusive OR llm_decision.enabled so the LLM loop / advisor v2 becomes the
	// only entry source. Default false.
	EntriesDisabled bool
	// EngineOwnedHoursJST overrides EntriesDisabled during specific JST hours-of-day: in these hours
	// the per-tick engine IS allowed to enter even though the LLM loop owns the symbol the rest of
	// the day. This is the deterministic side of the hour partition (e.g. exhaustion_fade on USD_JPY in
	// JST {4,10,11}); it must equal the symbol's bot_config.llm_decision.exclude_hours_jst so the two
	// paths never overlap. Empty = EntriesDisabled applies at all hours (back-compat).
	EngineOwnedHoursJST []int
}

// TradingCycleInput bundles the per-tick state. Same shape as the inline
// inputs worker.priceTick used to assemble, but explicit so future signal
// sources can construct it independently.
type TradingCycleInput struct {
	Now             time.Time
	Ticker          *market.Ticker
	ActiveConfig    *config.StrategyConfig
	Summary         *market.MarketSummary
	Candles1m       []market.Candle
	Candles5m       []market.Candle
	Candles1h       []market.Candle
	AccountSnapshot risk.AccountSnapshot
}

// TradingCycleResult lets callers log a one-liner without re-implementing
// the branching. ExecuteErr is the order-placement error (rare — most
// failures already trip emergency_stop inside Executor). If Decision is
// not DecisionEnter, GateReason explains why no order was attempted.
type TradingCycleResult struct {
	Signal     strategy.Signal
	Decision   strategy.Decision
	GateReason string
	Executed   bool
	ExecuteErr error
}

// Execute runs the cycle. Returns nil iff nothing crashed; non-actionable
// outcomes (gate reject, NoTrade, etc.) are reported through the result.
func (t *TradingCycle) Execute(ctx context.Context, in TradingCycleInput) (TradingCycleResult, error) {
	if t.Evaluator == nil {
		return TradingCycleResult{}, nil
	}
	if t.EntriesDisabled && !jstHourIn(in.Now, t.EngineOwnedHoursJST) {
		// LLM loop / advisor_v2.exclusive owns this symbol now: engine-tick entries are off.
		// Exception: during EngineOwnedHoursJST the LLM cedes the symbol to the deterministic engine
		// strategy (hour partition), so entries are allowed. Exits (ManageOpenPositions) run on a
		// separate path, so open positions keep being managed regardless.
		return TradingCycleResult{Decision: strategy.DecisionNoTrade, GateReason: "engine_entries_disabled_v2_exclusive"}, nil
	}
	res := t.Evaluator.Evaluate(ctx, EvaluateInput{
		Now:             in.Now,
		ActiveConfig:    in.ActiveConfig,
		Summary:         in.Summary,
		Candles1m:       in.Candles1m,
		Candles5m:       in.Candles5m,
		Candles1h:       in.Candles1h,
		AccountSnapshot: in.AccountSnapshot,
	})
	out := TradingCycleResult{
		Signal:     res.Signal,
		Decision:   res.Signal.Decision,
		GateReason: res.GateReason,
	}
	if !res.GateAllowed || t.Executor == nil {
		return out, nil
	}
	if err := t.Executor.OnSignal(ctx, res.Signal, in.Ticker); err != nil {
		var rej *AdmissionRejectedError
		if errors.As(err, &rej) {
			// Risk-gate refusal: a normal "no entry" outcome, not an execution failure.
			// It must not be reported as Executed=true for an order that never existed.
			out.GateReason = rej.Reason
			if t.Logger != nil {
				t.Logger.Warn("trading_cycle_entry_admission_rejected", "reason", rej.Reason)
			}
			return out, nil
		}
		out.ExecuteErr = err
		if t.Logger != nil {
			t.Logger.Error("trading_cycle_execute_failed", "err", err)
		}
		return out, nil // caller decides whether to keep going
	}
	out.Executed = true
	return out, nil
}

// jstHourIn reports whether now's hour-of-day in JST (UTC+9, no DST) is in hours.
// Empty/nil hours → false. Shared by the hour-partition gates.
func jstHourIn(now time.Time, hours []int) bool {
	if len(hours) == 0 {
		return false
	}
	h := now.UTC().Add(9 * time.Hour).Hour()
	for _, x := range hours {
		if x == h {
			return true
		}
	}
	return false
}
