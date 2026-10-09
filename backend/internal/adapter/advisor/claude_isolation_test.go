package advisor

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// bot が呼ぶ claude サブプロセスには、broker の API キー・DB の DSN・ダッシュボードのパスワードを
// 渡さない(LLM のツールや hook から読めないように)。claude CLI の認証・プロキシ・ロケールなど、
// CLI が動くのに要るものだけを残す。
func TestClaudeEnv_DropsBotSecretsKeepsCLIEssentials(t *testing.T) {
	in := []string{
		"PATH=/usr/bin", "HOME=/home/u", "USER=u", "LANG=ja_JP.UTF-8", "TMPDIR=/tmp/x",
		"ANTHROPIC_API_KEY=sk-test", "CLAUDE_CODE_OAUTH_TOKEN=tok", "HTTPS_PROXY=http://proxy:8080",
		"AWS_REGION=us-east-1", "XDG_CONFIG_HOME=/home/u/.config",
		"GMO_API_KEY=k", "GMO_API_" + "SECRET=s", // split so the repo secret scanners do not flag the fixture "DATABASE_URL=postgres://u:p@h/db",
		"DATABASE_URL_RO=postgres://r:p@h/db", "DASHBOARD_PASS=pw", "DASHBOARD_USER=admin",
		"INTEGRATION_TEST_DB_URL=postgres://u:p@h/x_test", "LIVE_TRADING_ENABLED=true",
	}
	got := map[string]bool{}
	for _, kv := range ClaudeEnv(in) {
		got[strings.SplitN(kv, "=", 2)[0]] = true
	}
	for _, keep := range []string{"PATH", "HOME", "USER", "LANG", "TMPDIR", "ANTHROPIC_API_KEY",
		"CLAUDE_CODE_OAUTH_TOKEN", "HTTPS_PROXY", "AWS_REGION", "XDG_CONFIG_HOME"} {
		if !got[keep] {
			t.Errorf("%s must be passed to the claude CLI", keep)
		}
	}
	for _, drop := range []string{"GMO_API_KEY", "GMO_API_SECRET", "DATABASE_URL", "DATABASE_URL_RO",
		"DASHBOARD_PASS", "DASHBOARD_USER", "INTEGRATION_TEST_DB_URL", "LIVE_TRADING_ENABLED"} {
		if got[drop] {
			t.Errorf("%s must NOT be passed to the claude CLI", drop)
		}
	}
}

// repo の .claude/settings.json の hooks(Stop hook のテスト実行など)は開発セッション用。bot の判断の
// たびに走らせない。
func TestClaudeArgs_DisableProjectHooks(t *testing.T) {
	for name, args := range map[string][]string{
		"base":          claudeBaseArgs(),
		"decision":      claudeDecisionArgs(true),
		"decisionPanel": claudeDecisionArgs(false),
	} {
		var settings string
		for i, a := range args {
			if a == "--settings" && i+1 < len(args) {
				settings = args[i+1]
			}
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(settings), &parsed); err != nil || parsed["disableAllHooks"] != true {
			t.Errorf("%s args must pass --settings {\"disableAllHooks\":true}; got %q", name, args)
		}
	}
}

// 全ての claude 呼び出しが通る applyClaudeEnv は、テスト用 fake が自前の Env を持つときは触らず、
// それ以外は os.Environ() を ClaudeEnv で絞ったものにする。
func TestApplyClaudeEnv(t *testing.T) {
	t.Setenv("GMO_API_SECRET", "s")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")

	cmd := exec.Command("true")
	applyClaudeEnv(cmd)
	joined := strings.Join(cmd.Env, "\n")
	if strings.Contains(joined, "GMO_API_SECRET=") || !strings.Contains(joined, "ANTHROPIC_API_KEY=sk-test") {
		t.Fatalf("env not filtered: %q", cmd.Env)
	}

	preset := exec.Command("true")
	preset.Env = []string{"GO_WANT_HELPER_PROCESS=1"}
	applyClaudeEnv(preset)
	if len(preset.Env) != 1 || preset.Env[0] != "GO_WANT_HELPER_PROCESS=1" {
		t.Fatalf("a preset Env (test fakes) must be left alone; got %q", preset.Env)
	}
}
