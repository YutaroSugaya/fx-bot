// cmd/config-check validates strategy config YAML files the same way the bot validates its
// active config at startup (parse + ValidateStatic: schema / hard_limits / strategy whitelist
// derived from the engine registry). scripts/seed_active_config.sh runs it before seeding
// strategy_configs, and refuses to seed when any file fails.
//
// Usage (from backend/):
//
//	go run ./cmd/config-check -hard-limits ../configs/hard_limits.yaml ../configs/trend_v4_USD_JPY.yaml
//
// On success it prints one TSV line per file (config_id, symbol, strategy, regime type,
// regime confidence, valid_from, valid_until) and exits 0. If any file fails, nothing is
// printed to stdout, the reasons go to stderr and it exits 1. Read-only: no DB, no network.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/strategy"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config-check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	hardLimitsPath := fs.String("hard-limits", "../configs/hard_limits.yaml", "path to hard_limits.yaml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	files := fs.Args()
	if len(files) == 0 {
		fmt.Fprintln(stderr, "usage: config-check [-hard-limits path] <strategy_config.yaml>...")
		return 2
	}

	limits, err := config.LoadHardLimits(*hardLimitsPath)
	if err != nil {
		fmt.Fprintf(stderr, "load hard_limits: %v\n", err)
		return 1
	}
	validator := config.NewValidator(limits)
	allowed := allowedStrategies()

	var lines []string
	failed := false
	for _, path := range files {
		line, err := check(validator, allowed, path)
		if err != nil {
			fmt.Fprintf(stderr, "NG %s: %v\n", path, err)
			failed = true
			continue
		}
		lines = append(lines, line)
	}
	if failed {
		return 1
	}
	for _, l := range lines {
		fmt.Fprintln(stdout, l)
	}
	return 0
}

func check(v *config.Validator, allowed []string, path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	cfg, err := config.ParseStrategyConfig(raw)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	if res := v.ValidateStatic(cfg, allowed...); !res.OK() {
		return "", fmt.Errorf("validation: %s", strings.TrimSpace(res.Summary()))
	}
	return strings.Join([]string{
		cfg.ConfigID,
		cfg.Symbol,
		string(cfg.Strategy.Name),
		string(cfg.MarketRegime.Type),
		fmt.Sprintf("%.2f", cfg.MarketRegime.Confidence),
		cfg.ValidFrom.UTC().Format(time.RFC3339),
		cfg.ValidUntil.UTC().Format(time.RFC3339),
	}, "\t"), nil
}

// allowedStrategies mirrors the bot's startup whitelist: every strategy the engine can run.
func allowedStrategies() []string {
	names := strategy.NewEngine().RegisteredNames()
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, string(n))
	}
	return out
}
