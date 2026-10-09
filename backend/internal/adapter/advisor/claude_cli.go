// Package advisor implements the Claude CLI adapters (port.Advisor plus the
// LLM decision / reflection / breakout-judge callers).
//
// Concept: We do NOT use the Claude API from Go. We invoke `claude -p`
// (Claude Code headless) and parse its stdout. ClaudeCLIAdvisor emits a
// strategy-config YAML every ai_advisor.interval_minutes (opt-in);
// LLMDecisionCLI / BreakoutJudgeCLI return trade decisions that still pass
// the risk Gate and the broker-side OCO. Claude never places orders itself.
package advisor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
)

// ExecCommandFunc mirrors exec.CommandContext for test injection. In tests
// the caller can supply a fake that re-invokes the test binary in
// TestHelperProcess mode.
type ExecCommandFunc func(ctx context.Context, name string, args ...string) *exec.Cmd

// claudeModelArgs pins the model + reasoning effort for EVERY `claude -p` invocation in this
// package — advisor, trade-decider and reflection — and, transitively, for every Task subagent
// they spawn (subagents inherit the session model + effort): the whole autonomous loop runs on
// the latest Opus at max effort (favor decision quality over token cost).
// Nothing else sets --model/--effort, so without this the loop floats to the CLI default model.
// 固定バージョンではなくエイリアス "opus" で常に最新 Opus に追従する。CLI デフォルトに任せない
// 理由: デフォルトは opus 系とは限らず (plan/設定次第で sonnet 等)、判断品質の下振れを許すため。
var claudeModelArgs = []string{"--model", "opus", "--effort", "max"}

// claudeBaseArgs is the canonical `claude -p` argument list: print mode, no session persistence,
// only Read+Task tools, the pinned model/effort and the repo hooks disabled. Returns a fresh slice
// each call (no aliasing).
func claudeBaseArgs() []string {
	args := append([]string{"-p", "--no-session-persistence", "--tools", "Read,Task"}, claudeModelArgs...)
	return append(args, claudeIsolationArgs...)
}

// claudeDecisionArgs is the trade-decision invocation. In SINGLE-AGENT mode it grants ONLY Read
// (no Task) so claude physically CANNOT spawn sub-agents — deterministically removing the stray-
// panel class of bugs. Panel mode keeps Task so the market-regime/trade-decider
// sub-agent panel can run (set via config decision_single_agent:false to revert).
func claudeDecisionArgs(singleAgent bool) []string {
	tools := "Read,Task"
	if singleAgent {
		tools = "Read"
	}
	args := append([]string{"-p", "--no-session-persistence", "--tools", tools}, claudeModelArgs...)
	return append(args, claudeIsolationArgs...)
}

// ClaudeCLIAdvisor invokes `claude -p` with the configured prompt + the
// MarketSummary serialized as JSON appended to the prompt, then captures
// stdout.
//
// The output is expected to be a YAML document (no markdown fences). We strip
// ``` fences defensively because Claude occasionally adds them despite the
// prompt instructing otherwise.
type ClaudeCLIAdvisor struct {
	CLIPath        string // path to `claude` binary
	PromptPath     string // prompts/generate_strategy_config.md
	OutputDir      string // runtime/ai_output/
	NextYAMLPath   string // configs/strategy_config.next.yaml
	TimeoutSeconds int    // 120 by default
	Logger         *slog.Logger
	Clock          func() time.Time
	IDGen          func() string   // run_id generator
	ExecCommand    ExecCommandFunc // exec.CommandContext or fake

	// WorkingDir は claude -p サブプロセスの CWD。プロンプトから skill 等を
	// Read("prompts/skills/...") のように相対パス参照する設計なので、
	// プロジェクトルートをここに渡して Claude の CWD を予測可能にする。
	// 空のときは Go の current working directory を継承 (テスト互換用)。
	WorkingDir string

	// TransientRetries は exit 0 だが stdout が transient な API/接続/過負荷
	// エラー (transientCLIErrorRE) だった場合の追加リトライ回数。New() で 1。
	// 一時障害は数秒で自己回復するので即リトライで当該 cycle の判断を救う。
	// usage/session limit (usageLimitRE) は数時間戻らないのでリトライしない。
	TransientRetries int
	// TransientRetryBackoff は上記リトライ間の待機。New() で 5s。ctx を尊重。
	TransientRetryBackoff time.Duration
}

// New creates a ClaudeCLIAdvisor with sensible defaults filled in.
func New(cli, promptPath, outputDir, nextYAMLPath string, timeoutSec int) *ClaudeCLIAdvisor {
	return &ClaudeCLIAdvisor{
		CLIPath:        cli,
		PromptPath:     promptPath,
		OutputDir:      outputDir,
		NextYAMLPath:   nextYAMLPath,
		TimeoutSeconds: timeoutSec,
		Logger:         slog.Default(),
		Clock:          time.Now,
		IDGen:          defaultRunID,
		ExecCommand:    exec.CommandContext,

		TransientRetries:      1,
		TransientRetryBackoff: 5 * time.Second,
	}
}

var (
	// transientCLIErrorRE は claude CLI が exit 0 のまま STDOUT に吐くインフラ系
	// 一時エラー。これらは exec-error 枝を素通りし、config_id が無いので parse_error
	// に誤分類されてしまう。実例: "API Error: 529 Overloaded",
	// "Unable to connect to API (ConnectionRefused)"。数秒で回復する
	// ので cli_error に分類のうえ即リトライする。
	transientCLIErrorRE = regexp.MustCompile(`(?i)API Error|Overloaded|ConnectionRefused|connection refused|Unable to connect to API`)

	// usageLimitRE は Claude の利用/セッション上限メッセージ。数時間戻らないので
	// リトライせず cli_error として明示分類する。signature: "You've hit your
	// session limit · resets 4:20am"。
	usageLimitRE = regexp.MustCompile(`(?i)session limit|usage limit|hit your.{0,20}limit`)
)

// firstLine は stdout の先頭非空行を最大 200 文字で返す (エラーメッセージ用)。
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			return truncate(ln, 200)
		}
	}
	return ""
}

func defaultRunID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// fence stripping + YAML 開始判定は prompt_parser.go の ResponseParser が担う。

// Generate runs the Claude CLI and returns an AdvisorRun describing the
// outcome. The caller is expected to validate ParsedYAML through
// usecase.UpdateStrategyConfig before promoting it.
func (a *ClaudeCLIAdvisor) Generate(ctx context.Context, summary *market.MarketSummary) (*port.AdvisorRun, error) {
	if a.CLIPath == "" {
		return nil, errors.New("advisor: cli path empty")
	}
	// 入力組立: prompt body + Summary JSON を connection。
	asm := &PromptAssembler{PromptPath: a.PromptPath}
	payload, summaryJSON, err := asm.Build(summary)
	if err != nil {
		return nil, err
	}

	// Context 尊重: ctx.Deadline() があればそちらを尊重し、
	// 設定 TimeoutSeconds はそれより短いときだけ追加適用する (= 最短側を採用)。
	cctx, cancel := a.applyTimeout(ctx)
	defer cancel()

	run := &port.AdvisorRun{
		RunID:     a.IDGen(),
		StartedAt: a.Clock(),
		InputJSON: summaryJSON,
	}

	// Read は skill ファイル参照に、Task は 4 並列 subagent
	// (.claude/agents/{risk-auditor,regime-classifier,strategy-selector,tpsl-designer}.md)
	// の起動に必要。subagent の tool 権限は各 agent 側 frontmatter (tools: Read)
	// で read-only に絞っているので、advisor サブプロセス全体として Bash/Edit/Write
	// などの破壊的ツールは一切使えない。
	parser := &ResponseParser{}
	attempts := a.TransientRetries + 1
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			// transient エラーの再試行前バックオフ。ctx deadline を尊重して
			// timeout を食い潰さない。
			select {
			case <-cctx.Done():
				run.Status = port.AdvisorRunStatusTimeout
				run.ErrorMsg = "claude CLI timed out during transient retry backoff"
				a.logRun(run, "timeout", "")
				_ = a.persistRawOutput(run)
				return run, nil
			case <-time.After(a.TransientRetryBackoff):
			}
		}

		cmd := a.ExecCommand(cctx, a.CLIPath, claudeBaseArgs()...)
		applyClaudeEnv(cmd)
		if a.WorkingDir != "" {
			cmd.Dir = a.WorkingDir
		}
		cmd.Stdin = bytes.NewReader(payload)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		err = cmd.Run()
		run.FinishedAt = a.Clock()
		run.OutputYAML = stdout.Bytes()

		// Categorise failure modes.
		if cctx.Err() == context.DeadlineExceeded {
			run.Status = port.AdvisorRunStatusTimeout
			run.ErrorMsg = "claude CLI timed out"
			a.logRun(run, "timeout", stderr.String())
			_ = a.persistRawOutput(run)
			return run, nil
		}
		if err != nil {
			var exitErr *exec.ExitError
			isExit := errors.As(err, &exitErr)
			// claude CLI が usage/session limit や transient infra error を
			// **非ゼロ exit** で返すとき、メッセージは STDOUT に出て stderr は空。
			// bare "claude exit 1:" を返すだけだと原因が DB/通知から見えず、
			// UsageLimited も立たないので scheduler が fast-retry で枯渇した quota を
			// 叩き続けてしまう。exit 0 path (下) と同じ STDOUT 分類をここでも先に通す。
			out := string(run.OutputYAML)
			switch outcome := classifyCLIOutput(out); {
			case isExit && outcome == cliOutcomeUsageLimit:
				run.Status = port.AdvisorRunStatusCLIError
				run.ErrorMsg = "claude usage/session limit: " + firstLine(out)
				run.UsageLimited = true
				a.logRun(run, string(run.Status), stderr.String())
				_ = a.persistRawOutput(run)
				return run, nil
			case isExit && outcome == cliOutcomeTransient:
				run.Status = port.AdvisorRunStatusCLIError
				run.ErrorMsg = "claude transient API error: " + firstLine(out)
				a.logRun(run, string(run.Status), stderr.String())
				_ = a.persistRawOutput(run)
				if attempt < attempts-1 {
					continue
				}
				return run, nil
			case isExit:
				run.Status = port.AdvisorRunStatusCLIError
				run.ErrorMsg = fmt.Sprintf("claude exit %d: %s", exitErr.ExitCode(), truncate(stderr.String(), 500))
			default:
				run.Status = port.AdvisorRunStatusCLIError
				run.ErrorMsg = err.Error()
			}
			a.logRun(run, "cli_error", stderr.String())
			_ = a.persistRawOutput(run)
			return run, nil
		}

		// Parse stdout → ParsedYAML
		parsed, perr := parser.Parse(stdout.Bytes())
		if perr == nil {
			run.ParsedYAML = parsed
			run.Status = port.AdvisorRunStatusSuccess
			if err := a.persistOutputs(run); err != nil {
				run.Status = port.AdvisorRunStatusCLIError
				run.ErrorMsg = fmt.Sprintf("output persist failed: %v", err)
				return run, nil
			}
			a.logRun(run, "success", "")
			return run, nil
		}

		// Parse 失敗。claude CLI は API/接続/過負荷/上限エラーを exit 0 のまま
		// STDOUT に吐くので、ここで識別する。これらは「YAML が壊れている」のでは
		// なく外部要因なので parse_error ではなく cli_error に分類する。transient
		// (529/接続) は数秒で回復するので即リトライ。usage/session limit は数時間
		// 戻らないのでリトライしない。
		out := string(run.OutputYAML)
		switch outcome := classifyCLIOutput(out); {
		case outcome == cliOutcomeUsageLimit:
			run.Status = port.AdvisorRunStatusCLIError
			run.ErrorMsg = "claude usage/session limit: " + firstLine(out)
			run.UsageLimited = true
			a.logRun(run, string(run.Status), stderr.String())
			_ = a.persistRawOutput(run)
			return run, nil
		case outcome == cliOutcomeTransient:
			run.Status = port.AdvisorRunStatusCLIError
			run.ErrorMsg = "claude transient API error: " + firstLine(out)
			a.logRun(run, string(run.Status), stderr.String())
			_ = a.persistRawOutput(run)
			if attempt < attempts-1 {
				continue
			}
			return run, nil
		case perr.Error() == "empty stdout":
			run.Status = port.AdvisorRunStatusCLIError
			run.ErrorMsg = perr.Error()
			a.logRun(run, string(run.Status), stderr.String())
			_ = a.persistRawOutput(run)
			return run, nil
		default:
			// 真の YAML 崩れ (config_id 欠落 / fence-only)。リトライしても同じ。
			run.Status = port.AdvisorRunStatusParseError
			run.ErrorMsg = perr.Error()
			a.logRun(run, string(run.Status), stderr.String())
			_ = a.persistRawOutput(run)
			return run, nil
		}
	}
	return run, nil
}

// applyTimeout は ctx.Deadline と TimeoutSeconds の短い方を採用した子 context を返す。
//   - ctx に deadline がなければ TimeoutSeconds (or default 120s) を適用
//   - ctx に deadline があれば、それより TimeoutSeconds が短いときだけ縮める
func (a *ClaudeCLIAdvisor) applyTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	cfg := a.TimeoutSeconds
	if cfg <= 0 {
		cfg = 120
	}
	cfgDur := time.Duration(cfg) * time.Second
	parentDeadline, hasParent := ctx.Deadline()
	cfgDeadline := a.Clock().Add(cfgDur)
	if !hasParent || cfgDeadline.Before(parentDeadline) {
		return context.WithDeadline(ctx, cfgDeadline)
	}
	// 親 deadline の方が早いので追加適用不要。cancel は no-op。
	return context.WithCancel(ctx)
}

func (a *ClaudeCLIAdvisor) logRun(r *port.AdvisorRun, label, stderr string) {
	lg := a.Logger
	if lg == nil {
		return
	}
	lg.Info("advisor_run",
		"run_id", r.RunID,
		"status", string(r.Status),
		"label", label,
		"duration_ms", r.FinishedAt.Sub(r.StartedAt).Milliseconds(),
		"err", truncate(r.ErrorMsg, 200),
		"stderr", truncate(stderr, 200),
	)
}

// persistOutputs writes the raw YAML to runtime/ai_output and atomically
// stages configs/strategy_config.next.yaml. On error the partial files are
// cleaned up (best-effort).
func (a *ClaudeCLIAdvisor) persistOutputs(r *port.AdvisorRun) error {
	if a.OutputDir == "" || a.NextYAMLPath == "" {
		return nil
	}
	if err := os.MkdirAll(a.OutputDir, 0o755); err != nil {
		return fmt.Errorf("mkdir output: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(a.NextYAMLPath), 0o755); err != nil {
		return fmt.Errorf("mkdir next: %w", err)
	}
	// Archive raw output to ai_output/strategy_config_YYYYMMDD_HHMMSS_runid.yaml
	tsName := r.StartedAt.UTC().Format("20060102_150405")
	archivePath := filepath.Join(a.OutputDir, fmt.Sprintf("strategy_config_%s_%s.yaml", tsName, r.RunID))
	if err := writeFileAtomic(archivePath, r.OutputYAML); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	// Stage next.yaml with the cleaned content
	if err := writeFileAtomic(a.NextYAMLPath, r.ParsedYAML); err != nil {
		return fmt.Errorf("stage next.yaml: %w", err)
	}
	return nil
}

// persistRawOutput writes only the archive (no next.yaml) — used on failure
// to preserve evidence.
func (a *ClaudeCLIAdvisor) persistRawOutput(r *port.AdvisorRun) error {
	if a.OutputDir == "" || len(r.OutputYAML) == 0 {
		return nil
	}
	if err := os.MkdirAll(a.OutputDir, 0o755); err != nil {
		return err
	}
	tsName := r.StartedAt.UTC().Format("20060102_150405")
	archivePath := filepath.Join(a.OutputDir, fmt.Sprintf("strategy_config_%s_%s.fail.yaml", tsName, r.RunID))
	return writeFileAtomic(archivePath, r.OutputYAML)
}

func writeFileAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "ai-out-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := io.Copy(tmp, bytes.NewReader(body)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
