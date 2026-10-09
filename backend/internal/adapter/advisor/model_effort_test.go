package advisor

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// Run the ENTIRE autonomous loop — the orchestrating `claude -p`
// AND every Task subagent it spawns (subagents inherit the session model + reasoning effort)
// — on the latest Opus (alias "opus", which tracks the newest Opus instead of a pinned version)
// at MAX effort (favor decision quality over token cost). Nothing else in
// the repo sets --model/--effort, so WITHOUT these flags the loop floats to whatever the CLI
// default model happens to be. These tests pin the flags so a future refactor can't silently
// drop them.

func TestClaudeBaseArgs_PinsLatestOpusAndMaxEffort(t *testing.T) {
	joined := strings.Join(claudeBaseArgs(), " ")
	for _, want := range []string{
		"-p",
		"--no-session-persistence",
		"--tools Read,Task",
		"--model opus",
		"--effort max",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("claudeBaseArgs missing %q: got %v", want, claudeBaseArgs())
		}
	}
}

func TestLLMDecide_InvokesLatestOpusMaxEffort(t *testing.T) {
	d := newLLMDecider(t, "decision_no_trade")
	var gotArgs []string
	base := d.ExecCommand
	d.ExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotArgs = append([]string(nil), args...)
		return base(ctx, name, args...)
	}
	if _, err := d.Decide(context.Background(), "USD_JPY", newSummary(), "pb"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "--model opus") || !strings.Contains(joined, "--effort max") {
		t.Fatalf("trade-decider must invoke latest opus at max effort: %v", gotArgs)
	}
}

func TestReflect_InvokesLatestOpusMaxEffort(t *testing.T) {
	r := &ReflectionCLI{
		CLIPath:        "/usr/local/bin/claude", // ignored by fakeExec
		TimeoutSeconds: 3,
		ExecCommand:    fakeExec("empty_stdout"),
	}
	var gotArgs []string
	base := r.ExecCommand
	r.ExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotArgs = append([]string(nil), args...)
		return base(ctx, name, args...)
	}
	if _, _, err := r.Reflect(context.Background(), "digest", "playbook"); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "--model opus") || !strings.Contains(joined, "--effort max") {
		t.Fatalf("reflection panel must invoke latest opus at max effort: %v", gotArgs)
	}
}
