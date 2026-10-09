package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// BatchConfig declares a multi-symbol backtest run as a single YAML file.
// Loaded via -batch <path> on the CLI. Fields are merged onto cliFlags:
// non-empty BatchConfig fields override the corresponding CLI defaults,
// and CLI -format / -slice stay caller-controlled (they affect output
// rendering, not the simulation).
//
// Example file:
//
//	symbols: [USD_JPY, EUR_JPY]
//	period: 2026-02-01..2026-05-01
//	config: configs/strategy_config.active.yaml
//	slippage: 0.5
//	fee: 0
type BatchConfig struct {
	Symbols  []string `yaml:"symbols"`
	Period   string   `yaml:"period"` // "YYYY-MM-DD..YYYY-MM-DD"
	Config   string   `yaml:"config"`
	Slippage float64  `yaml:"slippage"`
	Fee      float64  `yaml:"fee"`
}

// LoadBatchConfig reads + parses a batch YAML.
func LoadBatchConfig(path string) (*BatchConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read batch: %w", err)
	}
	var b BatchConfig
	if err := yaml.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("parse batch: %w", err)
	}
	if len(b.Symbols) == 0 {
		return nil, fmt.Errorf("batch: symbols must be a non-empty list")
	}
	for i, s := range b.Symbols {
		if strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("batch: symbols[%d] is empty", i)
		}
	}
	if b.Period == "" {
		return nil, fmt.Errorf("batch: period is required (YYYY-MM-DD..YYYY-MM-DD)")
	}
	if _, _, err := splitPeriod(b.Period); err != nil {
		return nil, fmt.Errorf("batch: %w", err)
	}
	return &b, nil
}

// splitPeriod parses "FROM..TO" into the two date strings (validation only;
// callers run time.Parse on the returned strings).
func splitPeriod(p string) (string, string, error) {
	parts := strings.Split(p, "..")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("period %q must be FROM..TO (e.g. 2026-04-01..2026-05-01)", p)
	}
	if _, err := time.Parse("2006-01-02", parts[0]); err != nil {
		return "", "", fmt.Errorf("period from: %w", err)
	}
	if _, err := time.Parse("2006-01-02", parts[1]); err != nil {
		return "", "", fmt.Errorf("period to: %w", err)
	}
	return parts[0], parts[1], nil
}

// applyBatch overlays a BatchConfig onto cliFlags. CLI flags that were not
// explicitly set (= default values) are populated from the batch; explicit
// CLI flags always win. -format / -slice are caller-only and not touched.
//
// Convention: CLI default sentinels we replace:
//   - symbol == "USD_JPY" && symbols == ""  → batch wins
//   - configPath == ""                       → batch wins
//   - from == "" / to == ""                  → batch wins (split Period)
//   - slippage / fee == 0                    → batch wins
func applyBatch(f cliFlags, b *BatchConfig) (cliFlags, error) {
	out := f
	// Symbols always come from the batch (the whole point of the file).
	out.symbols = strings.Join(b.Symbols, ",")
	out.symbol = b.Symbols[0]
	if out.configPath == "" {
		out.configPath = b.Config
	}
	from, to, err := splitPeriod(b.Period)
	if err != nil {
		return out, err
	}
	if out.from == "" {
		out.from = from
	}
	if out.to == "" {
		out.to = to
	}
	if out.slippage == 0 {
		out.slippage = b.Slippage
	}
	if out.fee == 0 {
		out.fee = b.Fee
	}
	return out, nil
}
