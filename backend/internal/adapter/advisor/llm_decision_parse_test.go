package advisor

import (
	"strings"
	"testing"

	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
)

// The autonomous LLM decision cycle lets Claude choose side + TP/SL each cycle. The parser is the
// safety-critical foundation: any garbled / out-of-range / inconsistent output MUST collapse to a
// NO-TRADE (flat is safe). Geometry that survives still flows through OnSignal's hard Gates + broker
// OCO downstream, but the parser is the first wall. Mirrors ParseBreakoutDecision's fail-safe doctrine.

func TestParseLLMDecision_FailSafe(t *testing.T) {
	cases := []struct {
		name     string
		stdout   string
		wantGo   bool
		wantSide string
	}{
		{"empty", "", false, "none"},
		{"garbled", "this is not yaml: : :", false, "none"},
		{"explicit_no_trade", "decision:\n  go: false\n  side: none\nreason_jp: 様子見", false, "none"},
		{"go_true_but_invalid_side", "decision:\n  go: true\n  side: SIDEWAYS\n  tp_pips: 20\n  sl_pips: 10", false, "none"},
		{"tp_zero", "decision:\n  go: true\n  side: BUY\n  tp_pips: 0\n  sl_pips: 10", false, "none"},
		{"sl_zero", "decision:\n  go: true\n  side: SELL\n  tp_pips: 20\n  sl_pips: 0", false, "none"},
		{"tp_absurd", "decision:\n  go: true\n  side: BUY\n  tp_pips: 5000\n  sl_pips: 10", false, "none"},
		{"sl_absurd", "decision:\n  go: true\n  side: BUY\n  tp_pips: 20\n  sl_pips: 9000", false, "none"},
		{"sl_too_tight", "decision:\n  go: true\n  side: BUY\n  tp_pips: 20\n  sl_pips: 0.5", false, "none"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, _ := ParseLLMDecision([]byte(c.stdout))
			if d.Go != c.wantGo {
				t.Errorf("Go = %v, want %v", d.Go, c.wantGo)
			}
			if d.Side != c.wantSide {
				t.Errorf("Side = %q, want %q", d.Side, c.wantSide)
			}
		})
	}
}

func TestParseLLMDecision_ValidBuy(t *testing.T) {
	stdout := "decision:\n  go: true\n  side: buy\n  entry: 161.20\n  tp_pips: 30\n  sl_pips: 15\nreason_jp: 上昇トレンドの押し目"
	d, err := ParseLLMDecision([]byte(stdout))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !d.Go || d.Side != "BUY" {
		t.Fatalf("want Go=true Side=BUY, got Go=%v Side=%q", d.Go, d.Side)
	}
	if d.TPPips != 30 || d.SLPips != 15 || d.Entry != 161.20 {
		t.Errorf("geometry: TP=%v SL=%v Entry=%v", d.TPPips, d.SLPips, d.Entry)
	}
	if d.Reason == "" {
		t.Errorf("reason should be carried through")
	}
}

func TestParseLLMDecision_ValidSellInFencedBlock(t *testing.T) {
	// LLMs often wrap YAML in a ```yaml fence; the shared cleanup must still parse it.
	stdout := "ここが判断です:\n```yaml\ndecision:\n  go: true\n  side: SELL\n  tp_pips: 25\n  sl_pips: 18\nreason_jp: 天井圏の戻り売り\n```"
	d, err := ParseLLMDecision([]byte(stdout))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !d.Go || d.Side != "SELL" || d.TPPips != 25 || d.SLPips != 18 {
		t.Fatalf("want SELL 25/18, got Go=%v Side=%q TP=%v SL=%v", d.Go, d.Side, d.TPPips, d.SLPips)
	}
}

// Failure mode: every pair sits at reason "yaml_unmarshal_error" and never trades.
// The trade-decider's `claude -p` can prepend PROSE that itself contains a fenced SHELL snippet
// (e.g. a `git diff` reacting to the project's enforcement files) BEFORE the real ```yaml decision.
// A parser that grabs the FIRST fence (the shell snippet) makes yaml.Unmarshal die. It must instead
// pick the fenced block that actually holds the `decision:` document.
func TestParseLLMDecision_PicksDecisionFenceAmongPreambleFences(t *testing.T) {
	stdout := "判断の前に設定を確認します:\n" +
		"```\n" +
		"cd ~/src/fx-bot\n" +
		"git diff .claude/settings.json\n" +
		"```\n" +
		"確認しました。では本日の判断です:\n" +
		"```yaml\n" +
		"decision:\n" +
		"  go: true\n" +
		"  side: BUY\n" +
		"  entry: 161.20\n" +
		"  tp_pips: 20\n" +
		"  sl_pips: 12\n" +
		"reason_jp: 上昇トレンドの押し目\n" +
		"```"
	d, err := ParseLLMDecision([]byte(stdout))
	if err != nil {
		t.Fatalf("must extract the decision fence past the shell-snippet fence, got err: %v", err)
	}
	if !d.Go || d.Side != "BUY" || d.TPPips != 20 || d.SLPips != 12 {
		t.Fatalf("want BUY 20/12, got Go=%v Side=%q TP=%v SL=%v", d.Go, d.Side, d.TPPips, d.SLPips)
	}
}

// Guard the fix's blast radius: a prose preamble with a non-decision fence and NO decision
// document anywhere must STILL fail safe to no-trade. Looking past the first fence must not let
// us start trading on garbage just because some other fenced block exists.
func TestParseLLMDecision_PreambleFenceButNoDecision_StaysNoTrade(t *testing.T) {
	stdout := "設定を確認します:\n```\ngit status\n```\nすみません、今回は判断を出力できません。"
	d, _ := ParseLLMDecision([]byte(stdout))
	if d.Go {
		t.Fatalf("no decision document → must be no-trade, got Go=true Side=%q", d.Side)
	}
}

// "Pick the decision fence" only covers FENCED preambles. claude can ignore "output ONLY the YAML
// (no prose, no fences)" and emit the decision document WITH a bare prose preamble and NO fence at
// all. With no fence to isolate, selectDecisionBlock would return the whole string and the prose
// would poison yaml.Unmarshal ("mapping values are not allowed in this context"), so EVERY such
// cycle would die at yaml_unmarshal_error → fail-safe no-trade. These must instead parse cleanly: cut the prose
// preamble down to the top-level `decision:` key.
func TestParseLLMDecision_NoFenceProsePreamble_NoTrade(t *testing.T) {
	// observed journal error: "yaml: line 3: mapping values are not allowed in this context"
	stdout := "no_trade をログにも出しておく方が一貫しているが、指示は trade-decider の YAML をそのまま出すこと。range 判定だが戦略Cの大前提(1h/6h flat)が崩れているため見送り。\n\n" +
		"decision:\n  go: false\n  tp_pips: 20\n  sl_pips: 15\n" +
		"reason_jp: \"戦略C(レンジ逆張り)を選択。大前提のレンジ判定が崩れているため見送り。\""
	d, err := ParseLLMDecision([]byte(stdout))
	if err != nil {
		t.Fatalf("prose preamble before decision: must be trimmed, not error; got %v", err)
	}
	if d.Go {
		t.Fatalf("decision says go:false → no-trade, got Go=true Side=%q", d.Side)
	}
	if d.Reason == "" || d.Reason == "yaml_unmarshal_error" {
		t.Errorf("must carry the parsed reason_jp (not the fail-safe sentinel), got %q", d.Reason)
	}
}

// Strongest case: a NO-FENCE prose preamble whose prose itself contains a colon ("ran:") — exactly
// the observed "Final decision:" shape — but with go:true. Without the preamble cut
// this is a MISSED TRADE (yaml_unmarshal_error → fail-safe no-trade); after, the BUY must survive.
func TestParseLLMDecision_NoFenceProsePreamble_GoTrueSurvives(t *testing.T) {
	stdout := "The two-step panel ran: market-regime classified the market, then trade-decider checked conditions. Final decision:\n\n" +
		"decision:\n  go: true\n  side: BUY\n  entry: 161.20\n  tp_pips: 30\n  sl_pips: 15\n" +
		"reason_jp: \"上昇トレンドの押し目を1h/5mで確認。\""
	d, err := ParseLLMDecision([]byte(stdout))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !d.Go || d.Side != "BUY" || d.TPPips != 30 || d.SLPips != 15 {
		t.Fatalf("prose preamble must not eat the trade; want BUY 30/15, got Go=%v Side=%q TP=%v SL=%v", d.Go, d.Side, d.TPPips, d.SLPips)
	}
}

// Observed error: "yaml: line 6: found unexpected end of stream". The decision is in a
// proper ```yaml fence, but claude DROPS the free-form reason_jp string's closing quote, so
// the whole document fails to unmarshal. reason_jp is non-safety-critical (a log field) and is the
// part most likely to break YAML; a broken reason must not discard the structured decision. Parse the
// decision: map on its own and best-effort the reason.
func TestParseLLMDecision_FencedButReasonQuoteUnterminated_NoTrade(t *testing.T) {
	stdout := "```yaml\n" +
		"decision:\n  go: false\n  entry: 0\n  tp_pips: 20\n  sl_pips: 15\n" +
		"reason_jp: \"regime=range なので戦略C(レンジ逆張り)を選択。複数条件が欠落のため no_trade。\n" + // closing quote dropped
		"```"
	d, err := ParseLLMDecision([]byte(stdout))
	if err != nil {
		t.Fatalf("a broken reason_jp must not error out the decision; got %v", err)
	}
	if d.Go {
		t.Fatalf("decision says go:false → no-trade, got Go=true Side=%q", d.Side)
	}
}

// The trade-decider can intermittently ignore the
// nested `decision:` map schema (trade-decider.md) and emits the OLD FLAT advisor schema instead:
// a SCALAR `decision: trade|no_trade` plus top-level side / take_profit_pips / stop_loss_pips
// (and noise keys strategy_name / regime_type / quantity / confidence). A nested-only parser
// dies at "cannot unmarshal !!str `trade` into struct{...}" → yaml_unmarshal_error → fail-safe
// no-trade, DISCARDING real decisions. The case below is a valid daytrade BUY
// (ma_pullback / trend_up / TP50 / SL25) that would be thrown away purely by schema drift — a
// missed trade, not a safety stop. The parser must accept the flat schema too.
func TestParseLLMDecision_FlatSchema_DecisionTradeScalar_BuySurvives(t *testing.T) {
	// a parse_fallback raw_stdout shape (a discarded BUY).
	stdout := "decision: trade\n" +
		"strategy_name: ma_pullback\n" +
		"regime_type: trend_up\n" +
		"side: BUY\n" +
		"take_profit_pips: 50\n" +
		"stop_loss_pips: 25\n" +
		"confidence: medium\n" +
		"reason_jp: \"全TFがup・6hは+52pipsの明確な上昇基調で regime=trend_up。戦略Aが最適合。\""
	d, err := ParseLLMDecision([]byte(stdout))
	if err != nil {
		t.Fatalf("flat schema must parse, not error; got %v", err)
	}
	if !d.Go || d.Side != "BUY" || d.TPPips != 50 || d.SLPips != 25 {
		t.Fatalf("flat schema must not eat the trade; want BUY 50/25, got Go=%v Side=%q TP=%v SL=%v", d.Go, d.Side, d.TPPips, d.SLPips)
	}
	if d.Reason == "" || d.Reason == "yaml_unmarshal_error" {
		t.Errorf("must carry reason_jp, got %q", d.Reason)
	}
}

// Flat schema variant using tp_pips/sl_pips key names inside a ```yaml fence.
func TestParseLLMDecision_FlatSchema_DecisionTradeScalar_Fenced_SellSurvives(t *testing.T) {
	stdout := "```yaml\n" +
		"decision: trade\n" +
		"side: SELL\n" +
		"strategy_name: range_fade\n" +
		"regime_type: range\n" +
		"tp_pips: 25\n" +
		"sl_pips: 18\n" +
		"quantity: 1000\n" +
		"reason_jp: \"戦略C(レンジ逆張り)を選択。\"\n" +
		"```"
	d, err := ParseLLMDecision([]byte(stdout))
	if err != nil {
		t.Fatalf("flat fenced schema must parse, not error; got %v", err)
	}
	if !d.Go || d.Side != "SELL" || d.TPPips != 25 || d.SLPips != 18 {
		t.Fatalf("want SELL 25/18, got Go=%v Side=%q TP=%v SL=%v", d.Go, d.Side, d.TPPips, d.SLPips)
	}
}

// Flat schema no_trade: scalar `decision: no_trade` + flat zero geometry must stay no-trade and
// still carry its reason (not the fail-safe sentinel).
func TestParseLLMDecision_FlatSchema_DecisionNoTradeScalar_StaysNoTrade(t *testing.T) {
	stdout := "decision: no_trade\n" +
		"strategy_name: range_fade\n" +
		"side: none\n" +
		"take_profit_pips: 0\n" +
		"stop_loss_pips: 0\n" +
		"reason_jp: \"regime=range で端に未到達=失速ポイントなし。見送り。\""
	d, err := ParseLLMDecision([]byte(stdout))
	if err != nil {
		t.Fatalf("flat no_trade must parse, not error; got %v", err)
	}
	if d.Go {
		t.Fatalf("decision: no_trade → no-trade, got Go=true Side=%q", d.Side)
	}
	if d.Reason == "" || d.Reason == "yaml_unmarshal_error" {
		t.Errorf("must carry reason_jp, got %q", d.Reason)
	}
}

// A trailing leftover sub-agent that emits ONLY chatter (no decision data anywhere) must fail safe
// to no-trade — there is nothing to recover, and we must never trade on prose. (journal EUR_JPY:
// "The final decision has already been output. ... no further action needed.")
func TestParseLLMDecision_LeftoverAgentChatter_StaysNoTrade(t *testing.T) {
	stdout := "The final decision has already been output. This is just a leftover no-op agent finishing — no further action needed."
	d, _ := ParseLLMDecision([]byte(stdout))
	if d.Go {
		t.Fatalf("chatter with no decision → must be no-trade, got Go=true Side=%q", d.Side)
	}
}

// A SUBMITTED trade (go:true SELL 30/25) can be journaled with an EMPTY reason — making the entry
// unauditable. ParseLLMDecision succeeds, so the reason is dropped SILENTLY by schema drift, not by
// a parse failure. The three
// silent-drop shapes below must all carry the reason through (the reason is observability-critical:
// it is the only record of WHY the bot entered).

// Drift shape 1: reason_jp nested INSIDE the decision: map (decisionMap had no reason field, so a
// clean unmarshal discarded it without any error).
func TestParseLLMDecision_ReasonJPNestedInDecisionMap_Carried(t *testing.T) {
	stdout := "decision:\n  go: true\n  side: SELL\n  entry: 1.32782\n  tp_pips: 30\n  sl_pips: 25\n  reason_jp: \"戻り売り: rpos24h 0.82 で規律充足\"\n"
	d, err := ParseLLMDecision([]byte(stdout))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !d.Go || d.Side != "SELL" || d.TPPips != 30 || d.SLPips != 25 {
		t.Fatalf("want SELL 30/25, got Go=%v Side=%q TP=%v SL=%v", d.Go, d.Side, d.TPPips, d.SLPips)
	}
	if d.Reason == "" {
		t.Errorf("a nested reason_jp must be carried, not silently dropped")
	}
}

// Drift shape 2: the key is `reason:` instead of `reason_jp:` (top level).
func TestParseLLMDecision_TopLevelReasonAlias_Carried(t *testing.T) {
	stdout := "decision:\n  go: true\n  side: BUY\n  entry: 161.20\n  tp_pips: 30\n  sl_pips: 25\nreason: \"上昇トレンドの押し目 rpos24h 0.41\"\n"
	d, err := ParseLLMDecision([]byte(stdout))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !d.Go || d.Side != "BUY" {
		t.Fatalf("want BUY, got Go=%v Side=%q", d.Go, d.Side)
	}
	if d.Reason == "" {
		t.Errorf("a `reason:` alias must be carried, not silently dropped")
	}
}

// Drift shape 3: reason_jp emitted BEFORE the decision: key. selectDecisionBlock slices the block
// to START at decision: (to drop prose preambles), which used to cut a leading reason_jp off with
// the preamble. The structured decision must still win, and the reason must be recovered.
func TestParseLLMDecision_ReasonJPBeforeDecisionKey_Carried(t *testing.T) {
	stdout := "reason_jp: \"戻り売り: 24h高値圏からの反転を確認\"\ndecision:\n  go: true\n  side: SELL\n  entry: 1.32782\n  tp_pips: 30\n  sl_pips: 25\n"
	d, err := ParseLLMDecision([]byte(stdout))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !d.Go || d.Side != "SELL" || d.TPPips != 30 || d.SLPips != 25 {
		t.Fatalf("want SELL 30/25, got Go=%v Side=%q TP=%v SL=%v", d.Go, d.Side, d.TPPips, d.SLPips)
	}
	if d.Reason == "" {
		t.Errorf("a reason_jp ahead of decision: must be recovered, not cut off with the preamble")
	}
}

// Guard the recovery's blast radius: prose that merely MENTIONS reason_jp mid-line (e.g. echoing
// the schema) must not be scraped into the reason; and a decision with NO reason anywhere stays
// empty (the CLI layer journals the raw stdout for diagnosis in that case).
func TestParseLLMDecision_NoReasonAnywhere_StaysEmpty(t *testing.T) {
	stdout := "decision:\n  go: true\n  side: SELL\n  entry: 1.32782\n  tp_pips: 30\n  sl_pips: 25\n"
	d, err := ParseLLMDecision([]byte(stdout))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !d.Go || d.Side != "SELL" {
		t.Fatalf("want SELL, got Go=%v Side=%q", d.Go, d.Side)
	}
	if d.Reason != "" {
		t.Errorf("no reason in the document → Reason must stay empty (no fabrication), got %q", d.Reason)
	}
}

// Same broken-quote failure mode but go:true: a dropped reason_jp closing quote must not cost us a
// real trade. The structured decision (SELL 25/18) must survive on the decision-only fallback.
func TestParseLLMDecision_FencedReasonQuoteUnterminated_GoTrueSurvives(t *testing.T) {
	stdout := "```yaml\n" +
		"decision:\n  go: true\n  side: SELL\n  tp_pips: 25\n  sl_pips: 18\n" +
		"reason_jp: \"天井圏の戻り売り。RR良好で\n" + // closing quote dropped
		"```"
	d, err := ParseLLMDecision([]byte(stdout))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !d.Go || d.Side != "SELL" || d.TPPips != 25 || d.SLPips != 18 {
		t.Fatalf("broken reason must not eat the trade; want SELL 25/18, got Go=%v Side=%q TP=%v SL=%v", d.Go, d.Side, d.TPPips, d.SLPips)
	}
}

// Arms: the LLM may pre-place up to 2 conditional plans (one per
// side) — "if price breaks LEVEL, enter SIDE with TP/SL". Fail-safe normalization: invalid side /
// trigger word / non-positive price / insane pips / direction-inconsistent plans (BUY must
// break_above, SELL must break_below) are DROPPED individually; more than one plan per side keeps
// the first; go:true discards arms entirely (an immediate entry occupies the single slot).
func TestParseLLMDecision_Arms(t *testing.T) {
	stdout := `decision:
  go: false
arms:
  - side: SELL
    trigger: break_below
    price: 161.95
    tp_pips: 30
    sl_pips: 25
    reason: "[L1] 戻り安値割れで下落再開"
  - side: BUY
    trigger: break_above
    price: 162.45
    tp_pips: 30
    sl_pips: 25
    reason: "[L4] 揉み合い上限抜けで再加速"
reason_jp: "[arm] L1/L4の土俵は成立・再転換待ちのため両側を武装"`
	d, err := ParseLLMDecision([]byte(stdout))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if d.Go {
		t.Fatalf("go must stay false")
	}
	if len(d.Arms) != 2 {
		t.Fatalf("want 2 arms, got %d: %+v", len(d.Arms), d.Arms)
	}
	s := d.Arms[0]
	if s.Side != order.SideSell || s.BreakAbove || s.TriggerPrice != 161.95 || s.TPPips != 30 || s.SLPips != 25 {
		t.Errorf("sell arm: %+v", s)
	}
	if !strings.Contains(s.Reason, "[L1]") {
		t.Errorf("sell arm must carry its lane tag, got %q", s.Reason)
	}
	b := d.Arms[1]
	if b.Side != order.SideBuy || !b.BreakAbove || b.TriggerPrice != 162.45 {
		t.Errorf("buy arm: %+v", b)
	}
}

func TestParseLLMDecision_Arms_FailSafeNormalization(t *testing.T) {
	cases := []struct {
		name   string
		yaml   string
		nArms  int
		checks func(t *testing.T, d LLMDecision)
	}{
		{"go:true discards arms", `decision:
  go: true
  side: SELL
  tp_pips: 30
  sl_pips: 25
arms:
  - side: BUY
    trigger: break_above
    price: 162.45
    tp_pips: 30
    sl_pips: 25
reason_jp: "[L1] 即エントリー"`, 0, func(t *testing.T, d LLMDecision) {
			if !d.Go || d.Side != "SELL" {
				t.Errorf("immediate entry must survive: %+v", d)
			}
		}},
		{"direction-inconsistent plan dropped (BUY+break_below)", `decision:
  go: false
arms:
  - side: BUY
    trigger: break_below
    price: 161.95
    tp_pips: 30
    sl_pips: 25
reason_jp: "[arm] x"`, 0, nil},
		{"second plan on the same side dropped", `decision:
  go: false
arms:
  - side: SELL
    trigger: break_below
    price: 161.95
    tp_pips: 30
    sl_pips: 25
    reason: "[L1] a"
  - side: SELL
    trigger: break_below
    price: 161.80
    tp_pips: 30
    sl_pips: 25
    reason: "[L1] b"
reason_jp: "[arm] x"`, 1, func(t *testing.T, d LLMDecision) {
			if d.Arms[0].TriggerPrice != 161.95 {
				t.Errorf("first same-side plan must win, got %+v", d.Arms[0])
			}
		}},
		{"insane pips dropped", `decision:
  go: false
arms:
  - side: SELL
    trigger: break_below
    price: 161.95
    tp_pips: 9000
    sl_pips: 25
reason_jp: "[arm] x"`, 0, nil},
		{"zero price dropped", `decision:
  go: false
arms:
  - side: SELL
    trigger: break_below
    price: 0
    tp_pips: 30
    sl_pips: 25
reason_jp: "[arm] x"`, 0, nil},
		{"unknown trigger word dropped", `decision:
  go: false
arms:
  - side: SELL
    trigger: touch
    price: 161.95
    tp_pips: 30
    sl_pips: 25
reason_jp: "[arm] x"`, 0, nil},
		{"plan without its own reason inherits reason_jp", `decision:
  go: false
arms:
  - side: SELL
    trigger: break_below
    price: 161.95
    tp_pips: 30
    sl_pips: 25
reason_jp: "[L1] 戻り待ち武装"`, 1, func(t *testing.T, d LLMDecision) {
			if !strings.Contains(d.Arms[0].Reason, "[L1]") {
				t.Errorf("plan must inherit reason_jp, got %q", d.Arms[0].Reason)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := ParseLLMDecision([]byte(tc.yaml))
			if len(d.Arms) != tc.nArms {
				t.Fatalf("want %d arms, got %d: %+v", tc.nArms, len(d.Arms), d.Arms)
			}
			if tc.checks != nil {
				tc.checks(t, d)
			}
		})
	}
}

// ToDomain must carry the arms through to the domain decision the cycle consumes.
func TestLLMDecision_ToDomain_CarriesArms(t *testing.T) {
	d := LLMDecision{Go: false, Side: "none", Arms: []strategy.ArmedPlan{
		{Side: order.SideSell, TriggerPrice: 161.95, TPPips: 30, SLPips: 25, Reason: "[L1] x"},
	}}
	dom := d.ToDomain()
	if len(dom.Arms) != 1 || dom.Arms[0].TriggerPrice != 161.95 {
		t.Fatalf("arms must survive ToDomain: %+v", dom.Arms)
	}
}
