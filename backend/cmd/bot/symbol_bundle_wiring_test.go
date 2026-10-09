package main

import "testing"

func TestWithSymbolSuffix(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		symbol string
		want   string
	}{
		{"json with dir", "runtime/ai_input/latest_summary.json", "USD_JPY",
			"runtime/ai_input/latest_summary_USD_JPY.json"},
		{"yaml in configs", "configs/strategy_config.next.yaml", "EUR_JPY",
			"configs/strategy_config.next_EUR_JPY.yaml"},
		{"no extension", "runtime/active", "USD_JPY", "runtime/active_USD_JPY"},
		{"empty path is pass-through", "", "USD_JPY", ""},
		{"empty symbol is pass-through", "x.yaml", "", "x.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := withSymbolSuffix(tc.path, tc.symbol); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveSymbolPaths_PrimaryOnlyKeepsLegacyPaths(t *testing.T) {
	paths := runtimePaths{
		Summary:      "runtime/ai_input/latest_summary.json",
		NextConfig:   "configs/strategy_config.next.yaml",
		ActiveConfig: "configs/strategy_config.active.yaml",
	}
	got := resolveSymbolPaths(paths, "USD_JPY", true)
	if got.Summary != paths.Summary || got.NextConfig != paths.NextConfig || got.ActiveConfig != paths.ActiveConfig {
		t.Errorf("primaryOnly should preserve unsuffixed paths: %+v", got)
	}
}

func TestResolveSymbolPaths_MultiSuffixesPaths(t *testing.T) {
	paths := runtimePaths{
		Summary:      "runtime/ai_input/latest_summary.json",
		NextConfig:   "configs/strategy_config.next.yaml",
		ActiveConfig: "configs/strategy_config.active.yaml",
	}
	got := resolveSymbolPaths(paths, "EUR_JPY", false)
	if got.Summary != "runtime/ai_input/latest_summary_EUR_JPY.json" {
		t.Errorf("Summary: %q", got.Summary)
	}
	if got.NextConfig != "configs/strategy_config.next_EUR_JPY.yaml" {
		t.Errorf("NextConfig: %q", got.NextConfig)
	}
	if got.ActiveConfig != "configs/strategy_config.active_EUR_JPY.yaml" {
		t.Errorf("ActiveConfig: %q", got.ActiveConfig)
	}
}

func TestAdvisorMaxConcurrent(t *testing.T) {
	cases := []struct {
		n    int
		want int
	}{
		{0, 0},  // empty / single bundle → no cap (errgroup.SetLimit not called)
		{1, 0},  // single bundle → no cap
		{2, 2},  // 2 bundles fully parallel
		{3, 2},  // 3 bundles → 2 concurrent
		{4, 2},  // 4 bundles → 2 concurrent (rounded-up half = 2)
		{5, 3},  // 5 bundles → 3 concurrent
		{10, 5}, // 10 → 5 concurrent
	}
	for _, tc := range cases {
		if got := advisorMaxConcurrent(tc.n); got != tc.want {
			t.Errorf("advisorMaxConcurrent(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
}
