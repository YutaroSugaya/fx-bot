package advisor

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"time"
)

// ReflectionCLI runs the reflection step of the autonomous learning loop: one `claude -p`
// whose inline prompt (BuildReflectionPayload) orchestrates the reflection-{regime,risk,strategy}
// subagents via Task (the former reflection-analyst orchestrator role is inlined here). Given a
// win/loss digest + the current playbook, it returns a revised playbook and whether to apply it.
//
// FAIL-SAFE: a CLI/transport failure returns ("", false) + error; a garbled-output PARSE failure
// returns ("", false) and NO error (= keep the current playbook). The reflection loop only swaps
// advisory text; it never touches the order path.
type ReflectionCLI struct {
	CLIPath        string
	WorkingDir     string
	TimeoutSeconds int
	Logger         *slog.Logger
	ExecCommand    ExecCommandFunc
}

// NewReflectionCLI builds a reflector with exec.CommandContext and a default (longer) timeout.
func NewReflectionCLI(cli, workingDir string, timeoutSec int, logger *slog.Logger) *ReflectionCLI {
	if timeoutSec <= 0 {
		timeoutSec = 180
	}
	return &ReflectionCLI{
		CLIPath: cli, WorkingDir: workingDir, TimeoutSeconds: timeoutSec,
		Logger: logger, ExecCommand: exec.CommandContext,
	}
}

// BuildReflectionPayload is the stdin prompt that runs the MULTI-AGENT reflection panel and
// synthesizes a revised strategy. The orchestrating Claude session uses the Task tool to consult
// three Read-only specialist subagents from different angles, then merges their findings into one
// conservative strategy revision. Multi-perspective review is the overfitting guard for strategy R&D
// (a single agent rewriting from a few noisy trades chases noise). Pure (no I/O).
func BuildReflectionPayload(tradesSummary, currentPlaybook string) []byte {
	var b bytes.Buffer
	b.WriteString("You are the LEAD strategy developer for an autonomous FX bot. The playbook is a STRATEGY ")
	b.WriteString("PORTFOLIO (multiple named strategies, each with an applicable regime + entry conditions + TP/SL). ")
	b.WriteString("Review it against recent trade outcomes using a PANEL of specialist subagents (use the Task ")
	b.WriteString("tool), then output the REVISED PORTFOLIO. Be conservative: do NOT chase noise; if evidence is ")
	b.WriteString("weak or the sample is small, set update:false.\n\n")
	b.WriteString("Run these three subagents and read their findings:\n")
	b.WriteString("  1. reflection-regime  — which regimes/sessions/vol does EACH strategy win or lose in? (regime fit)\n")
	b.WriteString("  2. reflection-risk    — risk/cost flaws per strategy (RR, SL too tight, spread, cost floor ~1.1pips, sizing)?\n")
	b.WriteString("  3. reflection-strategy— concrete revised entry conditions / regime mapping that fix the flaws.\n")
	b.WriteString("Then SYNTHESIZE: improve EACH strategy individually, sharpen the regime→strategy mapping (which ")
	b.WriteString("strategy to use in which regime — drop or quarantine a strategy that keeps losing), and you may ")
	b.WriteString("add ONE new strategy only with clear evidence. Keep it conservative — prefer update:false when thin.\n")
	b.WriteString("HUMAN-LOCKED BLOCKS: the portfolio's 【HARD禁止】 and ")
	b.WriteString("【この通貨の規律】 blocks are human-approved discipline ")
	b.WriteString("enforced by code vetoes (night_buy_veto / chase_buy_veto / sell_low_veto). Carry them into the ")
	b.WriteString("revised portfolio verbatim — never relax, weaken or delete them (you may PROPOSE tightening in ")
	b.WriteString("the revision note; only a human may relax them, never the reflection loop).\n\n")
	b.WriteString("Output ONLY the YAML document with `update` and `rules_jp` (the FULL revised portfolio: each ")
	b.WriteString("strategy with name, applicable regime, direction, numbered entry conditions, TP/SL, exclusions). No prose, no fences.\n\n")
	b.WriteString("=== current_portfolio ===\n")
	b.WriteString(currentPlaybook)
	b.WriteString("\n\n=== recent_trades ===\n")
	b.WriteString(tradesSummary)
	b.WriteString("\n")
	return b.Bytes()
}

// Reflect runs the subagent and returns (newRules, update). Fail-safe to no-op.
func (r *ReflectionCLI) Reflect(ctx context.Context, tradesSummary, currentPlaybook string) (string, bool, error) {
	payload := BuildReflectionPayload(tradesSummary, currentPlaybook)
	cctx, cancel := context.WithTimeout(ctx, time.Duration(r.TimeoutSeconds)*time.Second)
	defer cancel()

	cmd := r.ExecCommand(cctx, r.CLIPath, claudeBaseArgs()...)
	applyClaudeEnv(cmd)
	if r.WorkingDir != "" {
		cmd.Dir = r.WorkingDir
	}
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if rerr := cmd.Run(); rerr != nil {
		return "", false, fmt.Errorf("reflection-analyst cli: %w (%s)", rerr, stderr.String())
	}
	rules, update := ParseReflection(stdout.Bytes())
	return rules, update, nil
}
