// Package testutil holds small cross-package test helpers. Each _test.go
// used to redefine these (nearly: 3
// 箇所, silentLogger / tempFlag: 命名違いで 2-3 箇所)、集約することで
// 1 か所の更新で全 callsite に反映される。
//
// 慣行: ここに足すのは「ロジックを持たない / cross-package で使い回す」
// helpers のみ。package-local な mock / fake はそのファイル内 (例:
// command/fakes_test.go) に残す。
package testutil

import (
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"testing"
)

// Nearly は |a-b| < tolerance を返す。float64 比較で == を避けるための定型。
func Nearly(a, b, tolerance float64) bool {
	return math.Abs(a-b) < tolerance
}

// SilentLogger は test 用に出力を捨てる slog.Logger を返す。
// 各 test ファイルで io.Discard 経由の slog を再実装していたのを統一。
func SilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TempFlag は t.TempDir() 配下に emergency_stop.flag のパスを返す。
// 実ファイルは作らない (= safety.Active で false が返る初期状態)。
// safety.Trip 系のテストで「flag が書かれた / 書かれていない」を検証する callsite で使う。
func TempFlag(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "emergency_stop.flag")
}
