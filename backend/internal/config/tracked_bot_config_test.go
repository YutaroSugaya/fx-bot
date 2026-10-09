package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

// configs/bot_config.yaml は git tracked。ここにある値は clone した人がそのまま動かす既定になるので、
// 安全側の値であることをテストで固定する。値を緩めるときはこのテストも同じ commit で変える。

func loadTrackedBotConfig(t *testing.T) (*BotConfig, string) {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// backend/internal/config/tracked_bot_config_test.go → repo root へ 3 階層
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	cfgPath := filepath.Join(repoRoot, "configs", "bot_config.yaml")
	cfg, err := LoadBotConfig(cfgPath)
	if err != nil {
		t.Fatalf("load %s: %v", cfgPath, err)
	}
	return cfg, cfgPath
}

// tracked ファイルが live_config だと、checkout したマシンで LIVE_TRADING_ENABLED・
// LIVE_CONFIRM_SYMBOLS・GMO_API_KEY/SECRET が揃った瞬間に Live が走る。live は
// configs/bot_config.live.yaml(gitignore)+ BOT_CONFIG_PATH に切り出す。
func TestTrackedBotConfigIsNotLiveMode(t *testing.T) {
	cfg, cfgPath := loadTrackedBotConfig(t)
	if cfg.Bot.Mode == ModeLiveConfig {
		t.Fatalf("tracked %s has mode=%q — live_config must NEVER be committed. "+
			"Use configs/bot_config.live.yaml (gitignored) + BOT_CONFIG_PATH instead.",
			cfgPath, cfg.Bot.Mode)
	}
}

// 最小ロット(1,000 通貨)を前提にした損失 cap。ロットを上げるときに一緒に動かす。
func TestTrackedBotConfig_DailyLossAndConsecutiveCapsAreStrict(t *testing.T) {
	cfg, _ := loadTrackedBotConfig(t)
	const maxAllowedDailyLossJPY = 2000
	if cfg.Risk.MaxDailyLossJPY > maxAllowedDailyLossJPY {
		t.Errorf("max_daily_loss_jpy=%d > %d — the tracked default must stay tight; "+
			"raise it only together with the lot size.",
			cfg.Risk.MaxDailyLossJPY, maxAllowedDailyLossJPY)
	}
	const maxAllowedConsecutiveLosses = 4
	if cfg.Risk.MaxConsecutiveLosses > maxAllowedConsecutiveLosses {
		t.Errorf("max_consecutive_losses=%d > %d — the tracked default must stay tight.",
			cfg.Risk.MaxConsecutiveLosses, maxAllowedConsecutiveLosses)
	}
}

// claude CLI のハングを早く検知する。応答が 5 分以内に来なければ打ち切って retry / alert 経路へ。
func TestTrackedBotConfig_ClaudeCLITimeoutIsTight(t *testing.T) {
	cfg, _ := loadTrackedBotConfig(t)
	const maxAllowedTimeoutSec = 300
	if cfg.AIAdvisor.ClaudeCLITimeoutSeconds > maxAllowedTimeoutSec {
		t.Errorf("claude_cli_timeout_seconds=%d > %d — a hung CLI must be cut off within 5 minutes.",
			cfg.AIAdvisor.ClaudeCLITimeoutSeconds, maxAllowedTimeoutSec)
	}
}

// LLM(claude CLI)を定期的に呼ぶ経路は opt-in。clone して `make start` しただけで
// 課金・利用枠の消費が始まらないよう、tracked ファイルでは全部 off にしておく。
// 使う人は自分の bot_config(BOT_CONFIG_PATH)で enabled: true にする。
func TestTrackedBotConfig_LLMCallsAreOptIn(t *testing.T) {
	cfg, cfgPath := loadTrackedBotConfig(t)
	if cfg.AIAdvisor.Enabled {
		t.Errorf("tracked %s: ai_advisor.enabled must be false (LLM calls are opt-in)", cfgPath)
	}
	if cfg.AdvisorV2.Enabled {
		t.Errorf("tracked %s: advisor_v2.enabled must be false (LLM calls are opt-in)", cfgPath)
	}
	if cfg.LLMDecision.Enabled {
		t.Errorf("tracked %s: llm_decision.enabled must be false (LLM calls are opt-in)", cfgPath)
	}
}
