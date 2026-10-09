package main

import (
	"errors"
	"log/slog"
	"os"

	"fx-bot/backend/internal/adapter/broker"
	"fx-bot/backend/internal/app"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
)

// selectExecBroker は "Live mode upgrade/downgrade 判定 + 実 broker の選択" の
// ヘルパ (run() の流れを追いやすくするため分離)。
//
// 副作用として botCfg.Bot.Mode が書き換わる (Live 不可なら PaperConfig に降格)
// 点に注意。
//
// 戻り値:
//   - port.Broker: paper / live どちらかの実行 broker
//   - error: live_config を要求しているのに GMO API key/secret が欠落している場合のみ
func selectExecBroker(
	botCfg *config.BotConfig,
	hardLimits *config.HardLimits,
	paperBroker *broker.PaperBroker,
	logger *slog.Logger,
) (port.Broker, error) {
	// Live mode guard: mode=live_config だけでは Live は走らない。
	// 環境変数 (LIVE_TRADING_ENABLED=true / LIVE_CONFIRM_SYMBOLS) が両方揃ったときだけ
	// Live を許可。1 つでも欠けたら paper に降格 (WARN ログ) して継続。
	// "うっかり Live" の防止 (live_guard.go を参照)。
	// 数量上限は hard_limits.quantity.max が SSOT — manual_trade / Validator が enforce。
	if botCfg.Bot.Mode == config.ModeLiveConfig {
		if ok, reason := app.CheckLiveModeFlags(botCfg); !ok {
			logger.Warn("live_mode_downgraded_to_paper", "reason", reason)
			botCfg.Bot.Mode = config.ModePaperConfig
		} else {
			logger.Warn("=========================================")
			logger.Warn("===  LIVE MODE ENABLED                ===")
			logger.Warn("===  REAL MONEY TRADING IS ACTIVE     ===")
			logger.Warn("=========================================",
				"symbols", botCfg.ResolveSymbols(),
				"max_quantity", hardLimits.Quantity.Max)
		}
	}

	// paper_config → PaperBroker (simulated)
	// live_config  → GmoBroker with API credentials (real money)
	if botCfg.Bot.Mode != config.ModeLiveConfig {
		return paperBroker, nil
	}
	apiKey := os.Getenv("GMO_API_KEY")
	apiSecret := os.Getenv("GMO_API_SECRET")
	if apiKey == "" || apiSecret == "" {
		return nil, errors.New("live_config mode requires GMO_API_KEY and GMO_API_SECRET environment variables")
	}
	execBroker := broker.NewGmoBroker(broker.GmoBrokerConfig{
		PublicBaseURL:  botCfg.GMO.PublicBaseURL,
		PrivateBaseURL: botCfg.GMO.PrivateBaseURL,
		APIKey:         apiKey,
		APISecret:      apiSecret,
		Logger:         logger,
	})
	logger.Info("live_broker_initialized", "symbol", botCfg.Symbol)
	return execBroker, nil
}
