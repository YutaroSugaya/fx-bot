package advisor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
)

// TestHelperProcess is the child process re-executed by ExecCommandFunc fakes.
// It selects behaviour based on TEST_ADVISOR_MODE env var.
//
//nolint:revive
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	mode := os.Getenv("TEST_ADVISOR_MODE")
	// Read stdin to mirror real invocation (some modes echo it).
	stdinBytes, _ := io.ReadAll(os.Stdin)
	_ = stdinBytes
	switch mode {
	case "success_clean":
		fmt.Print(`config_id: "x"
generated_at: "2026-05-15T10:00:00+09:00"
valid_from: "2026-05-15T10:00:00+09:00"
valid_until: "2026-05-15T11:00:00+09:00"
symbol: USD_JPY
enabled: true
strategy:
  name: momentum_pullback
`)
		os.Exit(0)
	case "success_fenced":
		fmt.Print("```yaml\nconfig_id: \"x\"\nsymbol: USD_JPY\n```\n")
		os.Exit(0)
	case "non_yaml_prose":
		fmt.Print("Green 確認。\n\nYAML config 自体は前ターンで出力済みです。\n")
		os.Exit(0)
	case "empty_stdout":
		os.Exit(0)
	case "transient_api_error":
		// claude CLI prints infra errors to STDOUT with exit 0 (.fail.yaml-confirmed).
		fmt.Print("API Error: 529 Overloaded. This is a server-side issue, usually temporary — try again in a moment.\n")
		os.Exit(0)
	case "usage_limit":
		fmt.Print("You've hit your session limit · resets 4:20am (Asia/Tokyo)\n")
		os.Exit(0)
	case "usage_limit_exit1":
		// Real CLI shape: the CLI emits the session-limit message on
		// STDOUT but exits NON-ZERO. The exit-0 usage_limit case above does not
		// reproduce it; this one does.
		fmt.Print("You've hit your session limit · resets 7:30pm (Asia/Tokyo)\n")
		os.Exit(1)
	case "transient_exit1":
		// Infra blip (529/connection) that surfaces with a non-zero exit.
		fmt.Print("API Error: 529 Overloaded. Try again in a moment.\n")
		os.Exit(1)
	case "rate_limit_exit1":
		// Real CLI signature: a SERVER-SIDE rate limit (HTTP 429). The
		// message says "(not your usage limit)" to disambiguate from a usage cap,
		// yet a naive usageLimitRE matches the substring "usage limit" → no retry.
		// This is TRANSIENT and must be retried.
		fmt.Print("API Error: Server is temporarily limiting requests (not your usage limit) · Rate limited\n")
		os.Exit(1)
	case "decision_no_trade":
		// A valid trade-decider no-trade decision (parseable by ParseLLMDecision).
		fmt.Print("decision:\n  go: false\nreason_jp: \"test no trade\"\n")
		os.Exit(0)
	case "decision_go_no_reason":
		// Observed shape: a cleanly-parseable go:true decision with NO reason anywhere.
		// The parse succeeds, so only the empty_reason journal path can preserve the evidence.
		fmt.Print("decision:\n  go: true\n  side: SELL\n  entry: 1.32782\n  tp_pips: 30\n  sl_pips: 25\n")
		os.Exit(0)
	case "stray_chatter":
		// When the prompt asks for a 2-step agent PANEL, claude can spawn subagents and
		// `claude -p` can return a TRAILING leftover/noop subagent's chatter as the final
		// stdout — the real decision YAML is lost. The
		// captured stdout holds NO decision document, only this meta-chatter. Must be
		// retried (self-heal), and on exhaustion fail safe to a clearly-labelled no-trade.
		fmt.Print("The panel is complete; the final decision was already output above. That last notification is just a stray noop wait agent. No further action needed.\n")
		os.Exit(0)
	case "garbled_unparseable":
		// Genuinely non-YAML output (exit 0) → ParseLLMDecision returns a yaml error
		// (perr != nil), exercising Decide's raw-stdout diagnostic log. The tail marker is
		// far enough into the output that go-yaml's short error snippet cannot contain it,
		// so seeing it in the log PROVES the full raw stdout is logged.
		fmt.Print("これは判断ではありません。\nRAWDUMP_TAIL_MARKER\n")
		os.Exit(0)
	case "generic_exit1_stdout":
		// Real CLI signature: claude prints its fatal reason to STDOUT and
		// exits NON-ZERO with EMPTY stderr (verified: "Not logged in · Please run
		// /login"). The decider must surface this stdout, not a bare "exit 1 ()".
		fmt.Print("Not logged in · Please run /login\n")
		os.Exit(1)
	case "exit_nonzero":
		fmt.Fprintln(os.Stderr, "boom: signature invalid")
		os.Exit(2)
	case "timeout":
		time.Sleep(5 * time.Second)
		os.Exit(0)
	default:
		os.Exit(0)
	}
}

// fakeExec returns an ExecCommandFunc that re-runs the test binary in
// helper-process mode with the given environment var.
func fakeExec(mode string) ExecCommandFunc {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], cs...)
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "TEST_ADVISOR_MODE="+mode)
		return cmd
	}
}

// writePrompt creates a small prompt file in t.TempDir and returns its path.
func writePrompt(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	p := filepath.Join(d, "prompt.md")
	if err := os.WriteFile(p, []byte("# test prompt\nproduce yaml.\n"), 0o600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	return p
}

// stagingDirs creates temp dirs for the output archive + next.yaml staging.
func stagingDirs(t *testing.T) (outDir, nextPath string) {
	t.Helper()
	d := t.TempDir()
	outDir = filepath.Join(d, "ai_output")
	nextPath = filepath.Join(d, "configs", "strategy_config.next.yaml")
	return
}

func newSummary() *market.MarketSummary {
	return &market.MarketSummary{
		Symbol: "USD_JPY",
		Time:   time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC),
	}
}

func newAdvisor(t *testing.T, mode string) *ClaudeCLIAdvisor {
	t.Helper()
	out, next := stagingDirs(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &ClaudeCLIAdvisor{
		CLIPath:        "/usr/local/bin/claude", // path is just a string; fakeExec ignores it
		PromptPath:     writePrompt(t),
		OutputDir:      out,
		NextYAMLPath:   next,
		TimeoutSeconds: 3,
		Logger:         logger,
		Clock:          func() time.Time { return time.Now().UTC() },
		IDGen:          func() string { return "test-run-id" },
		ExecCommand:    fakeExec(mode),
	}
	return a
}

func TestGenerate_Success_CleanYAML(t *testing.T) {
	a := newAdvisor(t, "success_clean")
	run, err := a.Generate(context.Background(), newSummary())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if run.Status != port.AdvisorRunStatusSuccess {
		t.Errorf("status: %s err=%s", run.Status, run.ErrorMsg)
	}
	if !strings.Contains(string(run.ParsedYAML), "config_id") {
		t.Errorf("parsedYAML missing config_id: %s", run.ParsedYAML)
	}
	// next.yaml should be staged
	body, err := os.ReadFile(a.NextYAMLPath)
	if err != nil {
		t.Fatalf("read next.yaml: %v", err)
	}
	if !strings.Contains(string(body), "config_id") {
		t.Errorf("next.yaml content: %s", body)
	}
}

// claude CLI が API/接続/過負荷エラーを exit 0 で STDOUT に吐くと parse_error
// 扱いされ、その cycle の判断が丸ごと捨てられる。一時障害として cli_error に
// 分類し、1 回リトライして自己回復する。
func TestGenerate_TransientAPIError_RetriesThenSucceeds(t *testing.T) {
	a := newAdvisor(t, "success_clean")
	a.TransientRetries = 1
	a.TransientRetryBackoff = time.Millisecond
	transient := fakeExec("transient_api_error")
	success := fakeExec("success_clean")
	var calls int
	a.ExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls++
		if calls == 1 {
			return transient(ctx, name, args...)
		}
		return success(ctx, name, args...)
	}
	run, err := a.Generate(context.Background(), newSummary())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 exec calls (1 retry), got %d", calls)
	}
	if run.Status != port.AdvisorRunStatusSuccess {
		t.Errorf("expected success after transient retry, got %s (%s)", run.Status, run.ErrorMsg)
	}
}

func TestGenerate_TransientAPIError_AllAttemptsFail_IsCLIErrorNotParseError(t *testing.T) {
	a := newAdvisor(t, "transient_api_error")
	a.TransientRetries = 1
	a.TransientRetryBackoff = time.Millisecond
	var calls int
	base := fakeExec("transient_api_error")
	a.ExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls++
		return base(ctx, name, args...)
	}
	run, err := a.Generate(context.Background(), newSummary())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if run.Status != port.AdvisorRunStatusCLIError {
		t.Errorf("transient API error must be cli_error (not parse_error), got %s", run.Status)
	}
	if calls != 2 {
		t.Errorf("expected retry on transient error (2 calls), got %d", calls)
	}
}

func TestGenerate_UsageLimit_IsCLIError_NoRetry(t *testing.T) {
	a := newAdvisor(t, "usage_limit")
	a.TransientRetries = 1
	a.TransientRetryBackoff = time.Millisecond
	var calls int
	base := fakeExec("usage_limit")
	a.ExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls++
		return base(ctx, name, args...)
	}
	run, err := a.Generate(context.Background(), newSummary())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if run.Status != port.AdvisorRunStatusCLIError {
		t.Errorf("usage/session limit must be cli_error, got %s", run.Status)
	}
	if calls != 1 {
		t.Errorf("usage limit must NOT retry (resets hours later), got %d calls", calls)
	}
	if !run.UsageLimited {
		t.Error("exit-0 usage limit must set run.UsageLimited so the scheduler skips fast-retry")
	}
}

// Real session-limit failures exit NON-ZERO with the limit message on STDOUT.
// If the ExitError branch fires first it returns a bare "claude exit 1:" (empty
// stderr), masking the cause AND leaving UsageLimited unset → the 10-min
// fast-retry loop hammers the exhausted quota.
func TestGenerate_UsageLimit_NonZeroExit_ClassifiedNotMasked(t *testing.T) {
	a := newAdvisor(t, "usage_limit_exit1")
	a.TransientRetries = 1
	a.TransientRetryBackoff = time.Millisecond
	var calls int
	base := fakeExec("usage_limit_exit1")
	a.ExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls++
		return base(ctx, name, args...)
	}
	run, err := a.Generate(context.Background(), newSummary())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if run.Status != port.AdvisorRunStatusCLIError {
		t.Errorf("status = %s, want cli_error", run.Status)
	}
	if !run.UsageLimited {
		t.Error("non-zero-exit usage limit must set run.UsageLimited (drives no-fast-retry)")
	}
	if !strings.Contains(run.ErrorMsg, "limit") {
		t.Errorf("ErrorMsg must surface the limit, got %q (must not be a bare 'claude exit 1:')", run.ErrorMsg)
	}
	if calls != 1 {
		t.Errorf("usage limit must NOT retry, got %d calls", calls)
	}
}

// A transient infra error on a non-zero exit should still be classified as a
// (retryable) transient cli_error, mirroring the exit-0 transient path.
func TestGenerate_TransientError_NonZeroExit_RetriesAndClassified(t *testing.T) {
	a := newAdvisor(t, "transient_exit1")
	a.TransientRetries = 1
	a.TransientRetryBackoff = time.Millisecond
	var calls int
	base := fakeExec("transient_exit1")
	a.ExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls++
		return base(ctx, name, args...)
	}
	run, err := a.Generate(context.Background(), newSummary())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if run.Status != port.AdvisorRunStatusCLIError {
		t.Errorf("status = %s, want cli_error", run.Status)
	}
	if run.UsageLimited {
		t.Error("transient error must NOT be flagged UsageLimited")
	}
	if calls != 2 {
		t.Errorf("non-zero-exit transient must retry once (2 calls), got %d", calls)
	}
}

func TestGenerate_EnablesReadAndTaskTools(t *testing.T) {
	a := newAdvisor(t, "success_clean")
	var gotArgs []string
	baseExec := fakeExec("success_clean")
	a.ExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotArgs = append([]string(nil), args...)
		return baseExec(ctx, name, args...)
	}

	if _, err := a.Generate(context.Background(), newSummary()); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "--no-session-persistence") {
		t.Fatalf("claude args missing --no-session-persistence: %v", gotArgs)
	}
	// Read は skill ファイル参照に、Task は 4 並列 subagent (risk-auditor /
	// regime-classifier / strategy-selector / tpsl-designer) 起動に必要。
	// この 2 ツールだけを許可し、他 (Bash, Edit, Write, WebFetch 等) は遮断する。
	if !strings.Contains(joined, "--tools Read,Task") {
		t.Fatalf("claude args must allow exactly Read,Task tools: %v", gotArgs)
	}
	// Pin the model + reasoning effort (subagents inherit them).
	if !strings.Contains(joined, "--model opus") || !strings.Contains(joined, "--effort max") {
		t.Fatalf("claude args must pin latest opus at max effort: %v", gotArgs)
	}
}

func TestGenerate_Success_FencedYAML_IsStripped(t *testing.T) {
	a := newAdvisor(t, "success_fenced")
	run, err := a.Generate(context.Background(), newSummary())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if run.Status != port.AdvisorRunStatusSuccess {
		t.Errorf("status: %s err=%s", run.Status, run.ErrorMsg)
	}
	if strings.Contains(string(run.ParsedYAML), "```") {
		t.Errorf("fence not stripped: %s", run.ParsedYAML)
	}
}

func TestGenerate_NonConfigOutput_IsParseErrorAndDoesNotStageNextYAML(t *testing.T) {
	a := newAdvisor(t, "non_yaml_prose")
	run, err := a.Generate(context.Background(), newSummary())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if run.Status != port.AdvisorRunStatusParseError {
		t.Fatalf("status: got %s want %s", run.Status, port.AdvisorRunStatusParseError)
	}
	if !strings.Contains(run.ErrorMsg, "missing config_id") {
		t.Fatalf("error msg should mention missing config_id: %q", run.ErrorMsg)
	}
	if _, err := os.Stat(a.NextYAMLPath); !os.IsNotExist(err) {
		t.Fatalf("next.yaml must not be staged for non-config output; stat err=%v", err)
	}
	entries, err := os.ReadDir(a.OutputDir)
	if err != nil {
		t.Fatalf("raw output should be archived: %v", err)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".fail.yaml") {
		t.Fatalf("expected one fail archive, got %+v", entries)
	}
}

func TestGenerate_EmptyStdout_IsCLIError(t *testing.T) {
	a := newAdvisor(t, "empty_stdout")
	run, _ := a.Generate(context.Background(), newSummary())
	if run.Status != port.AdvisorRunStatusCLIError {
		t.Errorf("status: %s", run.Status)
	}
	if !strings.Contains(run.ErrorMsg, "empty") {
		t.Errorf("err msg: %s", run.ErrorMsg)
	}
}

func TestGenerate_NonZeroExit_IsCLIError(t *testing.T) {
	a := newAdvisor(t, "exit_nonzero")
	run, _ := a.Generate(context.Background(), newSummary())
	if run.Status != port.AdvisorRunStatusCLIError {
		t.Errorf("status: %s", run.Status)
	}
	if !strings.Contains(run.ErrorMsg, "exit") {
		t.Errorf("err msg should mention exit: %s", run.ErrorMsg)
	}
}

func TestGenerate_Timeout_IsTimeout(t *testing.T) {
	a := newAdvisor(t, "timeout")
	a.TimeoutSeconds = 1 // less than the 5s sleep in helper
	run, _ := a.Generate(context.Background(), newSummary())
	if run.Status != port.AdvisorRunStatusTimeout {
		t.Errorf("status: %s err=%s", run.Status, run.ErrorMsg)
	}
}

// fence 剥がしは ResponseParser.Parse() が担い、テストは
// prompt_parser_test.go の TestResponseParser_* にある
// (config_id を含まない YAML は upstream で reject する)。

func TestGenerate_MissingPrompt(t *testing.T) {
	a := newAdvisor(t, "success_clean")
	a.PromptPath = "/path/that/does/not/exist.md"
	_, err := a.Generate(context.Background(), newSummary())
	if err == nil || !strings.Contains(err.Error(), "read prompt") {
		t.Fatalf("expected prompt err, got %v", err)
	}
}

func TestGenerate_EmptyCLIPath(t *testing.T) {
	a := newAdvisor(t, "success_clean")
	a.CLIPath = ""
	_, err := a.Generate(context.Background(), newSummary())
	if err == nil || !strings.Contains(err.Error(), "cli path empty") {
		t.Fatalf("expected cli path err, got %v", err)
	}
}
