package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// /api/ask-claude はダッシュボード入力(外部から来る任意文)をそのまま claude -p に渡す。
// ツールが使えると、プロンプトインジェクションでファイル読取(.env の API キー)や
// コマンド実行に至り得るため、ツール無し(--tools "")・セッション非保存で起動する。
func TestNewClaudeRunner_DisablesAllTools(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake_claude.sh")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '[%s]' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := newClaudeRunner(fake, 10, slog.New(slog.NewTextHandler(io.Discard, nil)))

	out, err := run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	for _, want := range []string{"[-p]", "[--tools][]", "[--no-session-persistence]"} {
		if !strings.Contains(out, want) {
			t.Errorf("claude args %q missing %q", out, want)
		}
	}
}

// ダッシュボードの質問でも、bot の秘密(GMO の API キー等)を claude に渡さず、repo の hooks も走らせない。
func TestNewClaudeRunner_IsolatesEnvAndHooks(t *testing.T) {
	t.Setenv("GMO_API_SECRET", "must-not-leak")
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake_claude.sh")
	script := "#!/bin/sh\nprintf '[%s]' \"$@\"\nprintf 'SECRET=%s' \"$GMO_API_SECRET\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	run := newClaudeRunner(fake, 10, slog.New(slog.NewTextHandler(io.Discard, nil)))

	out, err := run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	if strings.Contains(out, "must-not-leak") {
		t.Errorf("GMO_API_SECRET leaked into the claude process: %q", out)
	}
	if !strings.Contains(out, `[--settings][{"disableAllHooks":true}]`) {
		t.Errorf("repo hooks must be disabled for ask-claude: %q", out)
	}
}
