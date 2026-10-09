package testutil

import (
	"path/filepath"
	"testing"
)

// 各 _test.go で再実装しがちな小ヘルパを集約する。

func TestNearly(t *testing.T) {
	if !Nearly(1.0000, 1.0001, 0.001) {
		t.Errorf("1.0000 vs 1.0001 within 0.001 should be Nearly")
	}
	if Nearly(1.0, 1.5, 0.01) {
		t.Errorf("1.0 vs 1.5 within 0.01 should NOT be Nearly")
	}
	if !Nearly(-2.0, -2.000001, 1e-5) {
		t.Errorf("negative close: should be Nearly")
	}
}

func TestSilentLogger_IsUsable(t *testing.T) {
	lg := SilentLogger()
	if lg == nil {
		t.Fatal("SilentLogger returned nil")
	}
	// 副作用ないこと: panic しないだけ確認
	lg.Info("test", "key", "value")
}

func TestTempFlag(t *testing.T) {
	p := TempFlag(t)
	if p == "" {
		t.Fatal("TempFlag returned empty path")
	}
	if filepath.Base(p) != "emergency_stop.flag" {
		t.Errorf("base: got %q want emergency_stop.flag", filepath.Base(p))
	}
}
