package strategy

import "fx-bot/backend/internal/config"

// NoTrade is the explicit "do nothing" strategy. The engine dispatches here
// when the active config picks strategy.name=no_trade or NoTrade.Enabled=true.
type NoTrade struct{}

func (NoTrade) Name() config.StrategyName { return config.StrategyNoTrade }

func (NoTrade) Evaluate(in EvalInput) Signal {
	reason := ""
	if in.Config != nil {
		reason = in.Config.NoTrade.Reason
	}
	return Signal{
		Decision:     DecisionNoTrade,
		StrategyName: config.StrategyNoTrade,
		Reason:       reason,
		CreatedAt:    in.Now,
	}
}
