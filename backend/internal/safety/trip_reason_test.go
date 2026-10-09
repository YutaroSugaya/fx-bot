package safety

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// safety.Trip の reason は TripReason 型で型安全にする。よく使う reason を定数化し
// grep 容易性を上げる。ad-hoc 文字列も TripReason("...") キャストで受ける。

func TestTripReason_String(t *testing.T) {
	if got := ReasonCandleRestoreFailed.String(); got != "candle_restore_failed" {
		t.Errorf("got %q", got)
	}
	if got := TripReason("custom").String(); got != "custom" {
		t.Errorf("custom: got %q", got)
	}
}

func TestTripFor_WritesReasonToFlag(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emer.flag")
	if err := TripFor(flag, ReasonManualViaAPI); err != nil {
		t.Fatalf("TripFor: %v", err)
	}
	body, err := os.ReadFile(flag)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "manual_via_api") {
		t.Errorf("flag body should include reason; got %q", body)
	}
}

func TestTripForWithDetail_IncludesDetail(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "emer.flag")
	if err := TripForWithDetail(flag, ReasonManualViaAPI, "broker_id=abc123"); err != nil {
		t.Fatalf("TripForWithDetail: %v", err)
	}
	body, _ := os.ReadFile(flag)
	if !strings.Contains(string(body), "manual_via_api") {
		t.Errorf("missing reason; got %q", body)
	}
	if !strings.Contains(string(body), "broker_id=abc123") {
		t.Errorf("missing detail; got %q", body)
	}
}

func TestTripFor_EmptyPathIsNoop(t *testing.T) {
	if err := TripFor("", ReasonManualViaAPI); err != nil {
		t.Errorf("empty path should be no-op; got err=%v", err)
	}
}

func TestKnownTripReasons_AreSnakeCase(t *testing.T) {
	// regression guard: 全 well-known TripReason は snake_case ("reconcile:" 等の
	// プレフィックス記号を含めない)。アドホック concat はせず Source_Kind を
	// 1 つの定数で表現する慣行を守る。
	for _, r := range knownReasonsForTest() {
		s := string(r)
		if s == "" {
			t.Errorf("empty reason constant")
		}
		if strings.ContainsAny(s, ": \t/") {
			t.Errorf("reason %q contains non-snake-case chars", s)
		}
	}
}

// knownReasonsForTest は test helper として well-known 一覧を返す。
// 本体側 (trip_reason.go) で定義しているので keep in sync。
func knownReasonsForTest() []TripReason {
	return []TripReason{
		ReasonCandleRestoreFailed,
		ReasonManualViaAPI,
	}
}
