package main

import (
	"io"
	"log/slog"
	"testing"

	"fx-bot/backend/internal/config"
)

// buildAPIAuth escape hatch: operator が DASHBOARD_AUTH_DISABLE=true
// を明示的に立てた時だけ、bot.mode が paper_config / live_config でも auth bypass
// を許可する。production で auth を抜くのは本来 anti-pattern だが、運用判断で
// 必要なケースがあるため、明示 opt-in を loud (WARN ログ + 専用 env 名) で残す。
//
// 既定のセマンティクス (= bot.mode=disabled でだけ bypass) は維持。

func silentLg() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestBuildAPIAuth_DashboardAuthDisableEnvAllowsBypassInLive(t *testing.T) {
	t.Setenv("DASHBOARD_AUTH_DISABLE", "true")
	t.Setenv("DASHBOARD_USER", "")
	t.Setenv("DASHBOARD_PASS", "")
	cfg := &config.BotConfig{Bot: config.BotSection{Mode: config.ModeLiveConfig}}

	got := buildAPIAuth(cfg, silentLg())
	if !got.AllowUnauthenticated {
		t.Fatalf("expected AllowUnauthenticated=true; got %+v", got)
	}
}

func TestBuildAPIAuth_DashboardAuthDisableEnvAllowsBypassInPaper(t *testing.T) {
	t.Setenv("DASHBOARD_AUTH_DISABLE", "true")
	t.Setenv("DASHBOARD_USER", "")
	t.Setenv("DASHBOARD_PASS", "")
	cfg := &config.BotConfig{Bot: config.BotSection{Mode: config.ModePaperConfig}}

	got := buildAPIAuth(cfg, silentLg())
	if !got.AllowUnauthenticated {
		t.Fatalf("expected AllowUnauthenticated=true; got %+v", got)
	}
}

func TestBuildAPIAuth_DashboardAuthDisableUnset_PaperRequiresCreds(t *testing.T) {
	// regression guard: env が立っていなければ paper でも creds 必須
	t.Setenv("DASHBOARD_AUTH_DISABLE", "")
	t.Setenv("DASHBOARD_USER", "admin")
	t.Setenv("DASHBOARD_PASS", "secret")
	cfg := &config.BotConfig{Bot: config.BotSection{Mode: config.ModePaperConfig}}

	got := buildAPIAuth(cfg, silentLg())
	if got.AllowUnauthenticated {
		t.Errorf("expected creds path; got AllowUnauthenticated=true")
	}
	if got.User != "admin" || got.Pass != "secret" {
		t.Errorf("creds: %+v", got)
	}
}

func TestBuildAPIAuth_DashboardAuthDisableFalseStringIsIgnored(t *testing.T) {
	// "false" や "0" を立てて bypass されてしまっては事故るので、
	// "true" (大文字小文字無視) のみを許容する。
	t.Setenv("DASHBOARD_AUTH_DISABLE", "false")
	t.Setenv("DASHBOARD_USER", "admin")
	t.Setenv("DASHBOARD_PASS", "secret")
	cfg := &config.BotConfig{Bot: config.BotSection{Mode: config.ModeLiveConfig}}

	got := buildAPIAuth(cfg, silentLg())
	if got.AllowUnauthenticated {
		t.Errorf("expected creds enforcement for DASHBOARD_AUTH_DISABLE=false")
	}
}

// DASHBOARD_ALLOWED_HOSTS(カンマ区切り)を API の Host 許可リストに渡す。ポートと空要素は無視する。
func TestBuildAllowedHosts(t *testing.T) {
	t.Setenv("DASHBOARD_ALLOWED_HOSTS", " my-mac.local:3000, ,Dash.Example.com ")
	got := buildAllowedHosts()
	if len(got) != 2 || got[0] != "my-mac.local" || got[1] != "dash.example.com" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("DASHBOARD_ALLOWED_HOSTS", "")
	if got := buildAllowedHosts(); len(got) != 0 {
		t.Fatalf("unset must be empty; got %q", got)
	}
}
