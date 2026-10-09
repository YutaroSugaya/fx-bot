package safety

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestActive は flag path の 3 状態を table-driven で検証。
func TestActive(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(t *testing.T) string // returns path
		wantActive bool
	}{
		{
			name:       "empty path is never active",
			setup:      func(*testing.T) string { return "" },
			wantActive: false,
		},
		{
			name: "missing file is not active",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "no_such_file")
			},
			wantActive: false,
		},
		{
			name: "existing file is active",
			setup: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "flag")
				if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
					t.Fatalf("setup: %v", err)
				}
				return p
			},
			wantActive: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.setup(t)
			if got := Active(path); got != tc.wantActive {
				t.Errorf("Active(%q) = %v want %v", path, got, tc.wantActive)
			}
		})
	}
}

// TestTrip は Trip / TripWithDetail / overwrite を table-driven で検証。
func TestTrip(t *testing.T) {
	cases := []struct {
		name string
		// applyAt は path を取って Trip 系を順に実行する。テスト後に
		// path を返し、その body が wantContains を全て含むことを確認する。
		applyAt             func(t *testing.T, path string)
		emptyPath           bool // true なら path="" を渡す
		wantContains        []string
		wantNotContains     []string
		wantBodyStartsDigit bool // RFC3339 starts with year digit
	}{
		{
			name:                "Trip writes reason with RFC3339 prefix",
			applyAt:             func(t *testing.T, p string) { _ = Trip(p, "reconcile_naked_position") },
			wantContains:        []string{"reconcile_naked_position"},
			wantBodyStartsDigit: true,
		},
		{
			name:         "TripWithDetail includes detail and reason",
			applyAt:      func(t *testing.T, p string) { _ = TripWithDetail(p, "resolve_failed", "order=abc-123 cause=timeout") },
			wantContains: []string{"resolve_failed", "order=abc-123"},
		},
		{
			name: "Trip overwrites earlier reason",
			applyAt: func(t *testing.T, p string) {
				_ = Trip(p, "first")
				_ = Trip(p, "second")
			},
			wantContains:    []string{"second"},
			wantNotContains: []string{"first"},
		},
		{
			name:      "Trip with empty path is a no-op (returns nil)",
			applyAt:   func(t *testing.T, p string) {}, // no-op asserted via no error path; helper below verifies returns
			emptyPath: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var path string
			if !tc.emptyPath {
				path = filepath.Join(t.TempDir(), "flag")
			}
			tc.applyAt(t, path)
			if tc.emptyPath {
				return // nothing to read; nil return verified separately by helpers
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(string(body), want) {
					t.Errorf("body should contain %q; got %q", want, body)
				}
			}
			for _, dontWant := range tc.wantNotContains {
				if strings.Contains(string(body), dontWant) {
					t.Errorf("body should NOT contain %q; got %q", dontWant, body)
				}
			}
			if tc.wantBodyStartsDigit && (len(body) == 0 || body[0] < '0' || body[0] > '9') {
				t.Errorf("body should start with RFC3339 digit; got %q", body)
			}
		})
	}
}

// Trip empty path: return nil — verified independently because table cases
// cannot assert the return value cleanly with `applyAt`.
func TestTrip_EmptyPathReturnsNil(t *testing.T) {
	if err := Trip("", "anything"); err != nil {
		t.Errorf("empty path should return nil, got %v", err)
	}
	if err := TripWithDetail("", "any", "detail"); err != nil {
		t.Errorf("empty path TripWithDetail should return nil, got %v", err)
	}
}

// Callback (SetOnTrip) は global state を触るため独立テストで残す。
// table 化するとケース間の defer SetOnTrip(nil) の依存が見えにくくなる。

func TestTrip_FiresOnTripCallback(t *testing.T) {
	var got string
	SetOnTrip(func(reason string) { got = reason })
	defer SetOnTrip(nil)

	path := filepath.Join(t.TempDir(), "flag")
	_ = Trip(path, "manual_test")
	if got != "manual_test" {
		t.Errorf("callback should fire with reason; got %q", got)
	}
}

func TestTrip_EmptyPathStillFiresCallback(t *testing.T) {
	var calls int
	SetOnTrip(func(reason string) { calls++ })
	defer SetOnTrip(nil)

	_ = Trip("", "noop_path")
	if calls != 1 {
		t.Errorf("callback should fire even when path is empty; got %d calls", calls)
	}
}

func TestSetOnTrip_NilUnregisters(t *testing.T) {
	calls := 0
	SetOnTrip(func(string) { calls++ })
	SetOnTrip(nil)

	_ = Trip("", "should_not_fire")
	if calls != 0 {
		t.Errorf("unregistered callback should not fire; got %d calls", calls)
	}
}
