package config

import (
	"testing"
	"time"
)

// LoadTimezoneOrUTC は IANA tz 名から *time.Location を返す。
// 不正な name や空文字なら UTC に fallback する。
// 3 箇所 (worker.go ×2 + main.go) で重複していたパターンの統合先。

func TestLoadTimezoneOrUTC_ValidName(t *testing.T) {
	loc := LoadTimezoneOrUTC("Asia/Tokyo")
	if loc == nil {
		t.Fatal("nil location")
	}
	if loc.String() != "Asia/Tokyo" {
		t.Errorf("got %s, want Asia/Tokyo", loc.String())
	}
}

func TestLoadTimezoneOrUTC_EmptyFallsBackToUTC(t *testing.T) {
	loc := LoadTimezoneOrUTC("")
	if loc == nil {
		t.Fatal("nil location")
	}
	if loc != time.UTC {
		t.Errorf("got %s, want UTC", loc.String())
	}
}

func TestLoadTimezoneOrUTC_InvalidFallsBackToUTC(t *testing.T) {
	loc := LoadTimezoneOrUTC("Not/A_Zone")
	if loc == nil {
		t.Fatal("nil location")
	}
	if loc != time.UTC {
		t.Errorf("got %s, want UTC", loc.String())
	}
}

// fallback location should be usable for time.Now().In(loc) without panicking.
func TestLoadTimezoneOrUTC_FallbackIsUsable(t *testing.T) {
	loc := LoadTimezoneOrUTC("garbage")
	now := time.Now().In(loc)
	_ = now.Format(time.RFC3339)
}

// StartOfDayIn は t の「tz から見たカレンダー日」の 0:00 を返す。
// worker / promotion_state / entry_admission の 4 箇所で重複していた
// "daily window 起点を tz 0:00 に揃える" パターンの統合先。
func TestStartOfDayIn_TokyoCrossesUTCDateBoundary(t *testing.T) {
	// 2026-06-09 20:00 UTC == 2026-06-10 05:00 JST → JST 日の起点は 06-10 00:00 JST。
	in := time.Date(2026, 6, 9, 20, 0, 0, 0, time.UTC)
	got := StartOfDayIn(in, "Asia/Tokyo")
	jst := LoadTimezoneOrUTC("Asia/Tokyo")
	want := time.Date(2026, 6, 10, 0, 0, 0, 0, jst)
	if !got.Equal(want) {
		t.Errorf("instant: got %s, want %s", got, want)
	}
	if got.Location().String() != "Asia/Tokyo" {
		t.Errorf("loc = %s, want Asia/Tokyo", got.Location())
	}
	if h, m, s := got.Clock(); h != 0 || m != 0 || s != 0 {
		t.Errorf("clock = %02d:%02d:%02d, want 00:00:00", h, m, s)
	}
}

func TestStartOfDayIn_EmptyNameUsesUTC(t *testing.T) {
	in := time.Date(2026, 6, 10, 15, 30, 45, 0, time.UTC)
	got := StartOfDayIn(in, "")
	want := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("got %s (loc %s), want %s UTC", got, got.Location(), want)
	}
}

func TestStartOfDayIn_InvalidNameFallsBackToUTC(t *testing.T) {
	in := time.Date(2026, 6, 10, 15, 30, 0, 0, time.UTC)
	got := StartOfDayIn(in, "Not/A_Zone")
	want := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %s, want %s", got, want)
	}
}
