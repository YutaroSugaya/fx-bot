package advisor

import (
	"errors"
	"strings"

	"gopkg.in/yaml.v3"

	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
)

// BreakoutDecision is the parsed judgment from the advisor v2 `breakout-advisor` subagent
// (see .claude/agents/breakout-advisor.md). The LLM only JUDGES;
// it never places orders. The deterministic engine + risk Gate act on this (and can still veto).
//
// FAIL-SAFE: every ambiguity collapses to a NO-TRADE (Go=false, Label=no_trade). "Flat is the
// strongest position" — on bad/garbled/contradictory output we never trade. This is the safe
// direction for real money and matches the no_trade-default doctrine.
type BreakoutDecision struct {
	Go           bool
	Label        string // trend_continuation | countertrend_reversion | no_trade
	Side         string // BUY | SELL | none
	Level        float64
	ATRPips      float64
	Invalidation float64
	Reason       string
}

const (
	LabelTrendContinuation = "trend_continuation"
	LabelCountertrend      = "countertrend_reversion"
	LabelNoTrade           = "no_trade"
)

// noTradeDecision is the safe default returned on any parse problem or inconsistency.
func noTradeDecision(reason string) BreakoutDecision {
	return BreakoutDecision{Go: false, Label: LabelNoTrade, Side: "none", Reason: reason}
}

// Verdict maps the parsed decision to the domain-level AdvisorVerdict the usecase consumes
// (keeps the usecase free of any adapter import — composition wires this).
func (d BreakoutDecision) Verdict() strategy.AdvisorVerdict {
	return strategy.AdvisorVerdict{
		Go:           d.Go,
		Label:        strategy.BreakoutLabel(d.Label),
		Side:         order.Side(d.Side),
		Level:        d.Level,
		ATRPips:      d.ATRPips,
		Invalidation: d.Invalidation,
		Reason:       d.Reason,
	}
}

type rawBreakout struct {
	Decision struct {
		Go           bool    `yaml:"go"`
		Label        string  `yaml:"label"`
		Side         string  `yaml:"side"`
		Level        float64 `yaml:"level"`
		ATRPips      float64 `yaml:"atr_pips"`
		Invalidation float64 `yaml:"invalidation"`
	} `yaml:"decision"`
	ReasonJP string `yaml:"reason_jp"`
}

// ParseBreakoutDecision turns the breakout-advisor CLI stdout into a BreakoutDecision.
// On any error it returns a no-trade decision AND the error (the caller logs it but is safe
// to act on the returned no-trade either way).
func ParseBreakoutDecision(stdout []byte) (BreakoutDecision, error) {
	if len(strings.TrimSpace(string(stdout))) == 0 {
		return noTradeDecision("empty_stdout"), errors.New("empty stdout")
	}
	// Reuse the same messy-output cleanup the strategy-config parser uses (same package):
	// restore &#32;, fix missing space after colon, then prefer a fenced ```yaml block.
	s := spaceEntityRE.ReplaceAllString(string(stdout), " ")
	s = colonNoSpaceRE.ReplaceAllString(s, "$1: $2")
	if m := fenceBlockRE.FindStringSubmatch(s); m != nil && strings.TrimSpace(m[1]) != "" {
		s = m[1]
	} else {
		s = openFenceRE.ReplaceAllString(s, "")
		s = strings.ReplaceAll(s, "```", "")
	}

	var raw rawBreakout
	if err := yaml.Unmarshal([]byte(s), &raw); err != nil {
		return noTradeDecision("yaml_unmarshal_error"), err
	}

	d := raw.Decision
	reason := strings.TrimSpace(raw.ReasonJP)

	// Not a go, or explicitly no_trade -> safe no-trade.
	if !d.Go || d.Label == LabelNoTrade || d.Label == "" {
		return BreakoutDecision{Go: false, Label: LabelNoTrade, Side: "none", Reason: reason}, nil
	}
	// Validate label.
	if d.Label != LabelTrendContinuation && d.Label != LabelCountertrend {
		return noTradeDecision("unknown_label:" + d.Label), nil
	}
	// Normalize + validate side.
	side := strings.ToUpper(strings.TrimSpace(d.Side))
	if side != "BUY" && side != "SELL" {
		return noTradeDecision("invalid_side:" + d.Side), nil
	}
	// Geometry sanity: a go decision must carry a usable level/invalidation/atr.
	if d.Level <= 0 || d.Invalidation <= 0 || d.ATRPips <= 0 {
		return noTradeDecision("incomplete_geometry"), nil
	}
	// Invalidation must be on the correct side of the level (below for BUY, above for SELL).
	if side == "BUY" && !(d.Invalidation < d.Level) {
		return noTradeDecision("invalidation_not_below_level"), nil
	}
	if side == "SELL" && !(d.Invalidation > d.Level) {
		return noTradeDecision("invalidation_not_above_level"), nil
	}

	return BreakoutDecision{
		Go: true, Label: d.Label, Side: side,
		Level: d.Level, ATRPips: d.ATRPips, Invalidation: d.Invalidation, Reason: reason,
	}, nil
}
