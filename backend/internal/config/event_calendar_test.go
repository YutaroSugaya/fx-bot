package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// EventCalendar は経済指標 freeze 窓を保持する。
// 手書きの configs/event_calendar.yaml をロードし、現在時刻が freeze 窓内か
// を返す純粋関数 InFreezeWindow を提供する。
//
// 仕様:
//   - 各 event は (at, pre_minutes, post_minutes) で表現
//   - freeze 窓 = [at - pre_minutes, at + post_minutes]
//   - at, pre, post すべて必須 (load 時に validate)
//   - 任意の event がマッチしたら freeze

func TestEventCalendar_InFreezeWindow(t *testing.T) {
	base := time.Date(2026, 6, 5, 21, 30, 0, 0, time.UTC)
	cal := &EventCalendar{
		Events: []CalendarEvent{
			{
				Name:        "US NFP test",
				At:          base, // 21:30 UTC = 06:30 JST 翌日
				PreMinutes:  30,
				PostMinutes: 30,
			},
		},
	}
	cases := []struct {
		name      string
		now       time.Time
		wantFreze bool
	}{
		{"before pre-window", base.Add(-31 * time.Minute), false},
		{"at pre-edge (-30min)", base.Add(-30 * time.Minute), true},
		{"during freeze", base, true},
		{"at post-edge (+30min)", base.Add(30 * time.Minute), true},
		{"after post-window (+31min)", base.Add(31 * time.Minute), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cal.InFreezeWindow(c.now); got != c.wantFreze {
				t.Errorf("InFreezeWindow(%v) = %v, want %v", c.now, got, c.wantFreze)
			}
		})
	}
}

// 複数 event 共存: いずれか 1 つでも窓内なら freeze。
func TestEventCalendar_InFreezeWindow_AnyMatch(t *testing.T) {
	t0 := time.Date(2026, 6, 5, 21, 30, 0, 0, time.UTC)
	t1 := time.Date(2026, 6, 18, 18, 0, 0, 0, time.UTC) // FOMC
	cal := &EventCalendar{
		Events: []CalendarEvent{
			{Name: "NFP", At: t0, PreMinutes: 30, PostMinutes: 30},
			{Name: "FOMC", At: t1, PreMinutes: 15, PostMinutes: 90},
		},
	}
	if !cal.InFreezeWindow(t1.Add(45 * time.Minute)) {
		t.Errorf("FOMC + 45min should be inside post-window 90min")
	}
	// どちらの window にも入らない時刻
	if cal.InFreezeWindow(time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("midweek noon should not be in any freeze window")
	}
}

// nil / 空 events → freeze なし。
func TestEventCalendar_NilOrEmpty(t *testing.T) {
	if (*EventCalendar)(nil).InFreezeWindow(time.Now()) {
		t.Errorf("nil calendar must report no freeze")
	}
	cal := &EventCalendar{}
	if cal.InFreezeWindow(time.Now()) {
		t.Errorf("empty calendar must report no freeze")
	}
}

// policy:breakout のイベントは freeze しない (攻めモード)。
// freeze するのは policy:freeze (または未指定=既定 freeze) のみ。
func TestEventCalendar_InFreezeWindow_BreakoutPolicyDoesNotFreeze(t *testing.T) {
	at := time.Date(2026, 6, 5, 13, 0, 0, 0, time.UTC) // 22:00 JST
	cal := &EventCalendar{
		Events: []CalendarEvent{
			{Name: "US data breakout", At: at, PreMinutes: 30, PostMinutes: 30, Policy: "breakout"},
		},
	}
	// 窓のど真ん中でも breakout policy は freeze しない。
	if cal.InFreezeWindow(at) {
		t.Errorf("breakout-policy event must NOT freeze")
	}
	// 未指定 policy は従来通り freeze する (back-compat)。
	cal2 := &EventCalendar{Events: []CalendarEvent{{Name: "x", At: at, PreMinutes: 30, PostMinutes: 30}}}
	if !cal2.InFreezeWindow(at) {
		t.Errorf("empty policy (default freeze) must still freeze")
	}
}

// ActiveOrUpcoming: 窓内なら in_window=true、窓前なら minutes_until>0、
// horizon 外なら ok=false。prompt の event_context と gate の両方が使う。
func TestEventCalendar_ActiveOrUpcoming(t *testing.T) {
	at := time.Date(2026, 6, 5, 13, 0, 0, 0, time.UTC) // 22:00 JST
	cal := &EventCalendar{
		Events: []CalendarEvent{
			{Name: "NFP", At: at, PreMinutes: 30, PostMinutes: 30, Policy: "breakout"},
		},
	}
	horizon := 60 * time.Minute

	// 25 分前 = pre 窓 (30分) 内 → in_window=true, minutes_until=25
	if ev, mins, inWin, ok := cal.ActiveOrUpcoming(at.Add(-25*time.Minute), horizon); !ok || !inWin || ev.Name != "NFP" || mins != 25 {
		t.Errorf("25min before: ok=%v inWin=%v mins=%d ev=%v", ok, inWin, mins, ev)
	}
	// 45 分前 = 窓外だが horizon(60) 内 → upcoming, in_window=false, minutes_until=45
	if ev, mins, inWin, ok := cal.ActiveOrUpcoming(at.Add(-45*time.Minute), horizon); !ok || inWin || mins != 45 || ev == nil {
		t.Errorf("45min before: ok=%v inWin=%v mins=%d", ok, inWin, mins)
	}
	// 90 分前 = horizon 外 → ok=false
	if _, _, _, ok := cal.ActiveOrUpcoming(at.Add(-90*time.Minute), horizon); ok {
		t.Errorf("90min before should be outside horizon")
	}
}

func TestLoadEventCalendar(t *testing.T) {
	yaml := `
events:
  - name: "US NFP"
    at: "2026-06-05T21:30:00Z"
    pre_minutes: 30
    post_minutes: 30
  - name: "FOMC"
    at: "2026-06-18T18:00:00Z"
    pre_minutes: 15
    post_minutes: 90
`
	path := filepath.Join(t.TempDir(), "event_calendar.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	cal, err := LoadEventCalendar(path)
	if err != nil {
		t.Fatalf("LoadEventCalendar: %v", err)
	}
	if len(cal.Events) != 2 {
		t.Fatalf("events: got %d want 2", len(cal.Events))
	}
	if cal.Events[0].Name != "US NFP" {
		t.Errorf("name: got %q", cal.Events[0].Name)
	}
	if cal.Events[1].PostMinutes != 90 {
		t.Errorf("post_minutes: got %d", cal.Events[1].PostMinutes)
	}
}

// ファイルなし → 空カレンダー + nil error (= freeze 無効化)。
// 月 1 メンテで運用者がファイルを忘れた場合も bot を止めないため、空扱い。
func TestLoadEventCalendar_MissingFile(t *testing.T) {
	cal, err := LoadEventCalendar(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Errorf("missing file should yield empty calendar + nil err: %v", err)
	}
	if cal == nil || len(cal.Events) != 0 {
		t.Errorf("expected empty calendar, got %+v", cal)
	}
}

func TestLoadEventCalendar_InvalidYAML_Rejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("events: not a list"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadEventCalendar(path); err == nil {
		t.Errorf("invalid YAML should be rejected")
	}
}
