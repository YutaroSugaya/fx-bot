package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestResolveSymbols(t *testing.T) {
	cases := []struct {
		name string
		in   cliFlags
		want []string
	}{
		{"legacy single symbol", cliFlags{symbol: "USD_JPY"}, []string{"USD_JPY"}},
		{"symbols CSV overrides", cliFlags{symbol: "USD_JPY", symbols: "USD_JPY,EUR_JPY"}, []string{"USD_JPY", "EUR_JPY"}},
		{"trims whitespace", cliFlags{symbols: " USD_JPY , EUR_JPY "}, []string{"USD_JPY", "EUR_JPY"}},
		{"empty entries skipped", cliFlags{symbols: "USD_JPY,,EUR_JPY,"}, []string{"USD_JPY", "EUR_JPY"}},
		{"empty symbols falls back to -symbol", cliFlags{symbol: "EUR_JPY", symbols: ""}, []string{"EUR_JPY"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.resolveSymbols(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

// TestRun_RejectsEmptyConfigPath: smoke test for argument validation. The CLI
// must complain (not panic) when -config is missing.
func TestRun_RejectsEmptyConfigPath(t *testing.T) {
	err := run(cliFlags{from: "2026-04-01", to: "2026-05-01", symbol: "USD_JPY"})
	if err == nil || !strings.Contains(err.Error(), "-config") {
		t.Fatalf("expected -config error, got %v", err)
	}
}

func TestRun_RejectsBadDateRange(t *testing.T) {
	err := run(cliFlags{configPath: "x", from: "2026-05-01", to: "2026-04-01"})
	if err == nil || !strings.Contains(err.Error(), "must be after") {
		t.Fatalf("expected date range error, got %v", err)
	}
}

func TestRun_RejectsMalformedDate(t *testing.T) {
	err := run(cliFlags{configPath: "x", from: "not-a-date", to: "2026-05-01"})
	if err == nil || !strings.Contains(err.Error(), "-from") {
		t.Fatalf("expected -from parse error, got %v", err)
	}
}
