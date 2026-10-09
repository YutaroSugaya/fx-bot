package app

import (
	"strings"
	"testing"

	"fx-bot/backend/internal/config"
)

func newBotCfg(symbol string) *config.BotConfig {
	c := &config.BotConfig{Symbol: symbol}
	c.Normalize()
	return c
}

func newBotCfgMulti(symbols ...string) *config.BotConfig {
	c := &config.BotConfig{Symbols: symbols}
	c.Normalize()
	return c
}

// TestCheckLiveModeFlags 全フラグ評価を 1 つの table で網羅。
// Live 切替二重ロックの全分岐を 1 関数で確認。
//
// 数量 cap は startup 専用の env チェックを持たず、実 order の数量 cap である
// hard_limits.quantity.max (manual_trade.go / config.Validator) に一本化している
// (単一の SSOT = hard_limits.yaml)。
func TestCheckLiveModeFlags(t *testing.T) {
	cases := []struct {
		name               string
		envEnabled         string
		envSymbol          string
		cfgSymbol          string
		wantOK             bool
		wantReasonContains string
	}{
		{"all set passes", "true", "USD_JPY", "USD_JPY", true, ""},
		{"enabled=yes also passes", "yes", "USD_JPY", "USD_JPY", true, ""},
		{"enabled=1 also passes", "1", "USD_JPY", "USD_JPY", true, ""},
		{"enabled=TRUE also passes", "TRUE", "USD_JPY", "USD_JPY", true, ""},
		{"enabled=false rejects", "false", "USD_JPY", "USD_JPY", false, "LIVE_TRADING_ENABLED"},
		{"symbol mismatch rejects", "true", "EUR_USD", "USD_JPY", false, "LIVE_CONFIRM_SYMBOL"},
		// LIVE_MAX_QUANTITY が設定されていなくても Live mode に入れる (旧仕様: 必須 → 新仕様: 廃止)
		{"works without LIVE_MAX_QUANTITY env", "true", "USD_JPY", "USD_JPY", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LIVE_TRADING_ENABLED", tc.envEnabled)
			t.Setenv("LIVE_CONFIRM_SYMBOL", tc.envSymbol)
			t.Setenv("LIVE_CONFIRM_SYMBOLS", "")
			// Intentionally clear LIVE_MAX_QUANTITY — the new
			// implementation must not read it.
			t.Setenv("LIVE_MAX_QUANTITY", "")

			ok, reason := CheckLiveModeFlags(newBotCfg(tc.cfgSymbol))
			if ok != tc.wantOK {
				t.Fatalf("ok: got %v want %v (reason=%q)", ok, tc.wantOK, reason)
			}
			if tc.wantReasonContains != "" && !strings.Contains(reason, tc.wantReasonContains) {
				t.Errorf("reason should contain %q; got %q", tc.wantReasonContains, reason)
			}
		})
	}
}

// --- multi-symbol Live confirm ---
//
// With multi-symbol bot_config, the operator must explicitly confirm every
// symbol — using either LIVE_CONFIRM_SYMBOLS (CSV) or, for single-symbol
// back-compat, LIVE_CONFIRM_SYMBOL. The env set MUST equal the config set
// exactly (no extras, no missing) so an unintended new symbol can't slip
// into Live just because the env hasn't been updated.

func TestCheckLiveModeFlags_MultiSymbol(t *testing.T) {
	cases := []struct {
		name          string
		envSymbols    string // LIVE_CONFIRM_SYMBOLS
		envLegacy     string // LIVE_CONFIRM_SYMBOL (back-compat)
		cfgSymbols    []string
		wantOK        bool
		wantReasonHas string
	}{
		{
			name:       "exact match multi passes",
			envSymbols: "USD_JPY,EUR_JPY",
			cfgSymbols: []string{"USD_JPY", "EUR_JPY"},
			wantOK:     true,
		},
		{
			name:       "order-insensitive multi passes",
			envSymbols: "EUR_JPY,USD_JPY",
			cfgSymbols: []string{"USD_JPY", "EUR_JPY"},
			wantOK:     true,
		},
		{
			name:          "env subset of config rejects (missing EUR_JPY)",
			envSymbols:    "USD_JPY",
			cfgSymbols:    []string{"USD_JPY", "EUR_JPY"},
			wantOK:        false,
			wantReasonHas: "EUR_JPY",
		},
		{
			name:          "env superset of config rejects (extra GBP_JPY)",
			envSymbols:    "USD_JPY,EUR_JPY,GBP_JPY",
			cfgSymbols:    []string{"USD_JPY", "EUR_JPY"},
			wantOK:        false,
			wantReasonHas: "GBP_JPY",
		},
		{
			name:          "legacy LIVE_CONFIRM_SYMBOL with multi-symbol config rejects (the vulnerability)",
			envSymbols:    "",        // unset
			envLegacy:     "USD_JPY", // legacy single-symbol env
			cfgSymbols:    []string{"USD_JPY", "EUR_JPY"},
			wantOK:        false,
			wantReasonHas: "LIVE_CONFIRM_SYMBOLS",
		},
		{
			name:       "legacy LIVE_CONFIRM_SYMBOL with single-symbol config passes (back-compat)",
			envSymbols: "",
			envLegacy:  "USD_JPY",
			cfgSymbols: []string{"USD_JPY"},
			wantOK:     true,
		},
		{
			name:       "LIVE_CONFIRM_SYMBOLS wins over LIVE_CONFIRM_SYMBOL when both set",
			envSymbols: "USD_JPY",
			envLegacy:  "EUR_JPY", // would be wrong if legacy was used
			cfgSymbols: []string{"USD_JPY"},
			wantOK:     true,
		},
		{
			name:          "both envs empty rejects",
			envSymbols:    "",
			envLegacy:     "",
			cfgSymbols:    []string{"USD_JPY"},
			wantOK:        false,
			wantReasonHas: "LIVE_CONFIRM",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LIVE_TRADING_ENABLED", "true")
			t.Setenv("LIVE_CONFIRM_SYMBOLS", tc.envSymbols)
			t.Setenv("LIVE_CONFIRM_SYMBOL", tc.envLegacy)
			t.Setenv("LIVE_MAX_QUANTITY", "")
			ok, reason := CheckLiveModeFlags(newBotCfgMulti(tc.cfgSymbols...))
			if ok != tc.wantOK {
				t.Fatalf("ok: got %v want %v (reason=%q)", ok, tc.wantOK, reason)
			}
			if tc.wantReasonHas != "" && !strings.Contains(reason, tc.wantReasonHas) {
				t.Errorf("reason should contain %q; got %q", tc.wantReasonHas, reason)
			}
		})
	}
}
