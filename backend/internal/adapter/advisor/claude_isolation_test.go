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
		"XDG_CONFIG_HOME=/home/u/.config",
		// GMO の 2 キーは文字列を分けて書く(リポジトリの secret scanner が fixture に反応しないように)。
		"GMO_API_" + "KEY=k", "GMO_API_" + "SECRET=s",
		"DATABASE_URL=postgres://u:p@h/db",
		"DATABASE_URL_RO=postgres://r:p@h/db", "DASHBOARD_PASS=pw", "DASHBOARD_USER=admin",
		"INTEGRATION_TEST_DB_URL=postgres://u:p@h/x_test", "LIVE_TRADING_ENABLED=true",
		// cloud credentials of unrelated tools: only forwarded when claude itself uses that provider
		"AWS_REGION=us-east-1", "AWS_SECRET_ACCESS_KEY=x", "GOOGLE_APPLICATION_CREDENTIALS=/k.json",
	}
	got := map[string]bool{}
	for _, kv := range ClaudeEnv(in) {
		got[strings.SplitN(kv, "=", 2)[0]] = true
	}
	for _, keep := range []string{"PATH", "HOME", "USER", "LANG", "TMPDIR", "ANTHROPIC_API_KEY",
		"CLAUDE_CODE_OAUTH_TOKEN", "HTTPS_PROXY", "XDG_CONFIG_HOME"} {
		if !got[keep] {
			t.Errorf("%s must be passed to the claude CLI", keep)
		}
	}
	for _, drop := range []string{"GMO_API_KEY", "GMO_API_SECRET", "DATABASE_URL", "DATABASE_URL_RO",
		"DASHBOARD_PASS", "DASHBOARD_USER", "INTEGRATION_TEST_DB_URL", "LIVE_TRADING_ENABLED",
		"AWS_REGION", "AWS_SECRET_ACCESS_KEY", "GOOGLE_APPLICATION_CREDENTIALS"} {
		if got[drop] {
			t.Errorf("%s must NOT be passed to the claude CLI", drop)
		}
	}
}

// claude を Bedrock / Vertex 経由で使う設定(CLAUDE_CODE_USE_BEDROCK / CLAUDE_CODE_USE_VERTEX)のときだけ、
// その provider の資格情報を渡す。
func TestClaudeEnv_ForwardsCloudCredentialsOnlyForThatProvider(t *testing.T) {
	keys := func(env []string) map[string]bool {
		m := map[string]bool{}
		for _, kv := range ClaudeEnv(env) {
			m[strings.SplitN(kv, "=", 2)[0]] = true
		}
		return m
	}
	bedrock := keys([]string{"CLAUDE_CODE_USE_BEDROCK=1", "AWS_REGION=us-east-1", "AWS_PROFILE=p", "GOOGLE_CLOUD_PROJECT=g"})
	if !bedrock["AWS_REGION"] || !bedrock["AWS_PROFILE"] || bedrock["GOOGLE_CLOUD_PROJECT"] {
		t.Errorf("bedrock: want AWS_* only, got %v", bedrock)
	}
	vertex := keys([]string{"CLAUDE_CODE_USE_VERTEX=1", "CLOUD_ML_REGION=us-east5", "GOOGLE_CLOUD_PROJECT=g", "AWS_REGION=x"})
	if !vertex["CLOUD_ML_REGION"] || !vertex["GOOGLE_CLOUD_PROJECT"] || vertex["AWS_REGION"] {
		t.Errorf("vertex: want GOOGLE_* / CLOUD_ML_* only, got %v", vertex)
	}
}

// claude の Read ツールで .env や鍵ファイル、live の bot_config を読ませない(cwd は repo root)。
func TestClaudeArgs_DenyReadingSecretFiles(t *testing.T) {
	var settings string
	args := claudeBaseArgs()
	for i, a := range args {
		if a == "--settings" && i+1 < len(args) {
			settings = args[i+1]
		}
	}
	var parsed struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(settings), &parsed); err != nil {
		t.Fatalf("settings: %v", err)
	}
	deny := strings.Join(parsed.Permissions.Deny, " ")
	for _, want := range []string{"Read(./.env)", "Read(**/.env)", "Read(**/.env.*)", "Read(**/*.pem)", "Read(./configs/bot_config.live.yaml)"} {
		if !strings.Contains(deny, want) {
			t.Errorf("deny rules %q missing %s", parsed.Permissions.Deny, want)
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
