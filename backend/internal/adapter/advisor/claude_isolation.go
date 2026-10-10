package advisor

import (
	"os"
	"os/exec"
	"strings"
)

// claudeIsolationArgs keeps the repository's own Claude Code hooks (.claude/settings.json: the
// Stop hook that runs the test suite, etc.) out of the bot's `claude -p` calls. Those hooks guard
// interactive development sessions; running them on every trading decision only adds latency and
// runs `make` with the bot's environment.
//
// The same settings deny the Read tool access to secret files: the CLI runs with the repo root as
// its working directory (so subagents can read .claude/agents and prompts/), and .env, key files
// and the live bot config sit there too.
var claudeIsolationArgs = []string{"--settings", `{"disableAllHooks":true,"permissions":{"deny":[` +
	`"Read(./.env)","Read(**/.env)","Read(**/.env.*)","Read(**/*.pem)","Read(**/*.key)",` +
	`"Read(./configs/bot_config.live.yaml)"]}}`}

// ClaudeIsolationArgs returns a copy of the hook-disabling CLI args for callers outside this
// package (the dashboard's ask-claude runner).
func ClaudeIsolationArgs() []string {
	return append([]string(nil), claudeIsolationArgs...)
}

// claudeEnvKeys / claudeEnvPrefixes are what the claude CLI needs to run and authenticate
// (Anthropic API key or OAuth token, Bedrock / Vertex credentials, proxies, locale, home dirs).
var claudeEnvKeys = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TERM": true,
	"TMPDIR": true, "TZ": true, "LANG": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true,
	"__CF_USER_TEXT_ENCODING": true,
}

var claudeEnvPrefixes = []string{"LC_", "XDG_", "ANTHROPIC_", "CLAUDE_"}

// Cloud credentials are forwarded only when the claude CLI is configured to use that provider, so
// unrelated AWS / Google credentials in the bot's environment do not reach the LLM process.
var (
	bedrockEnvPrefixes = []string{"AWS_"}
	vertexEnvPrefixes  = []string{"GOOGLE_", "CLOUD_ML_", "VERTEX_"}
)

// ClaudeEnv filters environ (os.Environ() form) down to what the claude CLI needs. The bot's own
// secrets — broker API keys, DB DSNs, dashboard credentials — are not passed to the LLM process.
func ClaudeEnv(environ []string) []string {
	prefixes := append([]string(nil), claudeEnvPrefixes...)
	for _, kv := range environ {
		key, val, _ := strings.Cut(kv, "=")
		if val == "" || val == "0" {
			continue
		}
		switch key {
		case "CLAUDE_CODE_USE_BEDROCK":
			prefixes = append(prefixes, bedrockEnvPrefixes...)
		case "CLAUDE_CODE_USE_VERTEX":
			prefixes = append(prefixes, vertexEnvPrefixes...)
		}
	}
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if claudeEnvKeys[key] {
			out = append(out, kv)
			continue
		}
		for _, p := range prefixes {
			if strings.HasPrefix(key, p) {
				out = append(out, kv)
				break
			}
		}
	}
	return out
}

// applyClaudeEnv gives a claude subprocess the filtered environment. A command whose Env is already
// set (test fakes that re-invoke the test binary) is left alone.
func applyClaudeEnv(cmd *exec.Cmd) {
	if cmd != nil && cmd.Env == nil {
		cmd.Env = ClaudeEnv(os.Environ())
	}
}
