package advisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
)

// LLMDecisionCLI runs the per-cycle trade decision (autonomous LLM trade loop):
// by default ONE `claude -p --tools Read` with an inline single-agent prompt
// (BuildSingleAgentDecisionPayload); the market-regime/trade-decider subagent panel
// (BuildDecisionPayload) remains only for decision_single_agent:false. It returns a domain
// LLMTradeDecision. It is the I/O boundary; the orchestration
// (command.LLMDecisionCycle) and parser (ParseLLMDecision) hold the tested logic.
//
// FAIL-SAFE: a CLI/transport failure returns a no-trade decision AND an error (cycle logs/retries);
// a garbled-output PARSE failure returns a no-trade decision and NO error (garbage = stay flat). The
// decision is still downstream of OnSignal's Gates + broker OCO and forced to the config qty.
type LLMDecisionCLI struct {
	CLIPath        string
	WorkingDir     string
	TimeoutSeconds int
	Logger         *slog.Logger
	ExecCommand    ExecCommandFunc

	// TransientRetries / TransientRetryBackoff mirror ClaudeCLIAdvisor (claude_cli.go):
	// claude prints transient infra errors (529/overload/connection) on STDOUT and a
	// short retry self-heals the cycle. usage/session limits are NOT retried (they reset
	// hours later). New() sets 1 / 5s.
	TransientRetries      int
	TransientRetryBackoff time.Duration

	// Journal durably records the raw claude stdout on a parse fallback (optional; nil = no-op).
	// Without it the only record of claude's malformed output is a transient slog line, which
	// leaves a silent no-trade streak undiagnosable after the fact.
	Journal port.LLMDecisionJournal

	// SingleAgent collapses the decision to ONE claude pass (no sub-agent panel), invoked with
	// --tools Read (no Task) so sub-agents cannot spawn. Default behaviour for a zero-value CLI is
	// the panel (back-compat with existing tests); wiring sets this from config.SingleAgentDecision().
	SingleAgent bool
}

// NewLLMDecisionCLI builds a decider with exec.CommandContext and a default timeout.
func NewLLMDecisionCLI(cli, workingDir string, timeoutSec int, logger *slog.Logger) *LLMDecisionCLI {
	if timeoutSec <= 0 {
		timeoutSec = 120
	}
	return &LLMDecisionCLI{
		CLIPath: cli, WorkingDir: workingDir, TimeoutSeconds: timeoutSec,
		Logger: logger, ExecCommand: exec.CommandContext,
		// 2 retries (3 attempts): server-side rate limits during the 5-pair burst can
		// take a couple of back-offs to clear. Bounded well under the per-cycle timeout,
		// and the hourly cadence is the ultimate retry.
		TransientRetries:      2,
		TransientRetryBackoff: 5 * time.Second,
	}
}

// Shared prompt fragments so the panel and single-agent builders cannot drift on the decision rule
// or the output contract.
const (
	// decisionBias is the decision rule: the playbook is a strict CHECKLIST, evaluated with the
	// summary's numbers. It keeps the anti-deadlock intent ("when a lane checks you MUST enter —
	// small losses acceptable") but carries no softeners ("guidelines, not strict gates" /
	// "roughly holds"), which let past-performance stories override live structure (e.g.
	// counter-trend SELLs into a rising day).
	decisionBias = "Decision rule: the playbook is a strict CHECKLIST, not advice. When EVERY box of a playbook lane checks against the summary's numbers, you MUST enter (BUY or SELL) at the configured size (quantity is code-enforced; you never choose it) — do not hesitate or demand confirmation beyond the checklist; small losses are acceptable. When any box fails, or a 【HARD禁止】 rule applies, you MUST answer no_trade — never improvise an entry the playbook does not define, and never override a live checklist with past-performance stories. 【HARD禁止】 rules are re-vetoed by deterministic code downstream (journal stages: night_buy_veto / chase_buy_veto / sell_low_veto / htf_trend_veto / exhaustion_veto / spike_veto), so proposing such an entry only wastes the cycle. The catastrophe caps (per-trade / daily-loss / spread) remain the hard floor.\n"

	// decisionSchema is the exact YAML the parser expects (ParseLLMDecision; nested decision: map).
	// reason_jp MUST open with the machine-parsable lane tag ([L1] / [L4] / [no_trade:<欠けた条件>])
	// so the journal supports automatic per-lane P&L attribution.
	decisionSchema = "decision:\n  go: <true|false>\n  side: <BUY|SELL>        # omit when go:false\n  entry: <reference price or 0>\n  tp_pips: <number>\n  sl_pips: <number>\narms:                     # OPTIONAL and only with go:false — up to 2 conditional plans (max one per side)\n  - side: <BUY|SELL>\n    trigger: <break_above|break_below>   # BUY=break_above / SELL=break_below only\n    price: <level>\n    tp_pips: <number>\n    sl_pips: <number>\n    reason: \"[L1|L4] <which timing box this level's break confirms>\"\nreason_jp: \"[L1|L4|arm|no_trade:<最初に欠けた条件>] <checked numbers, 1-2 sentences>\"\n"

	// decisionOutputContract forces the FINAL message to BE the YAML and bans trailing status remarks
	// (a stray agent's closing remark can otherwise replace the decision in claude -p's
	// final-message-only stdout). Phrased to apply whether or not a subagent produced it.
	decisionOutputContract = "OUTPUT CONTRACT (critical): your VERY LAST message MUST BE the YAML decision document and nothing else. Even if it was already produced, REPEAT the exact YAML as your final message. Do NOT end with any status/closing remark (e.g. \"the decision was already output\", \"no further action needed\", a stray/noop/leftover wait agent) and do NOT wait on background agents — only your final message is read, and anything that is not the YAML loses the decision.\n"
)

// decisionBotState is the TRIMMED bot state the decision prompt carries: just the position slot (an
// open position → no new entry anyway). The advisor-era extras (mode / emergency_stop / daily
// P&L / consecutive losses / window counters) are code-enforced and only invite mood-based
// drift in the LLM's reasoning. trades_today is omitted too: it counts from a DIFFERENT day
// definition (0:00 JST, all-symbol) than the trading day (06:00 JST, per-symbol) and would show
// the LLM contradictory evidence.
type decisionBotState struct {
	CurrentPosition    *string `json:"current_position"`
	OpenPositionsCount int     `json:"open_positions_count"`
}

// decisionInput is the decision prompt payload (不要なコンテキストを渡さない). It
// carries ONLY what the playbook checklist evaluates: the TF windows (incl. change_pips — the
// lane yardstick), current rate/spread, the trimmed bot state, the event context and the
// playbook text. Dropped vs the full MarketSummary: hard_limits, allowed_strategies,
// recent_decisions (the LLM anchoring on its own past calls), recent_rejections,
// next_valid_from — and today_closed_trades (no checklist box consumes it, so sending it would
// only invite mood-based self-throttling; the cycle still injects it into
// summary.RecentTrades for the dormant daily_loss_stop code mechanism).
type decisionInput struct {
	Symbol string `json:"symbol"`
	// TimeJST is the decision-time wall clock in JST ("2006-01-02 15:04 JST") — the clock the
	// playbook's time-of-day boxes (JST 0〜5時台) are judged against (without it the trimmed
	// payload carries no explicit clock and night rules are unevaluable).
	TimeJST      string               `json:"time_jst,omitempty"`
	CurrentRate  market.CurrentRate   `json:"current_rate"`
	Summary5m    market.WindowSummary `json:"summary_5m"`
	Summary15m   market.WindowSummary `json:"summary_15m"`
	Summary1h    market.WindowSummary `json:"summary_1h"`
	Summary6h    market.WindowSummary `json:"summary_6h"`
	Summary24h   market.WindowSummary `json:"summary_24h"`
	BotState     decisionBotState     `json:"bot_state"`
	EventContext *market.EventContext `json:"event_context,omitempty"`
	Playbook     string               `json:"playbook"`
}

func decisionInputJSON(symbol string, summary *market.MarketSummary, playbook string) ([]byte, error) {
	input := decisionInput{Symbol: symbol, Playbook: playbook}
	if summary != nil {
		if !summary.Time.IsZero() {
			input.TimeJST = summary.Time.In(time.FixedZone("JST", 9*3600)).Format("2006-01-02 15:04 JST")
		}
		input.CurrentRate = summary.CurrentRate
		input.Summary5m = summary.Summary5m
		input.Summary15m = summary.Summary15m
		input.Summary1h = summary.Summary1h
		input.Summary6h = summary.Summary6h
		input.Summary24h = summary.Summary24h
		input.BotState = decisionBotState{
			CurrentPosition:    summary.BotState.CurrentPosition,
			OpenPositionsCount: summary.BotState.OpenPositionsCount,
		}
		input.EventContext = summary.EventContext
	}
	j, err := json.MarshalIndent(input, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal decision input: %w", err)
	}
	return j, nil
}

// BuildDecisionPayload is the PANEL stdin prompt: claude runs a 2-step sub-agent panel (market-regime
// then trade-decider). Retained for reversibility (config decision_single_agent:false). Pure (no I/O).
func BuildDecisionPayload(symbol string, summary *market.MarketSummary, playbook string) ([]byte, error) {
	j, err := decisionInputJSON(symbol, summary, playbook)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString("Decide whether to trade " + symbol + " right now using a 2-step agent panel, then output ONLY the final YAML decision.\n")
	b.WriteString("1. Use the market-regime subagent to classify the current regime (trend_up/trend_down/range/volatile/unclear) from the summary.\n")
	b.WriteString("2. Use the trade-decider subagent, giving it the classified regime + the playbook CHECKLIST + the summary, to ")
	b.WriteString("evaluate the playbook lanes strictly with the summary's numbers.\n")
	b.WriteString(decisionBias)
	b.WriteString("Output ONLY the trade-decider's YAML decision document (no prose, no fences).\n")
	b.WriteString(decisionOutputContract)
	b.WriteString("\nInput JSON:\n")
	b.Write(j)
	b.WriteString("\n")
	return b.Bytes(), nil
}

// BuildSingleAgentDecisionPayload is the SINGLE-AGENT stdin prompt (the default): ONE
// claude pass reasons regime→strategy→entry jointly, with NO sub-agent panel. Invoked with --tools
// Read (no Task), so claude cannot spawn sub-agents — killing the panel's orchestration fragility and
// letting regime + strategy be judged together (better on the borderline range/grind calls). Pure.
func BuildSingleAgentDecisionPayload(symbol string, summary *market.MarketSummary, playbook string) ([]byte, error) {
	j, err := decisionInputJSON(symbol, summary, playbook)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString("You are fx-bot's trade decider for " + symbol + ". Decide trade/no_trade RIGHT NOW in a SINGLE pass — do everything yourself, do NOT delegate or spawn any helper agent.\n")
	b.WriteString("Step 1 — Direction (24h leads): summary_24h.change_pips is the PRIMARY yardstick of what the market actually did over the last day (negative = it fell). summary_6h must AGREE with that direction for a lane to check. summary_1h/15m/5m are entry TIMING only — never derive direction from them, and never let a 6h counter-move against the 24h direction license a counter-trend entry (that is a known losing pattern).\n")
	b.WriteString("Step 2 — Lanes: walk the playbook checklist IN ORDER: 【HARD禁止】 first, then each lane. Evaluate every box with the summary's numbers (change_pips / range_position_pct / trend_direction / spread_pips). All boxes of a lane check → enter with that lane's TP/SL. Anything missing → no_trade.\n")
	b.WriteString("Step 3 — Arm: when a lane's 土俵 holds (the change_pips band AND the 6h agreement) but ONLY the timing box (再転換/再加速の起点) is not confirmed yet, do NOT settle for no_trade — ARM a conditional plan instead: pick the STRUCTURAL price level whose break would confirm that timing box (L1: break below the pullback low → SELL / L4: break above the consolidation high → BUY), within 30 pips of the current price, with the lane's TP/SL. Deterministic code fires it the instant price crosses, after re-validating every veto and the lane band on fresh data (journal stages: armed_fired / lane_recheck_failed / spike_veto ...); your next hourly decision replaces the plans wholesale. At most one plan per side. When even the 土俵 is absent, answer plain no_trade with NO arms.\n")
	b.WriteString("reason_jp MUST start with the tag: \"[L1] ...\" / \"[L4] ...\" on an immediate entry, \"[arm] ...\" when placing arms, or \"[no_trade:<最初に欠けた条件>] ...\" — then the NUMBERS you checked, 1-2 sentences.\n")
	b.WriteString(decisionBias)
	b.WriteString("Output ONLY this YAML (no prose, no fences):\n")
	b.WriteString(decisionSchema)
	b.WriteString(decisionOutputContract)
	b.WriteString("\nInput JSON:\n")
	b.Write(j)
	b.WriteString("\n")
	return b.Bytes(), nil
}

func (j *LLMDecisionCLI) noTrade() strategy.LLMTradeDecision {
	return strategy.LLMTradeDecision{Go: false, Side: "none"}
}

// strayChatterRE matches a trailing leftover/noop subagent's meta-chatter: the panel finished,
// the YAML was produced by a subagent, but claude -p returned this closing remark as the final
// stdout instead of the decision. Markers are deliberately specific so a real decision
// or an ordinary garbled attempt is not swept up.
var strayChatterRE = regexp.MustCompile(`(?i)already (been )?output|no further action|stray|no-?op|leftover|panel is complete|decision was already|nothing (further|more) to do`)

// isStrayPanelChatter reports output that carries NO decision document yet reads like a stray
// subagent's closing remark. Such output cannot be parsed into a decision (the decision isn't there)
// but, unlike genuine garbage, a fresh attempt usually yields the real YAML — so it is retryable.
func isStrayPanelChatter(out string) bool {
	if decisionKeyRE.MatchString(out) {
		return false // a real decision document is present — not stray chatter
	}
	return strayChatterRE.MatchString(out)
}

// Decide runs the subagent and returns the domain decision (fail-safe to no-trade).
//
// claude -p writes its FATAL reasons (usage/session limit, "API Error: 529 Overloaded",
// "Not logged in") to STDOUT and exits non-zero with an EMPTY stderr. Wrapping only stderr
// would surface every failure as a content-free "exit status 1 ()". So we classify on STDOUT
// exactly like ClaudeCLIAdvisor: surface the real reason, skip retry on usage limits, and retry
// transient infra blips to self-heal the cycle.
func (j *LLMDecisionCLI) Decide(ctx context.Context, symbol string, summary *market.MarketSummary, playbook string) (strategy.LLMTradeDecision, error) {
	build := BuildDecisionPayload
	if j.SingleAgent {
		build = BuildSingleAgentDecisionPayload
	}
	payload, err := build(symbol, summary, playbook)
	if err != nil {
		return j.noTrade(), err
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(j.TimeoutSeconds)*time.Second)
	defer cancel()

	attempts := j.TransientRetries + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-cctx.Done():
				return j.noTrade(), fmt.Errorf("trade-decider cli: timed out during transient retry backoff")
			case <-time.After(j.TransientRetryBackoff):
			}
		}

		cmd := j.ExecCommand(cctx, j.CLIPath, claudeDecisionArgs(j.SingleAgent)...)
		applyClaudeEnv(cmd)
		if j.WorkingDir != "" {
			cmd.Dir = j.WorkingDir
		}
		cmd.Stdin = bytes.NewReader(payload)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		rerr := cmd.Run()
		out := stdout.String()

		if cctx.Err() == context.DeadlineExceeded {
			return j.noTrade(), fmt.Errorf("trade-decider cli: timed out")
		}

		// Classify claude-level fatal messages on STDOUT first — they arrive on BOTH
		// exit 0 and non-zero exit, with stderr empty. classifyCLIOutput routes a
		// server-side rate limit ("(not your usage limit) · Rate limited") to the
		// TRANSIENT (retry) path, not the usage-cap give-up path.
		switch classifyCLIOutput(out) {
		case cliOutcomeUsageLimit:
			return j.noTrade(), fmt.Errorf("trade-decider cli: claude usage/session limit: %s", firstLine(out))
		case cliOutcomeTransient:
			lastErr = fmt.Errorf("trade-decider cli: claude transient API error: %s", firstLine(out))
			if attempt < attempts-1 {
				continue
			}
			return j.noTrade(), lastErr
		}

		if rerr != nil {
			var exitErr *exec.ExitError
			if errors.As(rerr, &exitErr) {
				// Surface STDOUT (where claude prints its real reason) plus stderr,
				// instead of a bare "exit status 1 ()".
				return j.noTrade(), fmt.Errorf("trade-decider cli: exit %d: %s",
					exitErr.ExitCode(), cliErrDetail(out, stderr.String()))
			}
			return j.noTrade(), fmt.Errorf("trade-decider cli: %w (%s)", rerr, stderr.String())
		}

		// Exit 0 but the output is a stray subagent's closing remark with NO decision document:
		// the real YAML was lost by the panel. A fresh attempt usually recovers it,
		// so RETRY within the cycle; only on exhaustion fail safe to a CLEARLY-labelled no-trade
		// (not the misleading "yaml_unmarshal_error", which implies a parser bug it is not).
		if isStrayPanelChatter(out) {
			if attempt < attempts-1 {
				lastErr = fmt.Errorf("trade-decider cli: stray panel chatter, no decision in output")
				if j.Logger != nil {
					j.Logger.Warn("trade_decider_stray_panel_retry", "symbol", symbol,
						"raw_stdout", truncate(strings.TrimSpace(out), 200))
				}
				continue
			}
			excerpt := truncate(strings.TrimSpace(out), 800)
			if j.Logger != nil {
				j.Logger.Warn("trade_decider_stray_panel_no_decision", "symbol", symbol, "raw_stdout", excerpt)
			}
			if j.Journal != nil {
				if rerr := j.Journal.Record(port.LLMDecisionLogEntry{
					Symbol: symbol, Event: "parse_fallback", Reason: "stray_panel_no_decision", RawStdout: excerpt,
				}); rerr != nil && j.Logger != nil {
					j.Logger.Warn("trade_decider_journal_record_failed", "symbol", symbol, "err", rerr)
				}
			}
			return strategy.LLMTradeDecision{Go: false, Side: "none", Reason: "stray_panel_no_decision"}, nil
		}

		// Exit 0: parse the decision. A garbled (non-limit/non-transient) document is a
		// SILENT no-trade — garbage = stay flat (the parser is the first safety wall).
		dec, perr := ParseLLMDecision(stdout.Bytes())
		if perr != nil {
			// Carry the raw stdout (truncated) so a malformation is diagnosable from the logs
			// alone — the go-yaml error by itself is a ~10-char snippet, not claude's actual output.
			excerpt := truncate(strings.TrimSpace(out), 800)
			if j.Logger != nil {
				j.Logger.Warn("trade_decider_parse_fallback_no_trade",
					"symbol", symbol, "err", perr, "raw_stdout", excerpt)
			}
			// Also persist it durably (slog is transient stdout). Time is stamped by the
			// journal; never fail the cycle on it.
			if j.Journal != nil {
				if rerr := j.Journal.Record(port.LLMDecisionLogEntry{
					Symbol: symbol, Event: "parse_fallback", Reason: perr.Error(), RawStdout: excerpt,
				}); rerr != nil && j.Logger != nil {
					j.Logger.Warn("trade_decider_journal_record_failed", "symbol", symbol, "err", rerr)
				}
			}
		}
		// A cleanly-parsed decision with NO reason is unauditable (the parse succeeded, so
		// otherwise no raw stdout is kept anywhere to diagnose the drop). Persist the raw stdout
		// durably, exactly like parse_fallback. Observability only: the decision itself still goes through.
		if perr == nil && strings.TrimSpace(dec.Reason) == "" {
			excerpt := truncate(strings.TrimSpace(out), 800)
			if j.Logger != nil {
				j.Logger.Warn("trade_decider_empty_reason", "symbol", symbol, "raw_stdout", excerpt)
			}
			if j.Journal != nil {
				if rerr := j.Journal.Record(port.LLMDecisionLogEntry{
					Symbol: symbol, Event: "empty_reason", Reason: "", RawStdout: excerpt,
				}); rerr != nil && j.Logger != nil {
					j.Logger.Warn("trade_decider_journal_record_failed", "symbol", symbol, "err", rerr)
				}
			}
		}
		return dec.ToDomain(), nil
	}
	if lastErr != nil {
		return j.noTrade(), lastErr
	}
	return j.noTrade(), nil
}

// cliErrDetail builds a human-readable failure detail, preferring claude's STDOUT
// message (its real reason) and appending stderr when present.
func cliErrDetail(stdout, stderr string) string {
	detail := firstLine(stdout)
	if se := strings.TrimSpace(stderr); se != "" {
		if detail != "" {
			detail += " | stderr: " + truncate(se, 200)
		} else {
			detail = truncate(se, 200)
		}
	}
	if detail == "" {
		detail = "(no output)"
	}
	return detail
}

// DecideFunc adapts Decide to command.LLMDecideFunc for a fixed symbol.
func (j *LLMDecisionCLI) DecideFunc(symbol string) func(ctx context.Context, summary *market.MarketSummary, playbook string) (strategy.LLMTradeDecision, error) {
	return func(ctx context.Context, summary *market.MarketSummary, playbook string) (strategy.LLMTradeDecision, error) {
		return j.Decide(ctx, symbol, summary, playbook)
	}
}
