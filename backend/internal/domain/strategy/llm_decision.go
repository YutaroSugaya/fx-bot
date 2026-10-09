package strategy

import (
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
)

// LLMTradeDecision is the DOMAIN result of the autonomous trade-decider LLM. Unlike AdvisorVerdict — which only gates
// deterministic geometry — here the LLM CHOOSES side + TP/SL. The decision is still downstream of the
// risk Gate + broker OCO and is forced to the config quantity; this type carries no authority of its
// own. The adapter parser (advisor.LLMDecision) maps into this; the usecase consumes only this.
type LLMTradeDecision struct {
	Go     bool
	Side   order.Side // BUY | SELL ("none" when !Go)
	Entry  float64    // advisory reference; live entry is the market price at submit
	TPPips float64
	SLPips float64
	Reason string
	// Arms are conditional entry scenarios the LLM pre-places
	// for the coming cycle window: "if price breaks TriggerPrice, enter Side with TP/SL".
	// Deterministic code watches ticks, RE-VALIDATES every veto at fire time, and executes;
	// the plans are replaced wholesale by the next cycle's decision. Empty when Go=true (an
	// immediate entry occupies the single position slot) — the parser enforces that.
	Arms []ArmedPlan
}

// ArmedPlan is one pre-placed conditional entry (see LLMTradeDecision.Arms). Pure data; every
// safety decision happens at fire time in the usecase (vetoes re-checked on fresh market state).
type ArmedPlan struct {
	Side         order.Side
	TriggerPrice float64
	BreakAbove   bool // true: fire when price rises through TriggerPrice; false: falls through
	TPPips       float64
	SLPips       float64
	Reason       string // carries the lane tag ([L1]/[L4]) — the fire-time lane re-check keys off it
}

// BuildLLMSignal converts a go LLM decision into an ENTER Signal. `entry` is the LIVE fill price
// (market, like the advisor_v2 signature cycle), TP/SL pips come from the LLM (already range-validated by the parser)
// and both are placed broker-side (OCO) downstream. Ratchet/MaxHold are policy defaults from the
// caller (config). Returns a NONE signal when not actionable — fail-safe, never a half-built entry.
func BuildLLMSignal(side order.Side, entry, tpPips, slPips float64, qty, maxHoldMin int, ratchetArmPips, ratchetGivebackPips float64, name config.StrategyName, now time.Time) Signal {
	none := func(reason string) Signal {
		return Signal{Decision: DecisionNone, StrategyName: name, Reason: reason, CreatedAt: now}
	}
	if !side.Valid() || qty <= 0 || entry <= 0 || tpPips <= 0 || slPips <= 0 {
		return none("not_actionable")
	}
	return Signal{
		Decision:            DecisionEnter,
		Side:                side,
		EntryPrice:          entry,
		TakeProfitPips:      tpPips, // broker-side OCO
		StopLossPips:        slPips, // broker-side OCO
		MaxHoldMinutes:      maxHoldMin,
		RatchetArmPips:      ratchetArmPips,
		RatchetGivebackPips: ratchetGivebackPips,
		Quantity:            qty,
		Reason:              "llm_decision " + string(side),
		StrategyName:        name,
		CreatedAt:           now,
	}
}
