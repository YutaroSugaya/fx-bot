package command

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/risk"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
)

// EvaluateEntry is the per-tick usecase: given the latest summary + candles
// + active config, dispatch to the strategy engine and then through the risk
// gate. Returns either an actionable Signal (Decision=ENTER, Allowed=true)
// or a recorded rejection.
type EvaluateEntry struct {
	Engine        *strategy.Engine
	RejectionRepo port.SignalRejectionRepository // optional
	Logger        *slog.Logger
}

// EvaluateInput bundles the per-tick state for one Evaluate call.
type EvaluateInput struct {
	Now             time.Time
	ActiveConfig    *config.StrategyConfig
	Summary         *market.MarketSummary
	Candles1m       []market.Candle
	Candles5m       []market.Candle
	Candles1h       []market.Candle
	AccountSnapshot risk.AccountSnapshot
}

// EvaluateResult holds both the generated signal and the gate verdict so
// the caller (order manager) knows whether to act.
type EvaluateResult struct {
	Signal      strategy.Signal
	GateAllowed bool
	GateReason  string
}

// Evaluate runs the engine + gate and returns the verdict.
func (u *EvaluateEntry) Evaluate(ctx context.Context, in EvaluateInput) EvaluateResult {
	sig := u.Engine.Evaluate(strategy.EvalInput{
		Now:       in.Now,
		Summary:   in.Summary,
		Candles1m: in.Candles1m,
		Candles5m: in.Candles5m,
		Candles1h: in.Candles1h,
		Config:    in.ActiveConfig,
	})
	if !sig.IsEntry() {
		// Not an entry signal — engine declined, nothing to gate.
		return EvaluateResult{Signal: sig, GateAllowed: false, GateReason: sig.Reason}
	}

	d := risk.EvaluateSignal(sig, in.ActiveConfig, in.AccountSnapshot, in.Summary)
	if !d.Allowed {
		u.recordRejection(ctx, sig, d.Reason, in)
		if u.Logger != nil {
			u.Logger.Info("signal_rejected",
				"reason", d.Reason,
				"strategy", string(sig.StrategyName),
				"side", string(sig.Side),
				"config_id", sig.ConfigID,
			)
		}
		return EvaluateResult{Signal: sig, GateAllowed: false, GateReason: d.Reason}
	}
	return EvaluateResult{Signal: sig, GateAllowed: true}
}

func (u *EvaluateEntry) recordRejection(ctx context.Context, sig strategy.Signal, reason string, in EvaluateInput) {
	if u.RejectionRepo == nil {
		return
	}
	detail := map[string]any{
		"strategy":        string(sig.StrategyName),
		"side":            string(sig.Side),
		"strategy_reason": sig.Reason,
		"snapshot":        in.AccountSnapshot,
	}
	if in.Summary != nil {
		detail["spread_pips"] = in.Summary.CurrentRate.SpreadPips
	}
	body, _ := json.Marshal(detail)
	_ = u.RejectionRepo.Insert(ctx, port.SignalRejection{
		StrategyConfigID: sig.ConfigID,
		Reason:           reason,
		Detail:           body,
	})
}
