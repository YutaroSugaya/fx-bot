package app

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"fx-bot/backend/internal/config"
)

// CheckLiveModeFlags は Live 切替二重ロックを評価する。
// bot_config の bot.mode: live_config だけでは Live は走らない。以下 2 つが揃ったときだけ
// Live を許可。1 つでも欠けると caller が paper_config に降格する想定。
//
//	LIVE_TRADING_ENABLED=true       単独で false なら強制 paper
//	LIVE_CONFIRM_SYMBOLS=<csv>      bot_config.symbols と完全一致
//	                                (= set 等価、過不足なし)
//	LIVE_CONFIRM_SYMBOL=<symbol>    legacy。bot_config.symbols が 1 要素
//	                                のときのみ有効。multi-symbol では reject
//	                                — 新規 symbol を env 更新せずに Live に
//	                                混入させないため
//
// 数量上限の SSOT は hard_limits.quantity.max (configs/hard_limits.yaml)。
// per-order の cap は manual_trade / config.Validator が直接 hard_limits
// を見て enforce するので、live_guard で重ねて env var (LIVE_MAX_QUANTITY)
// は持たない (冗長)。
//
// GMO_API_KEY / GMO_API_SECRET は別途 live broker 初期化側でチェック。
//
// (ok=true, "")  → 全条件 satisfied
// (ok=false, reason) → reason に最初に失敗した条件の説明が入る
func CheckLiveModeFlags(botCfg *config.BotConfig) (bool, string) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LIVE_TRADING_ENABLED")))
	if v != "true" && v != "1" && v != "yes" {
		return false, "LIVE_TRADING_ENABLED is not 'true'"
	}
	cfgSymbols := botCfg.ResolveSymbols()
	envSymbols := readConfirmSymbols(len(cfgSymbols))
	if envSymbols == nil {
		return false, "LIVE_CONFIRM_SYMBOLS (or legacy LIVE_CONFIRM_SYMBOL for single-symbol) is required"
	}
	if msg, ok := compareSymbolSets(envSymbols, cfgSymbols); !ok {
		return false, msg
	}
	return true, ""
}

// readConfirmSymbols returns the operator-confirmed symbol set.
// Prefers LIVE_CONFIRM_SYMBOLS (CSV); falls back to legacy
// LIVE_CONFIRM_SYMBOL only when the config has exactly one symbol.
// Returns nil when neither is usable so the caller emits the required-env
// rejection (= explicit operator action needed).
func readConfirmSymbols(cfgSymbolCount int) []string {
	if raw := strings.TrimSpace(os.Getenv("LIVE_CONFIRM_SYMBOLS")); raw != "" {
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if v := strings.TrimSpace(p); v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	if cfgSymbolCount == 1 {
		// Legacy back-compat: single-symbol setups may keep using the
		// old LIVE_CONFIRM_SYMBOL env. Multi-symbol explicitly requires
		// the new env so a newly-added symbol cannot slip into Live
		// without operator review.
		if legacy := strings.TrimSpace(os.Getenv("LIVE_CONFIRM_SYMBOL")); legacy != "" {
			return []string{legacy}
		}
	}
	return nil
}

// compareSymbolSets enforces SET EQUALITY (no extras, no missing). Returns
// a human-readable reason on mismatch citing the first offending symbol.
func compareSymbolSets(env, cfg []string) (string, bool) {
	envSet := toSet(env)
	cfgSet := toSet(cfg)
	for _, sym := range sortedKeys(cfgSet) {
		if !envSet[sym] {
			return fmt.Sprintf("LIVE_CONFIRM_SYMBOLS does not confirm %q (config wants %v, env confirms %v)",
				sym, sortedKeys(cfgSet), sortedKeys(envSet)), false
		}
	}
	for _, sym := range sortedKeys(envSet) {
		if !cfgSet[sym] {
			return fmt.Sprintf("LIVE_CONFIRM_SYMBOLS contains %q which is not in bot_config.symbols (config %v, env %v)",
				sym, sortedKeys(cfgSet), sortedKeys(envSet)), false
		}
	}
	return "", true
}

func toSet(syms []string) map[string]bool {
	out := map[string]bool{}
	for _, s := range syms {
		out[s] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
