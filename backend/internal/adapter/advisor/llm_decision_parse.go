package advisor

import (
	"errors"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
)

// LLMDecision is the parsed output of the autonomous trade-decider subagent (.claude/agents/
// trade-decider.md). Unlike the advisor_v2 breakout judge (which only gates deterministic geometry),
// this lets Claude choose side + TP/SL each cycle (the autonomous LLM trade loop). The decision is
// still downstream of OnSignal's hard Gates + broker OCO, and is forced to the configured quantity;
// this parser is the FIRST safety wall.
//
// FAIL-SAFE: any empty / garbled / out-of-range / inconsistent output collapses to a NO-TRADE
// (Go=false, Side="none"). Flat is safe; on bad output we never trade. TP/SL must be sane absolute
// pip magnitudes (so the LLM cannot emit a 0.5-pip or 9000-pip stop that survives downstream).
type LLMDecision struct {
	Go     bool
	Side   string  // BUY | SELL | none
	Entry  float64 // reference entry price (0 = enter at market)
	TPPips float64
	SLPips float64
	Reason string
	// Arms are the conditional plans (already fail-safe normalized: valid side/trigger/
	// price/pips, direction-consistent, ≤1 per side, dropped entirely when Go=true).
	Arms []strategy.ArmedPlan
}

// Sane absolute bounds for LLM-chosen geometry. These are SAFETY limits, not a strategy: they only
// reject absurd magnitudes. RR / frequency / direction quality are left to the playbook + reflection
// loop, and the per-trade max-loss cap (ValidateSignalBoundaries) is the absolute downstream backstop.
const (
	llmMinPips = 2.0
	llmMaxPips = 300.0
)

func noTradeLLM(reason string) LLMDecision {
	return LLMDecision{Go: false, Side: "none", Reason: reason}
}

// ToDomain maps the parsed adapter decision to the domain type the usecase consumes (keeps the
// usecase free of any adapter import — composition wires this, like BreakoutDecision.Verdict()).
func (d LLMDecision) ToDomain() strategy.LLMTradeDecision {
	side := order.Side("")
	if d.Go {
		side = order.Side(d.Side)
	}
	return strategy.LLMTradeDecision{
		Go: d.Go, Side: side, Entry: d.Entry, TPPips: d.TPPips, SLPips: d.SLPips, Reason: d.Reason,
		Arms: d.Arms,
	}
}

// rawArm is one entry of the `arms:` list as the LLM writes it. Normalized (and possibly
// dropped) by normalizeArms — never trusted as-is.
type rawArm struct {
	Side    string  `yaml:"side"`
	Trigger string  `yaml:"trigger"` // break_above | break_below
	Price   float64 `yaml:"price"`
	TPPips  float64 `yaml:"tp_pips"`
	SLPips  float64 `yaml:"sl_pips"`
	Reason  string  `yaml:"reason"`
}

// normalizeArms fail-safe-filters the raw arms list: each plan must have a valid side, a known
// trigger word, a positive price, sane pip magnitudes, and a DIRECTION-CONSISTENT trigger (the
// lanes are continuation-only: BUY fires on break_above, SELL on break_below — a counter-shaped
// plan is dropped, not "fixed"). At most one plan per side (first wins). A plan without its own
// reason inherits the document-level reason so the lane tag is never lost.
func normalizeArms(raws []rawArm, fallbackReason string) []strategy.ArmedPlan {
	out := make([]strategy.ArmedPlan, 0, 2)
	seen := map[order.Side]bool{}
	for _, a := range raws {
		side := order.Side(strings.ToUpper(strings.TrimSpace(a.Side)))
		if !side.Valid() || seen[side] {
			continue
		}
		var breakAbove bool
		switch strings.ToLower(strings.TrimSpace(a.Trigger)) {
		case "break_above", "above", "up":
			breakAbove = true
		case "break_below", "below", "down":
			breakAbove = false
		default:
			continue // unknown trigger word — fail-safe drop
		}
		if (side == order.SideBuy) != breakAbove {
			continue // direction-inconsistent (counter-shaped) plan
		}
		if a.Price <= 0 ||
			a.TPPips < llmMinPips || a.TPPips > llmMaxPips ||
			a.SLPips < llmMinPips || a.SLPips > llmMaxPips {
			continue
		}
		reason := firstNonEmpty(a.Reason, fallbackReason)
		out = append(out, strategy.ArmedPlan{
			Side: side, TriggerPrice: a.Price, BreakAbove: breakAbove,
			TPPips: a.TPPips, SLPips: a.SLPips, Reason: reason,
		})
		seen[side] = true
	}
	return out
}

// decisionMap is the canonical nested schema trade-decider.md asks for (decision: {go, side, ...}).
// ReasonJP/Reason tolerate the drift where claude indents reason_jp INSIDE the map: without these
// fields the unmarshal succeeds and the reason vanishes silently — a submitted trade would be
// journaled with an empty reason, unauditable after the fact.
type decisionMap struct {
	Go       bool    `yaml:"go"`
	Side     string  `yaml:"side"`
	Entry    float64 `yaml:"entry"`
	TPPips   float64 `yaml:"tp_pips"`
	SLPips   float64 `yaml:"sl_pips"`
	ReasonJP string  `yaml:"reason_jp"`
	Reason   string  `yaml:"reason"`
}

// rawLLMDecision tolerates BOTH the canonical nested schema AND the OLD FLAT advisor schema the
// trade-decider keeps drifting back to: a SCALAR `decision: trade|no_trade` plus
// top-level side / tp_pips|take_profit_pips / sl_pips|stop_loss_pips. `Decision` is a yaml.Node so a
// scalar there no longer hard-errors the unmarshal; resolve() then reads the flat top-level fields.
// Without this, schema drift discards genuine trades as yaml_unmarshal_error.
type rawLLMDecision struct {
	Decision yaml.Node `yaml:"decision"` // nested map (canonical) OR scalar trade|no_trade (flat)

	// Flat / legacy top-level fields, used only when Decision is not a nested map.
	Go             *bool   `yaml:"go"`
	Side           string  `yaml:"side"`
	Entry          float64 `yaml:"entry"`
	TPPips         float64 `yaml:"tp_pips"`
	SLPips         float64 `yaml:"sl_pips"`
	TakeProfitPips float64 `yaml:"take_profit_pips"`
	StopLossPips   float64 `yaml:"stop_loss_pips"`

	ReasonJP string `yaml:"reason_jp"`
	Reason   string `yaml:"reason"` // key-name drift: `reason:` instead of `reason_jp:`

	// Arms is the top-level conditional-plan list (sibling of decision:). Tolerated on
	// both schemas; normalized fail-safe by normalizeArms and dropped entirely on go:true.
	Arms []rawArm `yaml:"arms"`
}

// goWords are the scalar `decision:` values that mean ENTER. Anything else (no_trade / none / no /
// false / "") resolves to go:false — fail-safe: an unrecognized verb never trades.
var goWords = map[string]bool{"trade": true, "enter": true, "go": true, "yes": true, "true": true}

// resolve collapses either schema into the canonical decisionMap. The nested `decision:` map is
// authoritative when present; otherwise the verb comes from the scalar `decision:` (or a top-level
// `go:`) and the geometry from the flat top-level fields (tp_pips OR take_profit_pips, etc.).
func (raw rawLLMDecision) resolve() decisionMap {
	if raw.Decision.Kind == yaml.MappingNode {
		var d decisionMap
		_ = raw.Decision.Decode(&d) // canonical nested schema
		return d
	}
	d := decisionMap{Side: raw.Side, Entry: raw.Entry}
	if raw.Decision.Kind == yaml.ScalarNode {
		d.Go = goWords[strings.ToLower(strings.TrimSpace(raw.Decision.Value))]
	}
	if raw.Go != nil { // an explicit top-level go: still wins (e.g. flat schema with go:true)
		d.Go = *raw.Go
	}
	d.TPPips = firstNonZero(raw.TPPips, raw.TakeProfitPips)
	d.SLPips = firstNonZero(raw.SLPips, raw.StopLossPips)
	return d
}

func firstNonZero(a, b float64) float64 {
	if a != 0 {
		return a
	}
	return b
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if t := strings.TrimSpace(s); t != "" {
			return t
		}
	}
	return ""
}

// decisionKeyRE marks the genuine trade-decider document: a top-level `decision:` map (go/side/
// tp_pips/sl_pips live under it). It is how selectDecisionBlock tells the real decision from OTHER
// fenced blocks claude may emit in a prose preamble, and where it cuts a no-fence prose preamble.
var decisionKeyRE = regexp.MustCompile(`(?m)^[ \t]*decision\s*:`)

// reasonJPLineRE grabs the reason_jp value best-effort (one line, possibly quote-opened). Used only
// when the structured-only fallback runs, so a broken reason field can't discard the whole decision.
var reasonJPLineRE = regexp.MustCompile(`(?m)^[ \t]*reason_jp[ \t]*:[ \t]*(.*)$`)

// selectDecisionBlock returns the YAML holding the trade-decider's decision, with any prose preamble
// BEFORE the `decision:` key trimmed off. claude keeps ignoring "output ONLY the YAML (no prose, no
// fences)" two ways, both of which would otherwise die at yaml_unmarshal_error → every cycle
// fail-safe to no-trade:
//   - prose that ITSELF contains fenced blocks (a `git diff` shell snippet) ahead of the
//     real ```yaml, so "the first fence" is not trustworthy.
//   - the decision document with a BARE prose preamble and NO fence at all
//     ("The two-step panel ran: ... Final decision:\n\ndecision:\n ..."); with no fence to isolate,
//     returning the whole string let the prose (e.g. the colon in "ran:") poison yaml.Unmarshal.
//
// We collect every fenced block plus the fence-stripped whole as a final candidate, and return the
// first that contains a top-level `decision:` key — sliced to START at that key so any preamble
// (fenced-internal or fence-less) is dropped. If none contains `decision:` we return the fence-
// stripped whole so genuinely malformed output still fails downstream → no-trade (fail-safe). Mirrors
// selectBody's "pick the block with the real schema keys" doctrine used for strategy_config.
//
// NOTE: ResponseParser.Parse (the strategy_config advisor path, off by default) still uses first-fence
// selection + selectBody and has the same latent blind spot; adopt this approach there if re-enabled.
func selectDecisionBlock(s string) string {
	whole := strings.ReplaceAll(openFenceRE.ReplaceAllString(s, ""), "```", "")
	candidates := make([]string, 0, 4)
	for _, m := range fenceBlockRE.FindAllStringSubmatch(s, -1) {
		if strings.TrimSpace(m[1]) != "" {
			candidates = append(candidates, m[1])
		}
	}
	candidates = append(candidates, whole)
	for _, c := range candidates {
		if loc := decisionKeyRE.FindStringIndex(c); loc != nil {
			return c[loc[0]:] // drop prose preamble before the decision: key
		}
	}
	return whole
}

// reasonJPKeyRE anchors the (always top-level) reason_jp key so structuredHead can cut it off.
var reasonJPKeyRE = regexp.MustCompile(`(?m)^[ \t]*reason_jp[ \t]*:`)

// structuredHead isolates the safety-critical structured part — everything from the `decision:`
// anchor up to (but excluding) the free-form reason_jp: line. reason_jp is the field most likely to
// break YAML (an unterminated/embedded quote → "found unexpected end of stream"), so dropping it
// lets a broken log field NOT discard a valid decision. Unlike a decision-map-only slice, this keeps top-level flat fields (side / tp_pips / take_profit_pips …) so the flat schema's
// geometry also survives a broken reason. Returns ("", false) if there is no decision: key to anchor.
func structuredHead(block string) (string, bool) {
	loc := decisionKeyRE.FindStringIndex(block)
	if loc == nil {
		return "", false
	}
	head := block[loc[0]:]
	if r := reasonJPKeyRE.FindStringIndex(head); r != nil {
		head = head[:r[0]]
	}
	return head, true
}

// ParseLLMDecision turns the trade-decider CLI stdout into an LLMDecision. On any problem it returns
// a no-trade decision (and, for empty/garbled input, the error); the caller logs it but is safe to
// act on the returned no-trade either way. Reuses the same messy-output cleanup as the other parsers.
func ParseLLMDecision(stdout []byte) (LLMDecision, error) {
	if len(strings.TrimSpace(string(stdout))) == 0 {
		return noTradeLLM("empty_stdout"), errors.New("empty stdout")
	}
	s := spaceEntityRE.ReplaceAllString(string(stdout), " ")
	s = colonNoSpaceRE.ReplaceAllString(s, "$1: $2")
	cleaned := s // pre-slice text, kept ONLY for the best-effort reason recovery below
	s = selectDecisionBlock(s)

	var raw rawLLMDecision
	if err := yaml.Unmarshal([]byte(s), &raw); err != nil {
		// The full document failed — almost always the free-form reason_jp string (an embedded or
		// unterminated quote: "found unexpected end of stream"). Retry on the structured
		// decision: map ALONE so a broken log field can't discard a valid, safety-critical decision;
		// recover the reason best-effort. Only if even the map won't parse do we fail-safe to no-trade.
		head, ok := structuredHead(s)
		if !ok {
			return noTradeLLM("yaml_unmarshal_error"), err
		}
		raw = rawLLMDecision{}
		if err2 := yaml.Unmarshal([]byte(head), &raw); err2 != nil {
			return noTradeLLM("yaml_unmarshal_error"), err
		}
		if m := reasonJPLineRE.FindStringSubmatch(s); m != nil {
			raw.ReasonJP = strings.Trim(strings.TrimSpace(m[1]), `"'`)
		}
	}
	d := raw.resolve()
	// The reason is observability-critical (the only record of WHY the bot entered — a trade with an
	// empty reason is unauditable), so tolerate every drift shape seen:
	// top-level reason_jp (canonical) / reason alias / nested inside the decision: map / emitted
	// BEFORE the decision: key (which selectDecisionBlock slices off with the prose preamble).
	reason := firstNonEmpty(raw.ReasonJP, raw.Reason, d.ReasonJP, d.Reason)
	if reason == "" {
		if m := reasonJPLineRE.FindStringSubmatch(cleaned); m != nil {
			reason = strings.Trim(strings.TrimSpace(m[1]), `"'`)
		}
	}

	if !d.Go {
		// A no_trade decision may still ARM conditional plans (normalized fail-safe).
		// go:true never carries arms — the immediate entry occupies the single position slot.
		return LLMDecision{Go: false, Side: "none", Reason: reason, Arms: normalizeArms(raw.Arms, reason)}, nil
	}
	side := strings.ToUpper(strings.TrimSpace(d.Side))
	if side != "BUY" && side != "SELL" {
		return noTradeLLM("invalid_side:" + d.Side), nil
	}
	if d.TPPips < llmMinPips || d.TPPips > llmMaxPips {
		return noTradeLLM("tp_out_of_range"), nil
	}
	if d.SLPips < llmMinPips || d.SLPips > llmMaxPips {
		return noTradeLLM("sl_out_of_range"), nil
	}

	return LLMDecision{
		Go: true, Side: side, Entry: d.Entry,
		TPPips: d.TPPips, SLPips: d.SLPips, Reason: reason,
	}, nil
}
