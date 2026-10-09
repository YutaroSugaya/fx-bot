package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const hardLimitsPath = "../../../configs/hard_limits.yaml"

// 起動時に bot が active config に掛けるのと同じ検証(parse + ValidateStatic + 戦略 whitelist)を
// seed の前に掛ける。通った config は seed に必要な列を TSV で 1 行ずつ出す。
func TestRun_ValidConfigPrintsSeedFields(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"-hard-limits", hardLimitsPath, "../../../configs/trend_v4_USD_JPY.yaml"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut.String())
	}
	want := "trend-v4-usdjpy\tUSD_JPY\ttrend_follow\tunclear\t0.70\t2026-06-15T00:00:00Z\t2030-12-31T23:59:59Z\n"
	if out.String() != want {
		t.Fatalf("stdout:\n got %q\nwant %q", out.String(), want)
	}
}

func TestRun_InvalidConfigFailsAndNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	raw, err := os.ReadFile("../../../configs/trend_v4_USD_JPY.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// allowed_symbols に無いシンボル → hard_limit で落ちる。
	if err := os.WriteFile(bad, bytes.Replace(raw, []byte("symbol: USD_JPY"), []byte("symbol: XAU_JPY"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run([]string{"-hard-limits", hardLimitsPath, "../../../configs/trend_v4_USD_JPY.yaml", bad}, &out, &errOut)
	if code == 0 {
		t.Fatalf("an invalid config must fail; stdout: %s", out.String())
	}
	if !strings.Contains(errOut.String(), "bad.yaml") {
		t.Errorf("stderr must name the failing file; got %q", errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("nothing may be printed for seeding when any file fails; got %q", out.String())
	}
}

func TestRun_UnknownStrategyFails(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "unknown.yaml")
	raw, err := os.ReadFile("../../../configs/trend_v4_USD_JPY.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, bytes.Replace(raw, []byte(`name: "trend_follow"`), []byte(`name: "not_a_strategy"`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"-hard-limits", hardLimitsPath, bad}, &out, &errOut); code == 0 {
		t.Fatal("a strategy the engine cannot run must fail")
	}
}

func TestRun_RequiresAtLeastOneFile(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"-hard-limits", hardLimitsPath}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2 (usage)", code)
	}
}
