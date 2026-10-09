package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBatch(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "batch.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestLoadBatchConfig_ParsesValidYAML(t *testing.T) {
	body := `
symbols:
  - USD_JPY
  - EUR_JPY
period: 2026-04-01..2026-05-01
config: configs/strategy_config.active.yaml
slippage: 0.5
fee: 0
`
	b, err := LoadBatchConfig(writeBatch(t, body))
	if err != nil {
		t.Fatalf("LoadBatchConfig: %v", err)
	}
	if len(b.Symbols) != 2 || b.Symbols[0] != "USD_JPY" || b.Symbols[1] != "EUR_JPY" {
		t.Errorf("Symbols: %+v", b.Symbols)
	}
	if b.Config != "configs/strategy_config.active.yaml" {
		t.Errorf("Config: %q", b.Config)
	}
	if b.Slippage != 0.5 {
		t.Errorf("Slippage: %v", b.Slippage)
	}
}

func TestLoadBatchConfig_RejectsBadPeriod(t *testing.T) {
	body := `
symbols: [USD_JPY]
period: garbage
`
	_, err := LoadBatchConfig(writeBatch(t, body))
	if err == nil || !strings.Contains(err.Error(), "period") {
		t.Fatalf("expected period error, got %v", err)
	}
}

func TestLoadBatchConfig_RejectsMissingSymbols(t *testing.T) {
	body := `
period: 2026-04-01..2026-05-01
`
	_, err := LoadBatchConfig(writeBatch(t, body))
	if err == nil || !strings.Contains(err.Error(), "symbols") {
		t.Fatalf("expected symbols error, got %v", err)
	}
}

func TestApplyBatch_BatchPopulatesEmptyCLI(t *testing.T) {
	b := &BatchConfig{
		Symbols:  []string{"USD_JPY", "EUR_JPY"},
		Period:   "2026-04-01..2026-05-01",
		Config:   "configs/strategy_config.active.yaml",
		Slippage: 0.5,
		Fee:      2,
	}
	got, err := applyBatch(cliFlags{format: "pretty"}, b)
	if err != nil {
		t.Fatalf("applyBatch: %v", err)
	}
	if got.symbols != "USD_JPY,EUR_JPY" {
		t.Errorf("symbols: %q", got.symbols)
	}
	if got.symbol != "USD_JPY" {
		t.Errorf("primary symbol: %q", got.symbol)
	}
	if got.configPath != "configs/strategy_config.active.yaml" {
		t.Errorf("configPath: %q", got.configPath)
	}
	if got.from != "2026-04-01" || got.to != "2026-05-01" {
		t.Errorf("dates: %q..%q", got.from, got.to)
	}
	if got.slippage != 0.5 || got.fee != 2 {
		t.Errorf("costs: slip=%v fee=%v", got.slippage, got.fee)
	}
	if got.format != "pretty" {
		t.Errorf("format must stay caller-controlled: %q", got.format)
	}
}

func TestApplyBatch_ExplicitCLIOverridesBatch(t *testing.T) {
	// CLI -from / -to / -slippage were explicit → batch values do NOT
	// overwrite them. Symbols are always batch (it's the file's purpose).
	b := &BatchConfig{
		Symbols:  []string{"USD_JPY"},
		Period:   "2026-04-01..2026-05-01",
		Slippage: 0.5,
	}
	in := cliFlags{from: "2026-03-01", to: "2026-04-01", slippage: 1.0, configPath: "x.yaml"}
	got, err := applyBatch(in, b)
	if err != nil {
		t.Fatalf("applyBatch: %v", err)
	}
	if got.from != "2026-03-01" || got.to != "2026-04-01" {
		t.Errorf("CLI dates should win: %q..%q", got.from, got.to)
	}
	if got.slippage != 1.0 {
		t.Errorf("CLI slippage should win: %v", got.slippage)
	}
	if got.configPath != "x.yaml" {
		t.Errorf("CLI config should win: %q", got.configPath)
	}
}
