package main

import (
	"path/filepath"
	"testing"
)

// make start は bot を backend/ で起動し、設定ファイルのパスを ../configs/… で渡す。
// EVENT_CALENDAR_PATH を渡さないとき、カレンダーは bot_config と同じディレクトリから読む
// (repo root 相対の固定値だと backend/configs/… を探して 0 件のまま黙って動く)。
func TestLoadRuntimePaths_EventCalendarDefaultsNextToBotConfig(t *testing.T) {
	t.Setenv("BOT_CONFIG_PATH", "../configs/bot_config.yaml")
	t.Setenv("EVENT_CALENDAR_PATH", "")

	p := loadRuntimePaths()
	if want := filepath.Join("..", "configs", "event_calendar.yaml"); p.EventCalendar != want {
		t.Fatalf("EventCalendar = %q, want %q", p.EventCalendar, want)
	}
}

func TestLoadRuntimePaths_EventCalendarEnvWins(t *testing.T) {
	t.Setenv("BOT_CONFIG_PATH", "../configs/bot_config.yaml")
	t.Setenv("EVENT_CALENDAR_PATH", "/tmp/my_calendar.yaml")

	if p := loadRuntimePaths(); p.EventCalendar != "/tmp/my_calendar.yaml" {
		t.Fatalf("EventCalendar = %q, want the env value", p.EventCalendar)
	}
}
