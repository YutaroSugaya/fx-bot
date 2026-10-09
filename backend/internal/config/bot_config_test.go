package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validBotConfigYAML = `
bot:
  mode: paper_config
  timezone: Asia/Tokyo
symbol: USD_JPY
ai_advisor:
  enabled: true
  provider: claude_cli
  interval_minutes: 60
  weekdays_only: true
  claude_cli_timeout_seconds: 120
  config_ttl_minutes: 90
  prompt_path: prompts/generate_strategy_config.md
  input_path: runtime/ai_input/latest_summary.json
  output_path: configs/strategy_config.next.yaml
risk:
  max_daily_loss_jpy: 8000
  max_consecutive_losses: 5
  max_open_positions: 1
gmo:
  public_base_url: https://forex-api.coin.z.com/public
  private_base_url: https://forex-api.coin.z.com/private
  private_get_limit_per_sec: 6
  private_post_limit_per_sec: 1
orders:
  prefer_order_type: MARKET_THEN_OCO
  fallback_order_type: ""
scheduler:
  weekdays_only: true
  start_day: MONDAY
  start_time: "07:00"
  end_day: SATURDAY
  end_time: "06:00"
`

func writeTempYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "bot_config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp yaml: %v", err)
	}
	return path
}

func TestLoadBotConfig_OK(t *testing.T) {
	path := writeTempYAML(t, validBotConfigYAML)
	cfg, err := LoadBotConfig(path)
	if err != nil {
		t.Fatalf("LoadBotConfig: %v", err)
	}
	if cfg.Bot.Mode != ModePaperConfig {
		t.Errorf("mode: got %q, want paper_config", cfg.Bot.Mode)
	}
	if cfg.Symbol != "USD_JPY" {
		t.Errorf("symbol: got %q, want USD_JPY", cfg.Symbol)
	}
	if cfg.AIAdvisor.IntervalMinutes != 60 {
		t.Errorf("interval_minutes: got %d, want 60", cfg.AIAdvisor.IntervalMinutes)
	}
	if cfg.AIAdvisor.PromptPath == "" {
		t.Errorf("prompt_path should be set")
	}
	if cfg.GMO.PrivateGetLimitPerSec != 6 {
		t.Errorf("private_get_limit_per_sec: got %d, want 6", cfg.GMO.PrivateGetLimitPerSec)
	}
}

func TestLoadBotConfig_InvalidMode(t *testing.T) {
	bad := strings.Replace(validBotConfigYAML, "mode: paper_config", "mode: garbage", 1)
	path := writeTempYAML(t, bad)
	_, err := LoadBotConfig(path)
	if err == nil {
		t.Fatalf("expected error for invalid mode")
	}
	if !strings.Contains(err.Error(), "bot.mode") {
		t.Errorf("error should mention bot.mode: %v", err)
	}
}

func TestLoadBotConfig_MissingSymbol(t *testing.T) {
	bad := strings.Replace(validBotConfigYAML, "symbol: USD_JPY", "symbol: \"\"", 1)
	path := writeTempYAML(t, bad)
	_, err := LoadBotConfig(path)
	if err == nil || !strings.Contains(err.Error(), "symbol") {
		t.Fatalf("expected symbol error, got %v", err)
	}
}

func TestLoadBotConfig_MissingPaths(t *testing.T) {
	bad := strings.Replace(validBotConfigYAML, "prompt_path: prompts/generate_strategy_config.md", "prompt_path: \"\"", 1)
	path := writeTempYAML(t, bad)
	_, err := LoadBotConfig(path)
	if err == nil || !strings.Contains(err.Error(), "ai_advisor paths") {
		t.Fatalf("expected ai_advisor paths error, got %v", err)
	}
}

func TestLoadBotConfig_NegativeInterval(t *testing.T) {
	bad := strings.Replace(validBotConfigYAML, "interval_minutes: 60", "interval_minutes: -1", 1)
	path := writeTempYAML(t, bad)
	_, err := LoadBotConfig(path)
	if err == nil || !strings.Contains(err.Error(), "interval_minutes") {
		t.Fatalf("expected interval_minutes error, got %v", err)
	}
}

func TestLoadBotConfig_ZeroTTL(t *testing.T) {
	bad := strings.Replace(validBotConfigYAML, "config_ttl_minutes: 90", "config_ttl_minutes: 0", 1)
	path := writeTempYAML(t, bad)
	_, err := LoadBotConfig(path)
	if err == nil || !strings.Contains(err.Error(), "config_ttl_minutes") {
		t.Fatalf("expected config_ttl_minutes error, got %v", err)
	}
}

func TestLoadBotConfig_FileNotFound(t *testing.T) {
	_, err := LoadBotConfig("/tmp/this-does-not-exist-fx-bot.yaml")
	if err == nil || !strings.Contains(err.Error(), "read bot_config") {
		t.Fatalf("expected read error, got %v", err)
	}
}

func TestLoadBotConfig_BadYAML(t *testing.T) {
	path := writeTempYAML(t, "bot:\n  mode: paper_config\n  bad indent\n")
	_, err := LoadBotConfig(path)
	if err == nil || !strings.Contains(err.Error(), "parse bot_config") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

// --- multi-symbol ---

// validBotConfigSymbolsYAML is validBotConfigYAML with the legacy single-symbol
// line swapped for a 2-symbol array + account-wide risk caps.
func validBotConfigSymbolsYAML(t *testing.T) string {
	t.Helper()
	out := strings.Replace(validBotConfigYAML,
		"symbol: USD_JPY",
		"symbols:\n  - USD_JPY\n  - EUR_JPY", 1)
	out = strings.Replace(out,
		"  max_open_positions: 1",
		"  max_open_positions: 1\n  account_max_open_positions: 2\n  account_max_daily_loss_jpy: 12000", 1)
	return out
}

func TestLoadBotConfig_SymbolsArrayLoadsAndBackfills(t *testing.T) {
	path := writeTempYAML(t, validBotConfigSymbolsYAML(t))
	cfg, err := LoadBotConfig(path)
	if err != nil {
		t.Fatalf("LoadBotConfig: %v", err)
	}

	wantSymbols := []string{"USD_JPY", "EUR_JPY"}
	got := cfg.ResolveSymbols()
	if !reflect.DeepEqual(got, wantSymbols) {
		t.Errorf("ResolveSymbols: got %v, want %v", got, wantSymbols)
	}
	// Normalize backfills legacy Symbol from Symbols[0].
	if cfg.Symbol != "USD_JPY" {
		t.Errorf("backfilled Symbol: got %q, want USD_JPY", cfg.Symbol)
	}
	if cfg.Risk.AccountMaxOpenPositions != 2 || cfg.Risk.AccountMaxDailyLossJPY != 12000 {
		t.Errorf("account-wide risk: got (%d, %d), want (2, 12000)",
			cfg.Risk.AccountMaxOpenPositions, cfg.Risk.AccountMaxDailyLossJPY)
	}
}

func TestLoadBotConfig_LegacySymbolStillLoads(t *testing.T) {
	// 旧 schema (symbol: USD_JPY、account_* 無し) も読めること。
	path := writeTempYAML(t, validBotConfigYAML)
	cfg, err := LoadBotConfig(path)
	if err != nil {
		t.Fatalf("LoadBotConfig: %v", err)
	}
	if got := cfg.ResolveSymbols(); !reflect.DeepEqual(got, []string{"USD_JPY"}) {
		t.Errorf("ResolveSymbols fallback: got %v", got)
	}
	if cfg.Risk.AccountMaxOpenPositions != 0 || cfg.Risk.AccountMaxDailyLossJPY != 0 {
		t.Errorf("account-wide risk omitted should default to 0, got (%d, %d)",
			cfg.Risk.AccountMaxOpenPositions, cfg.Risk.AccountMaxDailyLossJPY)
	}
}

func TestLoadBotConfig_SymbolsValidationErrors(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(string) string
		wantErrFrag string
	}{
		{
			name:        "missing both symbol and symbols",
			mutate:      func(s string) string { return strings.Replace(s, "symbol: USD_JPY", "", 1) },
			wantErrFrag: "symbol",
		},
		{
			name: "symbols array contains empty entry",
			mutate: func(string) string {
				return strings.Replace(validBotConfigSymbolsYAML(t), "  - EUR_JPY", `  - ""`, 1)
			},
			wantErrFrag: "empty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempYAML(t, tc.mutate(validBotConfigYAML))
			_, err := LoadBotConfig(path)
			if err == nil || !strings.Contains(err.Error(), tc.wantErrFrag) {
				t.Fatalf("want error containing %q, got %v", tc.wantErrFrag, err)
			}
		})
	}
}

func TestBotConfig_Normalize(t *testing.T) {
	cases := []struct {
		name        string
		in          BotConfig
		wantSymbol  string
		wantSymbols []string
	}{
		{"symbols → symbol",
			BotConfig{Symbols: []string{"EUR_JPY", "USD_JPY"}},
			"EUR_JPY", []string{"EUR_JPY", "USD_JPY"}},
		{"symbol → symbols",
			BotConfig{Symbol: "USD_JPY"},
			"USD_JPY", []string{"USD_JPY"}},
		{"both empty stays empty",
			BotConfig{},
			"", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.in
			cfg.Normalize()
			if cfg.Symbol != tc.wantSymbol {
				t.Errorf("Symbol: got %q, want %q", cfg.Symbol, tc.wantSymbol)
			}
			if !reflect.DeepEqual(cfg.Symbols, tc.wantSymbols) {
				t.Errorf("Symbols: got %v, want %v", cfg.Symbols, tc.wantSymbols)
			}
		})
	}
}

// AIAdvisorSection gained two runtime-tunable fields for multi-symbol
// parallel advisor cycles. Both default to 0 → caller uses derived
// defaults (single-bundle: no limit; otherwise half of bundle count and
// ClaudeCLITimeoutSeconds+30s respectively).
func TestAIAdvisorSection_MultiSymbolTuning(t *testing.T) {
	yaml := strings.Replace(validBotConfigYAML,
		"  claude_cli_timeout_seconds: 120",
		"  claude_cli_timeout_seconds: 120\n  max_concurrent_symbols: 3\n  per_symbol_timeout_seconds: 480",
		1)
	path := writeTempYAML(t, yaml)
	cfg, err := LoadBotConfig(path)
	if err != nil {
		t.Fatalf("LoadBotConfig: %v", err)
	}
	if cfg.AIAdvisor.MaxConcurrentSymbols != 3 {
		t.Errorf("MaxConcurrentSymbols: got %d want 3", cfg.AIAdvisor.MaxConcurrentSymbols)
	}
	if cfg.AIAdvisor.PerSymbolTimeoutSeconds != 480 {
		t.Errorf("PerSymbolTimeoutSeconds: got %d want 480", cfg.AIAdvisor.PerSymbolTimeoutSeconds)
	}
}

func TestAIAdvisorSection_MultiSymbolTuningOmittedDefaultsZero(t *testing.T) {
	// Back-compat: existing YAMLs without the new fields load and report 0
	// (= "derive default" sentinel).
	path := writeTempYAML(t, validBotConfigYAML)
	cfg, err := LoadBotConfig(path)
	if err != nil {
		t.Fatalf("LoadBotConfig: %v", err)
	}
	if cfg.AIAdvisor.MaxConcurrentSymbols != 0 || cfg.AIAdvisor.PerSymbolTimeoutSeconds != 0 {
		t.Errorf("defaults should be 0, got (%d, %d)",
			cfg.AIAdvisor.MaxConcurrentSymbols, cfg.AIAdvisor.PerSymbolTimeoutSeconds)
	}
}

func TestBotConfig_ValidateAgainstHardLimits(t *testing.T) {
	cases := []struct {
		name        string
		cfg         BotConfig
		allowed     []string
		wantErrFrag string // "" = expect no error
	}{
		{"all symbols allowed",
			BotConfig{Symbols: []string{"USD_JPY", "EUR_JPY"}},
			[]string{"USD_JPY", "EUR_JPY", "GBP_JPY"}, ""},
		{"one symbol not allowed",
			BotConfig{Symbols: []string{"USD_JPY", "EUR_JPY"}},
			[]string{"USD_JPY"}, "EUR_JPY"},
		{"legacy single Symbol still cross-checked",
			BotConfig{Symbol: "GBP_JPY"},
			[]string{"USD_JPY"}, "GBP_JPY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.Normalize()
			limits := &HardLimits{AllowedSymbols: tc.allowed}
			err := cfg.ValidateAgainstHardLimits(limits)
			if tc.wantErrFrag == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErrFrag) {
				t.Fatalf("want error containing %q, got %v", tc.wantErrFrag, err)
			}
		})
	}
}

func TestMode_Valid(t *testing.T) {
	cases := map[Mode]bool{
		ModeDisabled:    true,
		ModePaperConfig: true,
		ModeLiveConfig:  true,
		Mode("nope"):    false,
		Mode(""):        false,
	}
	for m, want := range cases {
		if got := m.Valid(); got != want {
			t.Errorf("Mode(%q).Valid() = %v, want %v", m, got, want)
		}
	}
}
