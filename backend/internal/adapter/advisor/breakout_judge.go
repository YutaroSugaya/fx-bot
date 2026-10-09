package advisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/strategy"
)

// BreakoutJudgeCLI invokes the `breakout-advisor` subagent (advisor v2) to grade ONE deterministic
// proposal and returns a domain AdvisorVerdict. It is the I/O boundary; the orchestration
// (command.SignatureCycle) and the parser (ParseBreakoutDecision) hold the tested logic.
//
// FAIL-SAFE: a CLI/transport failure returns a no-trade verdict AND an error (so the cycle can
// log/retry); a garbled-output PARSE failure returns a no-trade verdict and NO error (garbage =
// stay flat, nothing to retry). The LLM only judges go/no-go — it never sets the stop.
type BreakoutJudgeCLI struct {
	CLIPath        string
	WorkingDir     string // project root (so the subagent can Read .claude/agents + prompts)
	TimeoutSeconds int
	Logger         *slog.Logger
	ExecCommand    ExecCommandFunc // exec.CommandContext or a test fake
}

// NewBreakoutJudge builds a judge with exec.CommandContext and a default timeout.
func NewBreakoutJudge(cli, workingDir string, timeoutSec int, logger *slog.Logger) *BreakoutJudgeCLI {
	if timeoutSec <= 0 {
		timeoutSec = 120
	}
	return &BreakoutJudgeCLI{
		CLIPath: cli, WorkingDir: workingDir, TimeoutSeconds: timeoutSec,
		Logger: logger, ExecCommand: exec.CommandContext,
	}
}

// BuildJudgePayload is the stdin prompt that tells Claude to run the breakout-advisor subagent on
// one proposal. Pure (no I/O) so it is unit-testable.
func BuildJudgePayload(symbol string, p strategy.BreakoutProposal, summary *market.MarketSummary) ([]byte, error) {
	input := map[string]any{
		"symbol":   symbol,
		"proposal": p,
		"summary":  summary,
	}
	j, err := json.MarshalIndent(input, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal judge input: %w", err)
	}
	var b bytes.Buffer
	b.WriteString("Use the breakout-advisor subagent (.claude/agents/breakout-advisor.md) to judge this ")
	b.WriteString("single proposal against the fixed 8 axes. Default to no_trade unless every axis is green. ")
	b.WriteString("Output ONLY the subagent's YAML decision document (no prose, no fences).\n\nInput JSON:\n")
	b.Write(j)
	b.WriteString("\n")
	return b.Bytes(), nil
}

func (j *BreakoutJudgeCLI) noTrade() strategy.AdvisorVerdict {
	return strategy.AdvisorVerdict{Go: false, Label: strategy.BreakoutNoTrade, Side: "none"}
}

// Judge runs the subagent on one proposal and returns the verdict (fail-safe to no-trade).
func (j *BreakoutJudgeCLI) Judge(ctx context.Context, symbol string, p strategy.BreakoutProposal, summary *market.MarketSummary) (strategy.AdvisorVerdict, error) {
	payload, err := BuildJudgePayload(symbol, p, summary)
	if err != nil {
		return j.noTrade(), err
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(j.TimeoutSeconds)*time.Second)
	defer cancel()

	cmd := j.ExecCommand(cctx, j.CLIPath, claudeBaseArgs()...)
	applyClaudeEnv(cmd)
	if j.WorkingDir != "" {
		cmd.Dir = j.WorkingDir
	}
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if rerr := cmd.Run(); rerr != nil {
		// Transport/infra failure -> no-trade + error (cycle decides to retry/log).
		return j.noTrade(), fmt.Errorf("breakout-advisor cli: %w (%s)", rerr, stderr.String())
	}
	dec, perr := ParseBreakoutDecision(stdout.Bytes())
	if perr != nil && j.Logger != nil {
		j.Logger.Warn("breakout_advisor_parse_fallback_no_trade", "symbol", symbol, "err", perr)
	}
	// dec is already a safe no-trade on any parse problem; never surface parse errors as retryable.
	return dec.Verdict(), nil
}

// JudgeFunc adapts Judge to the command.SignatureCycle.Judge signature for a fixed symbol.
func (j *BreakoutJudgeCLI) JudgeFunc(symbol string) func(ctx context.Context, p strategy.BreakoutProposal, summary *market.MarketSummary) (strategy.AdvisorVerdict, error) {
	return func(ctx context.Context, p strategy.BreakoutProposal, summary *market.MarketSummary) (strategy.AdvisorVerdict, error) {
		return j.Judge(ctx, symbol, p, summary)
	}
}
