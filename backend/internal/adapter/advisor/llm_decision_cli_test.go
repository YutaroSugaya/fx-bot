package advisor

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
)

// TestBuildDecisionPayload_ChecklistContract pins the decision rule:
// the playbook is a strict CHECKLIST — when every box of a lane checks the LLM MUST enter
// (anti-deadlock intent: no hesitation, small losses acceptable),
// and when any box fails it MUST answer no_trade. Softeners ("guidelines, not
// strict gates" / "roughly holds") let past-performance stories override live structure
// (e.g. counter-trend SELLs into a rising day) and must be absent from BOTH payload builders.
func TestBuildDecisionPayload_ChecklistContract(t *testing.T) {
	single, err := BuildSingleAgentDecisionPayload("USD_JPY", nil, "PB")
	if err != nil {
		t.Fatal(err)
	}
	panel, err := BuildDecisionPayload("USD_JPY", nil, "PB")
	if err != nil {
		t.Fatal(err)
	}
	for name, payload := range map[string]string{"single": string(single), "panel": string(panel)} {
		if strings.Contains(payload, "Default to no_trade unless") {
			t.Errorf("%s prompt must NOT default to no_trade — it deadlocks the loop", name)
		}
		low := strings.ToLower(payload)
		for _, banned := range []string{"guidelines", "roughly"} {
			if strings.Contains(low, banned) {
				t.Errorf("%s prompt must NOT soften the checklist with %q", name, banned)
			}
		}
		for _, want := range []string{"CHECKLIST", "MUST enter", "MUST answer no_trade", "small losses"} {
			if !strings.Contains(payload, want) {
				t.Errorf("%s prompt missing checklist phrase %q", name, want)
			}
		}
	}
}

// A "2-step agent panel" prompt can let claude end with a stray subagent's
// chatter ("the final decision was already output above … no further action needed") instead of
// the YAML, so `claude -p`'s final-message-only stdout lost the decision. The prompt must demand
// that the VERY LAST message be the YAML decision, repeated verbatim even if a subagent already
// produced it — so the captured stdout always contains the decision.
func TestBuildDecisionPayload_FinalMessageIsYAMLContract(t *testing.T) {
	payload, err := BuildDecisionPayload("USD_JPY", nil, "PB")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ToLower(string(payload))
	for _, want := range []string{"last message", "repeat", "no further action"} {
		if !strings.Contains(s, want) {
			t.Errorf("prompt missing final-message contract phrase %q (stray-agent chatter must be forbidden)", want)
		}
	}
}

// A stray-agent chatter stdout (no decision document) must be RETRIED within the cycle; the retry
// produces a real decision → the cycle self-heals with no error (mirrors the transient-retry path).
func TestLLMDecide_StrayPanelChatter_RetriesThenRecovers(t *testing.T) {
	stray := fakeExec("stray_chatter")
	success := fakeExec("decision_no_trade")
	var calls int
	d := newLLMDecider(t, "stray_chatter")
	d.ExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls++
		if calls == 1 {
			return stray(ctx, name, args...)
		}
		return success(ctx, name, args...)
	}
	dec, err := d.Decide(context.Background(), "USD_JPY", newSummary(), "pb")
	if err != nil {
		t.Fatalf("stray chatter must retry & recover, got %v", err)
	}
	if calls != 2 {
		t.Errorf("stray chatter must retry (2 calls), got %d", calls)
	}
	if dec.Go {
		t.Error("recovered decision_no_trade must be no-trade")
	}
}

// If every attempt is stray chatter, fail safe to no-trade with an HONEST reason (not the
// alarming "yaml_unmarshal_error", which falsely implies a parser bug) and journal the raw stdout.
func TestLLMDecide_StrayPanelChatter_AllFail_HonestNoTrade(t *testing.T) {
	d := newLLMDecider(t, "stray_chatter")
	j := &recordingJournal{}
	d.Journal = j
	dec, err := d.Decide(context.Background(), "USD_JPY", newSummary(), "pb")
	if err != nil {
		t.Fatalf("exhausted stray chatter must be a silent no-trade, got err %v", err)
	}
	if dec.Go {
		t.Fatal("stray chatter must fail safe to no-trade")
	}
	if strings.Contains(dec.Reason, "yaml_unmarshal_error") {
		t.Errorf("stray chatter must NOT be mislabelled yaml_unmarshal_error, got reason %q", dec.Reason)
	}
	if dec.Reason == "" {
		t.Errorf("stray chatter must carry an honest reason, got empty")
	}
	if len(j.entries) != 1 {
		t.Fatalf("want exactly one journal entry for the exhausted stray, got %d", len(j.entries))
	}
	if !strings.Contains(j.entries[0].RawStdout, "stray noop") {
		t.Errorf("journal must carry the raw stray stdout for diagnosis, got %q", j.entries[0].RawStdout)
	}
}

// The per-cycle decision can be collapsed to a SINGLE agent. The single-agent
// prompt must do regime+strategy+entry in ONE pass (no sub-agent panel) and must NOT instruct claude
// to spawn subagents, while preserving the aggressive-trade bias and the exact output schema.
func TestBuildSingleAgentDecisionPayload_NoPanel(t *testing.T) {
	payload, err := BuildSingleAgentDecisionPayload("USD_JPY", nil, "PB")
	if err != nil {
		t.Fatal(err)
	}
	s := string(payload)
	low := strings.ToLower(s)
	for _, banned := range []string{"subagent", "2-step", "panel"} {
		if strings.Contains(low, banned) {
			t.Errorf("single-agent prompt must NOT reference %q (it must do everything in one pass)", banned)
		}
	}
	for _, want := range []string{"MUST enter", "small losses", "decision:", "tp_pips", "reason_jp"} {
		if !strings.Contains(s, want) {
			t.Errorf("single-agent prompt missing required phrase %q", want)
		}
	}
}

// 24h-led hierarchy + lane tags. The prompt must (a) make summary_24h.change_pips
// the PRIMARY direction yardstick — a "never fight the 6h trend" rule lets an intraday
// pullback flip the 6h sign and license counter-trend SELLs into a rising day —
// (b) require the machine-parsable lane tag at the head of reason_jp so per-lane attribution is
// automatic, and (c) keep 【HARD禁止】 strict (code re-vetoes downstream) WITHOUT duplicating the
// numeric discipline in the prompt (single source of truth = the playbook; the duplicated
// "pullback BUY requires ≤0.50" line must not appear either).
func TestBuildSingleAgentDecisionPayload_V8HierarchyAndLaneTags(t *testing.T) {
	payload, err := BuildSingleAgentDecisionPayload("USD_JPY", nil, "PB")
	if err != nil {
		t.Fatal(err)
	}
	s := string(payload)
	if !strings.Contains(s, "【HARD禁止】") {
		t.Errorf("prompt must keep 【HARD禁止】 rules strict (code vetoes them downstream)")
	}
	for _, banned := range []string{"fights the 6h trend", "range_position_pct ≤ 0.50", "not by itself a reason to skip"} {
		if strings.Contains(s, banned) {
			t.Errorf("prompt must NOT contain the retired rule %q (6h-led hierarchy / duplicated discipline)", banned)
		}
	}
	for _, want := range []string{"summary_24h.change_pips", "24h", "[L1]", "[L4]", "[no_trade:", "night_buy_veto", "exhaustion_veto", "spike_veto"} {
		if !strings.Contains(s, want) {
			t.Errorf("single-agent prompt missing %q", want)
		}
	}
	// Arms: the prompt must teach WHEN to arm (土俵のみ成立), the schema (arms list with
	// break_above/break_below), the per-side cap, and the [arm] reason tag.
	for _, want := range []string{"Step 3 — Arm", "arms:", "break_above", "break_below", "[arm]", "one plan per side"} {
		if !strings.Contains(s, want) {
			t.Errorf("single-agent prompt missing arm marker %q", want)
		}
	}
}

// Payload trim (不要なコンテキストを渡さない). The decision input carries
// ONLY what the lane checklist needs: the TF windows (incl. change_pips), current rate/spread,
// a trimmed bot state (position slot + open count), the JST clock, the event context and the
// playbook. The advisor-era extras — allowed
// strategies, the LLM's own recent decisions (anchoring), rejections, hard-limits geometry,
// daily P&L / consecutive losses (code-enforced, invite mood-based drift) — must be GONE.
func TestDecisionInputJSON_TrimmedPayload(t *testing.T) {
	pos := "BUY"
	full := &market.MarketSummary{
		Symbol:        "USD_JPY",
		Time:          time.Date(2026, 7, 9, 6, 30, 0, 0, time.UTC), // = 15:30 JST
		NextValidFrom: time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC),
		CurrentRate:   market.CurrentRate{Bid: 162.0, Ask: 162.005, SpreadPips: 0.5},
		Summary24h:    market.WindowSummary{ChangePips: -42.5, RangePositionPct: 0.31, NumCandles: 1440},
		BotState: market.BotState{
			Mode: "live_config", EmergencyStop: true, CurrentPosition: &pos,
			OpenPositionsCount: 1, DailyPnLJPY: -1234, ConsecutiveLosses: 3,
			TradesToday: 2, TradesInCurrentWindow: 1,
		},
		RecentTrades:      []market.RecentTrade{{Side: "SELL", ProfitLossPips: -16.1, ProfitLossJPY: -1610, CloseReason: "ratchet_stoploss"}},
		RecentRejections:  []market.RecentRejection{{Reason: "spread", Count: 2}},
		HardLimits:        &market.HardLimitsForJSON{MaxDailyLossJPY: 20000},
		AllowedStrategies: []string{"momentum_pullback"},
		RecentDecisions:   []market.RecentDecision{{RegimeType: "range", StrategyName: "no_trade", AgeMinutes: 30}},
		EventContext:      &market.EventContext{Name: "FOMC", Policy: "freeze", MinutesUntil: 45},
	}
	j, err := decisionInputJSON("USD_JPY", full, "PB-TEXT")
	if err != nil {
		t.Fatal(err)
	}
	s := string(j)
	for _, banned := range []string{
		"allowed_strategies", "momentum_pullback", "recent_decisions", "recent_rejections",
		"hard_limits", "next_valid_from", "daily_pnl_jpy", "consecutive_losses",
		"emergency_stop", "trades_in_current_window", `"mode"`, `"recent_trades"`,
		// trades_today uses a DIFFERENT day definition (0:00 JST, all-symbol) than
		// today_closed_trades — contradictory evidence.
		"trades_today",
		// Neither a 同日2敗 stop nor an L4 once-per-day rule is in the checklist,
		// so no checklist box consumes today's trade history — sending
		// it would only invite mood-based self-throttling. The cycle still injects it into
		// summary.RecentTrades for the dormant daily_loss_stop mechanism.
		"today_closed_trades",
	} {
		if strings.Contains(s, banned) {
			t.Errorf("decision input must NOT carry %q", banned)
		}
	}
	for _, want := range []string{
		"change_pips", "summary_24h", "current_rate", "PB-TEXT",
		"open_positions_count", "current_position", "FOMC",
		// The JST clock the playbook's time-of-day boxes (JST 0-5時台)
		// are judged against — 15:30 JST for the fixture's 06:30 UTC.
		`"time_jst": "2026-07-09 15:30 JST"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("decision input missing %q", want)
		}
	}
	// nil summary (first boot / tests) must stay marshal-safe.
	if _, err := decisionInputJSON("USD_JPY", nil, "PB"); err != nil {
		t.Fatalf("nil summary must not error: %v", err)
	}
}

// Single-agent mode must be invoked WITHOUT the Task tool so claude CANNOT spawn subagents — this
// deterministically removes the stray-panel class of bugs. Panel mode keeps Task.
func TestClaudeDecisionArgs_SingleAgentOmitsTask(t *testing.T) {
	single := strings.Join(claudeDecisionArgs(true), " ")
	if strings.Contains(single, "Task") {
		t.Errorf("single-agent args must omit Task (no subagent spawning), got %q", single)
	}
	if !strings.Contains(single, "Read") {
		t.Errorf("single-agent args must still allow Read, got %q", single)
	}
	panel := strings.Join(claudeDecisionArgs(false), " ")
	if !strings.Contains(panel, "Task") {
		t.Errorf("panel args must keep Task (subagent panel), got %q", panel)
	}
}

// End-to-end: a SingleAgent decider invokes claude with --tools Read (no Task) and the single-agent
// prompt; a panel decider keeps Task. Captures the actual exec args.
func TestLLMDecide_SingleAgent_InvokesWithoutTask(t *testing.T) {
	var gotArgs []string
	base := fakeExec("decision_no_trade")
	d := newLLMDecider(t, "decision_no_trade")
	d.SingleAgent = true
	d.ExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotArgs = append([]string{}, args...)
		return base(ctx, name, args...)
	}
	if _, err := d.Decide(context.Background(), "USD_JPY", newSummary(), "pb"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	joined := strings.Join(gotArgs, " ")
	if strings.Contains(joined, "Task") {
		t.Errorf("single-agent Decide must invoke claude WITHOUT Task, got args %q", joined)
	}
}

// recordingJournal captures entries the decider writes, for assertions.
type recordingJournal struct{ entries []port.LLMDecisionLogEntry }

func (r *recordingJournal) Record(e port.LLMDecisionLogEntry) error {
	r.entries = append(r.entries, e)
	return nil
}

// newLLMDecider builds an LLMDecisionCLI wired to the helper-process fake.
func newLLMDecider(t *testing.T, mode string) *LLMDecisionCLI {
	t.Helper()
	return &LLMDecisionCLI{
		CLIPath:               "/usr/local/bin/claude", // ignored by fakeExec
		WorkingDir:            "",
		TimeoutSeconds:        3,
		Logger:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		ExecCommand:           fakeExec(mode),
		TransientRetries:      1,
		TransientRetryBackoff: time.Millisecond,
	}
}

// Failure mode: every pair shows "trade-decider cli: exit status 1 ()" on the dashboard.
// claude writes its fatal reason to STDOUT and exits non-zero with EMPTY stderr, so
// wrapping only stderr yields a content-free error. The returned
// error MUST surface the stdout reason.
func TestLLMDecide_GenericNonZeroExit_SurfacesStdoutReason(t *testing.T) {
	d := newLLMDecider(t, "generic_exit1_stdout")
	dec, err := d.Decide(context.Background(), "USD_JPY", newSummary(), "playbook")
	if err == nil {
		t.Fatal("non-zero exit must return a transport error, got nil")
	}
	if !strings.Contains(err.Error(), "Not logged in") {
		t.Errorf("error must surface stdout reason, got %q (must not be a bare 'exit status 1 ()')", err.Error())
	}
	if dec.Go {
		t.Error("must fail safe to no-trade")
	}
}

// usage/session limit on a non-zero exit: surface the limit, and do NOT retry
// (it resets hours later — retrying just hammers the exhausted quota).
func TestLLMDecide_UsageLimit_NonZeroExit_SurfacedNoRetry(t *testing.T) {
	var calls int
	base := fakeExec("usage_limit_exit1")
	d := newLLMDecider(t, "usage_limit_exit1")
	d.ExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls++
		return base(ctx, name, args...)
	}
	dec, err := d.Decide(context.Background(), "USD_JPY", newSummary(), "pb")
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("usage limit must be surfaced, got err=%v", err)
	}
	if calls != 1 {
		t.Errorf("usage limit must NOT retry, got %d calls", calls)
	}
	if dec.Go {
		t.Error("must fail safe to no-trade")
	}
}

// usage/session limit can also arrive on exit 0 (printed to stdout). It must be
// surfaced as an error, not silently swallowed as a no-trade.
func TestLLMDecide_UsageLimit_Exit0_Surfaced(t *testing.T) {
	d := newLLMDecider(t, "usage_limit")
	_, err := d.Decide(context.Background(), "USD_JPY", newSummary(), "pb")
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("exit-0 usage limit must be surfaced as an error, got %v", err)
	}
}

// A transient infra error (529/overload) on a non-zero exit is retried; the retry
// succeeds → the cycle is saved with no error.
func TestLLMDecide_TransientError_RetriesThenSucceeds(t *testing.T) {
	transient := fakeExec("transient_exit1")
	success := fakeExec("decision_no_trade")
	var calls int
	d := newLLMDecider(t, "transient_exit1")
	d.ExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls++
		if calls == 1 {
			return transient(ctx, name, args...)
		}
		return success(ctx, name, args...)
	}
	dec, err := d.Decide(context.Background(), "USD_JPY", newSummary(), "pb")
	if err != nil {
		t.Fatalf("transient retry should succeed, got %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 1 retry (2 calls), got %d", calls)
	}
	if dec.Go {
		t.Error("decision_no_trade should map to no-trade")
	}
}

// If every transient attempt fails, surface a transient cli error (retryable class).
func TestLLMDecide_TransientError_AllFail_Surfaced(t *testing.T) {
	var calls int
	base := fakeExec("transient_exit1")
	d := newLLMDecider(t, "transient_exit1")
	d.ExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls++
		return base(ctx, name, args...)
	}
	_, err := d.Decide(context.Background(), "USD_JPY", newSummary(), "pb")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "api") {
		t.Fatalf("exhausted transient must surface, got %v", err)
	}
	if calls != 2 {
		t.Errorf("expected retry (2 calls), got %d", calls)
	}
}

// A SERVER-SIDE rate limit ("Server is temporarily limiting requests (not your usage
// limit) · Rate limited") must not be misclassified as a usage/session cap (the message
// contains the substring "usage limit") and given up on with NO retry → every pair would
// show "エラー". It is TRANSIENT and must be retried; the retry self-heals the cycle.
func TestLLMDecide_ServerRateLimit_RetriesThenRecovers(t *testing.T) {
	rl := fakeExec("rate_limit_exit1")
	success := fakeExec("decision_no_trade")
	var calls int
	d := newLLMDecider(t, "rate_limit_exit1")
	d.ExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls++
		if calls == 1 {
			return rl(ctx, name, args...)
		}
		return success(ctx, name, args...)
	}
	dec, err := d.Decide(context.Background(), "USD_JPY", newSummary(), "pb")
	if err != nil {
		t.Fatalf("server rate-limit must retry & recover, got %v", err)
	}
	if calls != 2 {
		t.Errorf("server rate-limit must retry (2 calls), got %d", calls)
	}
	if dec.Go {
		t.Error("recovered decision_no_trade must be no-trade")
	}
}

// Even when every attempt is rate-limited, it must surface as a TRANSIENT (retryable)
// error — never as a usage/session cap (which would wrongly imply a multi-hour outage).
func TestLLMDecide_ServerRateLimit_AllFail_NotTreatedAsUsageCap(t *testing.T) {
	var calls int
	base := fakeExec("rate_limit_exit1")
	d := newLLMDecider(t, "rate_limit_exit1")
	d.ExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls++
		return base(ctx, name, args...)
	}
	_, err := d.Decide(context.Background(), "USD_JPY", newSummary(), "pb")
	if err == nil {
		t.Fatal("exhausted rate-limit must surface an error")
	}
	if strings.Contains(strings.ToLower(err.Error()), "usage/session limit") {
		t.Errorf("server rate-limit must NOT be classified as a usage cap, got %q", err.Error())
	}
	if calls != 2 {
		t.Errorf("server rate-limit must retry (2 calls), got %d", calls)
	}
}

// Contract preserved: a VALID run whose decision YAML is merely garbled (not a
// limit/transient signal) stays a silent no-trade — garbage = stay flat, no error.
func TestLLMDecide_GarbledOutput_IsSilentNoTrade(t *testing.T) {
	d := newLLMDecider(t, "non_yaml_prose")
	dec, err := d.Decide(context.Background(), "USD_JPY", newSummary(), "pb")
	if err != nil {
		t.Fatalf("garbled (non-limit) output must NOT error, got %v", err)
	}
	if dec.Go {
		t.Error("garbled output must fail safe to no-trade")
	}
}

// Happy path: a valid decision YAML on exit 0 parses with no error.
func TestLLMDecide_ValidDecision_NoError(t *testing.T) {
	d := newLLMDecider(t, "decision_no_trade")
	dec, err := d.Decide(context.Background(), "USD_JPY", newSummary(), "pb")
	if err != nil {
		t.Fatalf("valid decision must not error, got %v", err)
	}
	if dec.Go {
		t.Error("decision_no_trade is a no-trade")
	}
}

// If the parse-fallback log records only go-yaml's error (a ~10-char snippet) and never
// claude's raw stdout, a loop stuck at "yaml_unmarshal_error" is undiagnosable. The fallback
// log MUST carry the raw stdout so a malformation is diagnosable from the logs alone.
func TestLLMDecide_ParseFailure_LogsRawStdout(t *testing.T) {
	var buf bytes.Buffer
	d := newLLMDecider(t, "garbled_unparseable")
	d.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	dec, err := d.Decide(context.Background(), "USD_JPY", newSummary(), "pb")
	if err != nil {
		t.Fatalf("garbled (non-limit) output must NOT error, got %v", err)
	}
	if dec.Go {
		t.Error("garbled output must fail safe to no-trade")
	}
	logged := buf.String()
	if !strings.Contains(logged, "trade_decider_parse_fallback_no_trade") {
		t.Fatalf("expected parse-fallback log event, got %q", logged)
	}
	if !strings.Contains(logged, "RAWDUMP_TAIL_MARKER") {
		t.Errorf("parse-fallback log MUST carry the raw stdout for diagnosis, got %q", logged)
	}
}

// Detecting a parse failure from the reason code is not enough — the raw claude stdout (the thing
// slog alone loses, because it goes only to a transient stdout) must be persisted DURABLY. On a parse-fallback Decide records a parse_fallback entry
// carrying the raw stdout to the append-only journal.
func TestLLMDecide_ParseFailure_RecordsRawStdoutToJournal(t *testing.T) {
	d := newLLMDecider(t, "garbled_unparseable")
	j := &recordingJournal{}
	d.Journal = j
	dec, err := d.Decide(context.Background(), "USD_JPY", newSummary(), "pb")
	if err != nil {
		t.Fatalf("garbled (non-limit) output must NOT error, got %v", err)
	}
	if dec.Go {
		t.Error("garbled output must fail safe to no-trade")
	}
	if len(j.entries) != 1 {
		t.Fatalf("want exactly one parse_fallback journal entry, got %d", len(j.entries))
	}
	e := j.entries[0]
	if e.Event != "parse_fallback" || e.Symbol != "USD_JPY" {
		t.Errorf("entry meta wrong: %+v", e)
	}
	if !strings.Contains(e.RawStdout, "RAWDUMP_TAIL_MARKER") {
		t.Errorf("parse_fallback entry MUST carry the raw stdout, got %q", e.RawStdout)
	}
}

// A submitted trade can carry an EMPTY reason and — because the parse SUCCEEDED — the raw claude
// stdout would never be persisted, leaving the drop undiagnosable after the fact. When a cleanly-parsed decision has no reason, Decide
// must durably record the raw stdout (event empty_reason), mirroring the parse_fallback doctrine:
// every unauditable decision leaves enough evidence to diagnose from the logs alone.
func TestLLMDecide_EmptyReason_RecordsRawStdoutToJournal(t *testing.T) {
	d := newLLMDecider(t, "decision_go_no_reason")
	j := &recordingJournal{}
	d.Journal = j
	dec, err := d.Decide(context.Background(), "GBP_USD", newSummary(), "pb")
	if err != nil {
		t.Fatalf("a parseable decision must not error, got %v", err)
	}
	if !dec.Go {
		t.Fatal("the decision itself must survive (observability must not veto the trade)")
	}
	if len(j.entries) != 1 {
		t.Fatalf("want exactly one empty_reason journal entry, got %d", len(j.entries))
	}
	e := j.entries[0]
	if e.Event != "empty_reason" || e.Symbol != "GBP_USD" {
		t.Errorf("entry meta wrong: %+v", e)
	}
	if !strings.Contains(e.RawStdout, "sl_pips") {
		t.Errorf("empty_reason entry MUST carry the raw stdout for diagnosis, got %q", e.RawStdout)
	}
}

// A valid (parseable) decision must NOT write a parse_fallback entry — journal noise stays
// proportional to actual failures (the per-cycle outcome is journaled upstream by the cycle).
func TestLLMDecide_ValidDecision_NoParseFallbackJournal(t *testing.T) {
	d := newLLMDecider(t, "decision_no_trade")
	j := &recordingJournal{}
	d.Journal = j
	if _, err := d.Decide(context.Background(), "USD_JPY", newSummary(), "pb"); err != nil {
		t.Fatalf("valid decision must not error, got %v", err)
	}
	if len(j.entries) != 0 {
		t.Errorf("valid decision must not write a parse_fallback entry, got %d", len(j.entries))
	}
}
