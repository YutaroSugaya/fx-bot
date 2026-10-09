package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fx-bot/backend/internal/adapter/broker"
	"fx-bot/backend/internal/app"
	"fx-bot/backend/internal/app/handler"
	"fx-bot/backend/internal/app/livesignal"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/clock"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/safety"
	"fx-bot/backend/internal/usecase/command"
	"fx-bot/backend/internal/usecase/query"
)

// httpWiringInput bundles everything wireHTTPHandlers needs. Kept as a
// dedicated struct (vs. dozens of positional args) so adding a future
// handler / query doesn't change the call site.
type httpWiringInput struct {
	BotConfig      *config.BotConfig
	Paths          runtimePaths
	Logger         *slog.Logger
	Counters       *app.Counters
	Repos          *port.Repositories
	PublicBroker   *broker.GmoBroker
	Holder         *app.ActiveConfigHolder
	CloseCommands  map[string]*command.ClosePositionCommand
	ManualCommands map[string]*command.ManualTradeCommand
	AskClaudeQuery *query.AskClaudeQuery
	TriggerAdvisor handler.TriggerAdvisorFunc
	TriggerLLM     func() error // manual "全ペア再判断" → POST /api/llm-decision/trigger (nil → 503)
	SignalHolder   *livesignal.Holder
	StartedAt      time.Time

	// Auth / CORS は env + bot mode から main.go で組み立てた値を渡す。
	// この層では fail-closed 判定をせず、APIServer.Run() に委ねる。
	Auth         app.APIAuth
	CORS         app.APICORS
	AllowedHosts []string
}

// wireHTTPHandlers constructs the APIServer and all handler structs from one
// place. Returns the assembled APIServer so main.go can hook
// OnTickerError → IncrementTickerErrors and call Run inside a goroutine.
//
// Kept out of main.go to keep cmd/bot/main.go focused on the
// runtime sequence (load config → build state → start loops). Each handler
// is a 5-10 line block here, easy to scan for "where is /api/foo wired".
//
// Auth / CORS は呼び出し側 (main.go) が APIAuth / APICORS を構築して渡す。
// この層で env を直読みしないのは、起動時に fail-closed 判定したいから
// (paper/live で creds 欠落 → APIServer.Run() がエラーを返し bot 全体を停止、
// disabled mode / 明示 override でのみ AllowUnauthenticated)。
func wireHTTPHandlers(in httpWiringInput) *app.APIServer {
	apiSrv := &app.APIServer{
		Addr:         in.Paths.APIAddr,
		Logger:       in.Logger,
		Counters:     in.Counters,
		Auth:         in.Auth,
		CORS:         in.CORS,
		AllowedHosts: in.AllowedHosts,
	}
	tickerErrCounter := apiSrv.TickerErrors()

	// /api/status emits per-symbol breakdown (Symbols []) + account-wide
	// totals on top of the legacy single-symbol fields (primary symbol).
	apiSrv.StatusHandler = &handler.StatusHandler{
		StatusQuery: &query.GetBotStatusQuery{
			BotConfig:        in.BotConfig,
			Positions:        in.Repos.Positions,
			GetActiveConfigs: in.Holder.All,
			EmergencyActive:  func() bool { return safety.Active(in.Paths.EmergencyFlag) },
			CountersFn:       func() any { return in.Counters.Snapshot() },
			TickerErrors:     func() int64 { return tickerErrCounter.Load() },
			StartedAt:        in.StartedAt,
			// Dashboard 用: 24h ウィンドウの観察指標。
			// 各 fn は repo を closure-capture。失敗時は Execute 側で
			// omit され、status エンドポイント全体は壊さない。
			Pnl24hJpyFn: func(ctx context.Context, symbol string) (float64, error) {
				return in.Repos.Trades.SumPnLJPYClosedSinceBySymbol(ctx, symbol, time.Now().Add(-24*time.Hour))
			},
			RejectCount24hFn: func(ctx context.Context) (int, error) {
				return in.Repos.ValidationEvents.CountFailSince(ctx, time.Now().Add(-24*time.Hour))
			},
			EarlyExitCount24hFn: func(ctx context.Context, symbol string) (int, error) {
				return in.Repos.Trades.CountEarlyExitTradesSinceBySymbol(ctx, symbol, time.Now().Add(-24*time.Hour))
			},
			LastAdvisorDurationMsFn: func(ctx context.Context) (int, error) {
				return in.Repos.AdvisorRuns.GetLastDurationMs(ctx)
			},
			// 計測パネル: 直近 closed trade (最大 90 日 / 30 件) から edge 指標
			// (PF / RR / 期待値 / 連敗) を算出。promote gate の「直近 30 trade」と件数を揃える。
			EdgeMetricsFn: func(ctx context.Context, symbol string) (query.EdgeMetricsView, error) {
				const lookbackDays = 90
				const window = 30
				now := time.Now()
				// Floor the lookback at the LLM loop's go-live (llm_decision.reflection_start_at,
				// the same epoch reflection uses) so the edge panel — win rate / PnL / count —
				// reflects ONLY this loop's trades. Prior strategies' trades stay in the DB but are
				// excluded from the dashboard aggregates.
				since := in.BotConfig.LLMDecision.ReflectionSince(now, lookbackDays*24*time.Hour)
				// Exclude the operator's own discretionary trades (external GMO-app
				// entries + manual_trade entries) so the panel measures the BOT's
				// edge. Fetch wider than `window` first so that dropping those does
				// not shrink the bot window below 30 trades.
				raw, err := in.Repos.Trades.ListClosedBySymbolSince(ctx, symbol, since, window*3)
				if err != nil {
					return query.EdgeMetricsView{}, err
				}
				trades := command.BotTrades(raw)
				if len(trades) > window {
					trades = trades[:window]
				}
				m := command.DeriveEdgeMetrics(trades)
				return query.EdgeMetricsView{
					TradeCount:           m.TradeCount,
					WinCount:             m.WinCount,
					LossCount:            m.LossCount,
					WinRatePct:           m.WinRatePct,
					ProfitFactor:         m.ProfitFactor,
					AvgWinPips:           m.AvgWinPips,
					AvgLossPips:          m.AvgLossPips,
					RewardRisk:           m.RewardRisk,
					ExpectancyJpy:        m.ExpectancyJPY,
					GrossPnlJpy:          m.GrossPnLJPY,
					FeeJpy:               m.FeeTotalJPY,
					SwapJpy:              m.SwapTotalJPY,
					NetPnlJpy:            m.NetPnLJPY,
					FeeEstimatedCount:    m.FeeEstimatedCount,
					MaxConsecutiveLosses: m.MaxConsecutiveLosses,
					WindowDays:           lookbackDays,
				}, nil
			},
			Logger: in.Logger,
		},
		// /api/active-config?symbol=… で per-symbol、no query で全 symbol map。
		GetActiveConfigs: in.Holder.All,
	}
	// /api/positions[?symbol=…]: PipSize は query 側が per-position に
	// market.PipSize(p.Symbol) で解決する。GetTicker は row の symbol を受け
	// 取り、Query 側で symbol ごとに 1 度だけ評価される (per-row cache)。
	apiSrv.PositionsHandler = &handler.PositionsHandler{
		ListQuery: &query.ListOpenPositionsQuery{
			Positions: in.Repos.Positions,
			GetTicker: func(ctx context.Context, sym string) (*market.Ticker, error) {
				return in.PublicBroker.GetTicker(ctx, sym)
			},
		},
		CloseCommands: in.CloseCommands,
		ExtendCommand: &command.ExtendMaxHoldCommand{
			Positions: in.Repos.Positions,
			Clock:     clock.System,
			Logger:    in.Logger,
		},
		Logger: in.Logger,
	}
	// Floor /api/trades at the LLM loop's go-live so the dashboard's cumulative P&L /
	// win-rate / avg / max (computed frontend-side over this list) reflect ONLY this
	// loop's trades, not prior strategies'. Old trades stay in the DB.
	tradesEpoch, _ := in.BotConfig.LLMDecision.StartAtTime()
	apiSrv.TradesHandler = &handler.TradesHandler{
		ListQuery:      &query.ListTradesQuery{Trades: in.Repos.Trades, Epoch: tradesEpoch},
		ManualCommands: in.ManualCommands, Logger: in.Logger,
	}
	apiSrv.AdvisorHandler = &handler.AdvisorHandler{
		RecentQuery: &query.ListRecentDecisionsQuery{StrategyConfigs: in.Repos.StrategyConfigs},
		Trigger:     in.TriggerAdvisor,
		CLITimeout:  time.Duration(in.BotConfig.AIAdvisor.ClaudeCLITimeoutSeconds) * time.Second,
		Logger:      in.Logger,
	}
	apiSrv.EmergencyHandler = &handler.EmergencyHandler{
		FlagPath: in.Paths.EmergencyFlag, Logger: in.Logger,
	}
	apiSrv.AdvisorV2Handler = &handler.AdvisorV2Handler{
		StatusPath: in.Paths.AdvisorV2Status, Logger: in.Logger,
	}
	apiSrv.LLMDecisionHandler = &handler.LLMDecisionHandler{
		StatusPath: filepath.Join(filepath.Dir(in.Paths.AdvisorV2Status), "llm_decision_status.json"),
		Trigger:    in.TriggerLLM,
		Logger:     in.Logger,
	}
	apiSrv.AskClaudeHandler = &handler.AskClaudeHandler{
		Query: in.AskClaudeQuery, Logger: in.Logger,
	}
	apiSrv.MarketHandler = &handler.MarketHandler{
		Query: &query.GetMarketStateQuery{
			Candles: in.Repos.Candles,
			Broker:  in.PublicBroker,
			Logger:  in.Logger,
		},
		Symbol: in.BotConfig.Symbol,
	}
	if in.SignalHolder != nil {
		apiSrv.StrategySignalHandler = &handler.StrategySignalHandler{Get: in.SignalHolder.Get}
	}
	return apiSrv
}

// buildAPIAuth は DASHBOARD_USER / DASHBOARD_PASS env から APIAuth を組む。
//
// fail-closed policy:
//   - production (bot.mode=paper_config / live_config in configs/bot_config.yaml)
//     は両 env 必須。欠落で APIServer.Run() が ErrAPIAuthMisconfigured で fail し
//     bot 全体を止める。
//   - bot.mode=disabled のときだけ AllowUnauthenticated=true を許可
//     (dev 向け escape hatch、起動ログに WARN が出る)。
//
// Operator override: `DASHBOARD_AUTH_DISABLE=true` を立てると mode に関係なく
// auth を bypass する。fail-closed を意図的に抜く loud な escape hatch で、
// 起動ログに必ず WARN が出る。bypass は loopback bind(API_ADDR=127.0.0.1:…)の
// ときだけ有効で、それ以外は APIServer.Run() が ErrAPIAuthUnsafeBind で起動を拒否する。
//
// mode は configs/bot_config.yaml の `bot.mode` が SSOT。`BOT_MODE` env は参照しない。
func buildAPIAuth(botCfg *config.BotConfig, logger *slog.Logger) app.APIAuth {
	// 1. operator が明示的に DASHBOARD_AUTH_DISABLE=true を立てた場合の最強優先。
	//    "true" のみ受理 (case-insensitive)。"false" や "0" は無視。
	if strings.EqualFold(strings.TrimSpace(os.Getenv("DASHBOARD_AUTH_DISABLE")), "true") {
		if logger != nil {
			logger.Warn("api_auth_bypass_via_env",
				"env", "DASHBOARD_AUTH_DISABLE=true",
				"mode", string(botCfg.Bot.Mode),
				"reason", "operator explicitly disabled BasicAuth — dashboard is unprotected")
		}
		return app.APIAuth{AllowUnauthenticated: true}
	}

	user, pass := os.Getenv("DASHBOARD_USER"), os.Getenv("DASHBOARD_PASS")
	if botCfg.Bot.Mode == config.ModeDisabled && (user == "" || pass == "") {
		if logger != nil {
			logger.Warn("api_auth_bypass_disabled_mode",
				"reason", "bot.mode=disabled with missing DASHBOARD_USER/PASS — auth bypassed")
		}
		return app.APIAuth{AllowUnauthenticated: true}
	}
	return app.APIAuth{User: user, Pass: pass}
}

// buildAllowedHosts は DASHBOARD_ALLOWED_HOSTS env(カンマ区切り)を、loopback bind 時に
// localhost / 127.0.0.1 / ::1 に加えて受け付ける Host 名にする(DNS rebinding 対策・APIServer.AllowedHosts)。
// ポートは無視し、小文字にそろえる。
func buildAllowedHosts() []string {
	var out []string
	for _, h := range strings.Split(os.Getenv("DASHBOARD_ALLOWED_HOSTS"), ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if name, _, err := net.SplitHostPort(h); err == nil {
			h = name
		}
		out = append(out, strings.Trim(h, "[]"))
	}
	return out
}

// buildAPICORS は DASHBOARD_ALLOWED_ORIGINS env (comma-separated) を allowlist
// として読む。未設定なら空 list = CORS ヘッダ無し (同一 origin のみ)。
// dashboard を別 origin で動かすときだけ明示的に設定する想定。
func buildAPICORS() app.APICORS {
	raw := strings.TrimSpace(os.Getenv("DASHBOARD_ALLOWED_ORIGINS"))
	if raw == "" {
		return app.APICORS{}
	}
	parts := strings.Split(raw, ",")
	origins := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			origins = append(origins, v)
		}
	}
	return app.APICORS{AllowedOrigins: origins}
}
