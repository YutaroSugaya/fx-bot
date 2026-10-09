package strategy

import (
	"sort"

	"fx-bot/backend/internal/config"
)

// SignalIDGenerator is invoked by Engine.Evaluate to attach a SignalID to
// entry signals. Lives on the Engine so production code can inject a
// crypto/rand-backed generator and backtests/replays can inject a
// deterministic one. nil = leave SignalID empty
// and let the caller assign one.
type SignalIDGenerator func() string

// Engine dispatches Evaluate to the registered Strategy matching the active
// config's strategy.name. It also handles TTL-expiration and NoTrade gating
// before bothering the underlying strategy.
type Engine struct {
	registry   map[config.StrategyName]Strategy
	SignalIDFn SignalIDGenerator
}

// NewEngine returns an engine with all day-trading strategies pre-registered.
// SignalIDFn is nil by default; callers must inject one (cmd/bot uses a
// crypto/rand-backed generator, backtests use a deterministic counter).
func NewEngine() *Engine {
	e := &Engine{registry: map[config.StrategyName]Strategy{}}
	e.Register(NoTrade{})
	e.Register(MomentumPullback{})
	e.Register(BreakoutFollow{})
	e.Register(RangeBreakoutProbe{})
	e.Register(MTFPullback{})
	e.Register(MAPullback{})
	e.Register(MAPullbackV2{})
	e.Register(GotobiFix{})
	e.Register(LondonBreakout{})
	e.Register(TrendFollow{})
	e.Register(DailyTrend{})
	e.Register(ExhaustionFade{})
	return e
}

// Register adds (or replaces) a Strategy in the registry.
func (e *Engine) Register(s Strategy) {
	e.registry[s.Name()] = s
}

// RegisteredNames returns every registered strategy name in deterministic
// (sorted) order. The production strategy whitelist is DERIVED from this
// so it can never drift from what the engine can actually run
// — with a hand-kept whitelist, a strategy could be live but absent from the list
// (advisor re-enable would then reject/replace the live config); sharing this
// single source makes that impossible.
func (e *Engine) RegisteredNames() []config.StrategyName {
	names := make([]config.StrategyName, 0, len(e.registry))
	for n := range e.registry {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

// Evaluate dispatches in.Config.Strategy.Name to its handler. If the config
// is missing, expired, or marked no_trade, the engine short-circuits with a
// NoTrade signal — strategies are only invoked for active, tradeable configs.
func (e *Engine) Evaluate(in EvalInput) Signal {
	if in.Config == nil {
		return Signal{Decision: DecisionNone, Reason: "no_active_config", CreatedAt: in.Now}
	}
	// TTL & no_trade re-check at evaluation time prevents the boundary race
	// where IsActive turns false between scheduler check and engine eval.
	if !in.Config.IsActive(in.Now) {
		return Signal{
			Decision:     DecisionNoTrade,
			Reason:       "config_not_active",
			ConfigID:     in.Config.ConfigID,
			StrategyName: in.Config.Strategy.Name,
			CreatedAt:    in.Now,
		}
	}
	// Hour-of-day filter (JST). Empty list = all hours allowed (back-compat).
	// Lets a config restrict entries to specific JST hours (e.g. from a
	// per-hour backtest breakdown) without strategy code.
	if !in.Config.Entry.IsHourAllowed(in.Now) {
		return Signal{
			Decision:     DecisionNoTrade,
			Reason:       "outside_allowed_hours_jst",
			ConfigID:     in.Config.ConfigID,
			StrategyName: in.Config.Strategy.Name,
			CreatedAt:    in.Now,
		}
	}
	s, ok := e.registry[in.Config.Strategy.Name]
	if !ok {
		return Signal{
			Decision:     DecisionNone,
			Reason:       "unregistered_strategy:" + string(in.Config.Strategy.Name),
			ConfigID:     in.Config.ConfigID,
			StrategyName: in.Config.Strategy.Name,
			CreatedAt:    in.Now,
		}
	}
	sig := s.Evaluate(in)
	// SignalID assignment is delegated to the injected generator.
	// Domain code stays deterministic / replay-friendly; the non-determinism
	// lives at the wiring layer where it belongs.
	if sig.IsEntry() && sig.SignalID == "" && e.SignalIDFn != nil {
		sig.SignalID = e.SignalIDFn()
	}
	if sig.ConfigID == "" {
		sig.ConfigID = in.Config.ConfigID
	}
	return sig
}
