package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"fx-bot/backend/internal/adapter/advisor"
	"fx-bot/backend/internal/adapter/artifact"
	"fx-bot/backend/internal/adapter/broker"
	"fx-bot/backend/internal/adapter/playbook"
	"fx-bot/backend/internal/app"
	"fx-bot/backend/internal/app/livesignal"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/clock"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/safety"
	"fx-bot/backend/internal/usecase"
	"fx-bot/backend/internal/usecase/command"
)

// advisor 向け MarketSummary に載せる過去判断/イベント文脈の範囲。
const (
	// advisorRecentDecisionsLimit は recent_decisions に載せる直近判断の件数。
	advisorRecentDecisionsLimit = 5
	// advisorEventHorizon は event_context に「これから来るイベント」を載せる先読み幅。
	// 30 分前仕込みを確実にカバーするため 60 分。
	advisorEventHorizon = 60 * time.Minute
)

// allowedStrategies is the strategy-name whitelist enforced both by the config
// promoter (rejects an advisor config naming anything else) and surfaced to the
// advisor prompt. DERIVED from the engine registry so it can never drift from
// what the engine can actually run: a hand-maintained list that omits a
// registered strategy would make a re-enabled advisor reject/replace an active
// config naming it on its first cycle. Treated read-only by both consumers.
var allowedStrategies = deriveAllowedStrategies()

// deriveAllowedStrategies builds the whitelist from the strategy engine's
// registry (single source of truth).
func deriveAllowedStrategies() []string {
	names := strategy.NewEngine().RegisteredNames()
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, string(n))
	}
	return out
}

// assertActiveStrategyWhitelisted is a startup guard: the
// active (frozen) config's strategy MUST be in allowedStrategies, otherwise a
// future advisor cycle would reject/replace the live config. Since the
// whitelist is now derived from the registry this can only fail if someone
// hand-trims it — fail loud rather than let the time bomb re-arm.
func assertActiveStrategyWhitelisted(strategyName string) error {
	for _, s := range allowedStrategies {
		if s == strategyName {
			return nil
		}
	}
	return fmt.Errorf("active config strategy %q is not in allowed_strategies %v "+
		"(derive whitelist from engine registry)", strategyName, allowedStrategies)
}

// bundleWiringDeps groups the shared (cross-symbol) inputs needed to build
// one SymbolBundle. Per-symbol parameters travel as buildSymbolBundle args.
type bundleWiringDeps struct {
	BotConfig         *config.BotConfig
	HardLimits        *config.HardLimits
	Paths             runtimePaths
	Validator         *config.Validator
	Holder            *app.ActiveConfigHolder
	Repos             *port.Repositories
	PublicBroker      *broker.GmoBroker
	ExecBroker        port.Broker
	Notifier          port.Notifier
	Engine            *strategy.Engine
	Counters          *app.Counters
	CloseMutex        *sync.Mutex
	SharedEntryMutex  *sync.Mutex // shared across symbols → account-wide entry serialization
	Logger            *slog.Logger
	EmergencyFlagPath string
	// ProjectRoot is the repo root used as the advisor v2 judge subprocess CWD (so the
	// breakout-advisor subagent can Read .claude/agents + prompts). Same root as the ai_advisor.
	ProjectRoot string
	// SignalHolder は worker が毎 tick の実評価 (戦略 + gate) を書き込む共有
	// store。/api/strategy/signal が読む。bot プロセスごとに 1 つ。
	SignalHolder *livesignal.Holder
	// PendingPositions は ExecuteOrder / ManualTradeCommand / Reconcile に同じ
	// instance を渡して entry saga 進行中の broker_position_id を共有する
	// (race-window 対策)。bot プロセスごとに 1 つ。
	PendingPositions port.PendingPositionTracker
	// ParseErrorMonitor は AdvisorCycle.OnRunComplete から status を受け取り
	// scheduler の nextScheduleInterval に「parse_error 中なら 10 分 cadence」
	// を通知する。bot プロセスごとに 1 つ。
	ParseErrorMonitor *app.ParseErrorMonitor
	// EventCalendar は経済指標発表帯 freeze 用の手書きカレンダー。
	// Worker / EntryAdmission の snapshot に
	// 同じ instance を渡し、InEventFreeze flag に反映する。nil 許容。
	EventCalendar *config.EventCalendar
	// LLMJournal は自律 LLM ループの判断履歴(append-only JSONL)。全 symbol で同じ
	// instance を共有し、書込 mutex が並列サイクルを直列化する。観測のみ・nil 許容。
	LLMJournal port.LLMDecisionJournal
	// EventRetrigger は決済確定・急変動で LLM 再判断を
	// 追加起動するコーディネータ。全 symbol で同じ instance を共有 (cooldown が
	// global に効く)。nil = 機能 OFF。Trigger の配線は main.go が bundles 構築後に行う。
	EventRetrigger *command.LLMEventRetrigger
}

// perSymbolPaths derives symbol-suffixed artifact paths from the
// bot-wide template paths. For 1-symbol setups the legacy unsuffixed paths
// are reused so existing test fixtures / runtime files keep working;
// 2+ symbols get suffixed paths (`summary_USD_JPY.json` etc.) so concurrent
// advisor cycles never collide on the same file.
type perSymbolPaths struct {
	Summary      string
	NextConfig   string
	ActiveConfig string
}

func resolveSymbolPaths(paths runtimePaths, symbol string, isPrimaryOnly bool) perSymbolPaths {
	if isPrimaryOnly {
		return perSymbolPaths{
			Summary:      paths.Summary,
			NextConfig:   paths.NextConfig,
			ActiveConfig: paths.ActiveConfig,
		}
	}
	return perSymbolPaths{
		Summary:      withSymbolSuffix(paths.Summary, symbol),
		NextConfig:   withSymbolSuffix(paths.NextConfig, symbol),
		ActiveConfig: withSymbolSuffix(paths.ActiveConfig, symbol),
	}
}

// withSymbolSuffix inserts "_<symbol>" before the file extension.
// runtime/ai_input/latest_summary.json + USD_JPY
//
//	→ runtime/ai_input/latest_summary_USD_JPY.json
func withSymbolSuffix(path, symbol string) string {
	if path == "" || symbol == "" {
		return path
	}
	ext := filepath.Ext(path)
	return strings.TrimSuffix(path, ext) + "_" + symbol + ext
}

// buildSymbolBundle wires every per-symbol usecase / adapter instance for one
// trading symbol. Returns a ready-to-start bundle. Startup reconcile is
// included in the bundle; the caller invokes StartupReconcile.Run() and
// applies its fail-close policy (Live aborts on Tripped>0 / err).
func buildSymbolBundle(
	ctx context.Context,
	symbol string,
	deps bundleWiringDeps,
	primaryOnly bool,
) (*app.SymbolBundle, error) {
	log := deps.Logger.With("symbol", symbol)
	mode := deps.BotConfig.Bot.Mode
	psp := resolveSymbolPaths(deps.Paths, symbol, primaryOnly)

	// Paper position restore (Live broker reads from GMO directly).
	if pb, ok := deps.ExecBroker.(*broker.PaperBroker); ok {
		restorePaperPositions(ctx, pb, deps.Repos.Positions, symbol, log)
	}

	// Aggregator capacity: 24h at 1m/5m/15m. 1h keeps 14 days (336 bars) because the
	// ma_pullback MTF runner reads a 1h 200SMA + 20-bar slope window (≥220 bars)
	// for its TREND, and mtf_pullback reads 1h swing structure. 14 days of FX
	// 1h bars (~240 trading-hour bars after weekend gaps) comfortably clears 220, and
	// the boot-time backfill (14-day DB restore) fills it. The 5m execution chart
	// still fits its 200-bar 200SMA/EMA in the 288-bar 5m ring.
	aggregator := market.NewAggregator(symbol, map[time.Duration]int{
		time.Minute:      24 * 60,
		5 * time.Minute:  24 * 12,
		15 * time.Minute: 24 * 4,
		time.Hour:        24 * 14,
	})
	app.BootstrapCandles(ctx, deps.PublicBroker, aggregator, deps.Repos.Candles, symbol, deps.EmergencyFlagPath, log)

	// Per-symbol artifact stores: separate paths so parallel advisor cycles
	// never race on the same files.
	summaryStore := artifact.NewFileMarketSummaryStore(psp.Summary)
	strategyConfigStore := artifact.NewFileStrategyConfigStore(psp.NextConfig, psp.ActiveConfig)

	// Per-symbol Promoter (pinned to this symbol so YAML targeting a
	// different symbol is rejected; multi-symbol safety).
	promoter := command.NewPromoter(deps.Validator, deps.Repos.StrategyConfigs, deps.Repos.ValidationEvents,
		strategyConfigStore, mode, string(port.AdvisorRunSourceAuto))
	promoter.ConfigPromoter = deps.Repos.ConfigPromoter
	promoter.Logger = log
	promoter.ExpectedSymbol = symbol
	promoter.AllowedStrategies = allowedStrategies

	// Per-symbol Advisor (writes to symbol-specific next.yaml).
	cliAdvisor := advisor.New(deps.Paths.ClaudeCLI, deps.Paths.Prompt, deps.Paths.AIOutDir,
		psp.NextConfig, deps.BotConfig.AIAdvisor.ClaudeCLITimeoutSeconds)

	// getActive closure capturing THIS bundle's symbol.
	getActive := func() *config.StrategyConfig { return deps.Holder.Get(symbol) }

	executor := command.NewExecuteOrder(deps.ExecBroker, deps.Repos.Positions, mode, symbol, log)
	executor.EmergencyFlagPath = deps.EmergencyFlagPath
	executor.EntryMutex = deps.SharedEntryMutex
	executor.ActiveConfig = getActive
	executor.PendingTracker = deps.PendingPositions
	// qty halving floor: hard_limits.quantity.min を broker 最低発注として渡す。
	// 1000 通貨未満になる halve は skip (applyQtyMultiplier 内で no-op)。
	executor.MinQuantity = deps.HardLimits.Quantity.Min
	// hard_limits を発注境界の実 Signal 値検査に渡す。
	executor.HardLimits = deps.HardLimits

	admission := &command.EntryAdmission{
		Symbol:            symbol,
		Mutex:             deps.SharedEntryMutex,
		Positions:         deps.Repos.Positions,
		Trades:            deps.Repos.Trades,
		BotConfig:         deps.BotConfig,
		HardLimits:        deps.HardLimits,
		EmergencyFlagPath: deps.EmergencyFlagPath,
		Logger:            log,
		CooldownFn: func(now time.Time) (bool, time.Time, string) {
			return app.ComputeCooldown(now, deps.HardLimits, deps.Repos.Trades, context.Background())
		},
		EventCalendar: deps.EventCalendar,
	}
	executor.Admission = admission

	manager := command.NewManageOpenPositions(deps.ExecBroker, deps.Repos.Positions, deps.Repos.Trades, symbol, mode, log)
	manager.EmergencyFlagPath = deps.EmergencyFlagPath
	manager.Closer = deps.Repos.Closer
	manager.CloseMutex = deps.CloseMutex
	// session flatten: llm_decision.session_flatten_jst (例 05:30 JST、未設定 = OFF) に残玉を通常スプレッドで手仕舞い
	// (05:45 スプレッド壁の 15 分前・土曜回が週末ギャップ対策を兼ねる)。全 symbol 一律 —
	// 壁は全ペアで立つ (GBP_JPY で 14pips・USD_JPY で 10pips 程度まで広がる)。
	if m, ok := deps.BotConfig.LLMDecision.SessionFlattenMinutesJST(); ok {
		manager.SessionFlattenEnabled = true
		manager.SessionFlattenStartMinuteJST = m
	}
	// shadow 時間ストップ計測 (発動なし・journal 記録のみ)。
	manager.Journal = deps.LLMJournal

	// event_retrigger: every settled close — bot-side saga (MaxHold/ratchet/
	// manual) AND the runtime reconcile's broker OCO fill — reports to the shared
	// coordinator so the freed slot is re-judged immediately. nil coordinator = hooks stay
	// nil = feature off.
	var onPositionClosed func(symbol, reason string)
	if deps.EventRetrigger != nil {
		onPositionClosed = deps.EventRetrigger.OnPositionClosed
	}
	manager.OnClosed = onPositionClosed

	evaluator := &command.EvaluateEntry{
		Engine:        deps.Engine,
		RejectionRepo: deps.Repos.SignalRejections,
		Logger:        log,
	}
	tradingCycle := &command.TradingCycle{
		Evaluator: evaluator,
		Executor:  executor,
		Logger:    log,
		// advisor_v2.exclusive: when v2 is the only entry source, disable engine-tick entries
		// (the fixed active config). Exits still run; open positions keep being managed.
		// engine-tick の固定戦略(trend_follow 等)の新規入口を止める条件:
		//   advisor_v2 が exclusive、OR 自律LLMループが有効(LLM を唯一の新規エントリー源にする)。
		//   どちらも「出口=既存建玉の管理」は継続(EntriesDisabled は新規のみ)。
		EntriesDisabled: (deps.BotConfig.AdvisorV2.Enabled && deps.BotConfig.AdvisorV2.Exclusive) || deps.BotConfig.LLMDecision.Enabled,
		// Hour partition: during the JST hours the LLM loop cedes for this symbol
		// (exclude_hours_jst), the per-tick engine IS allowed to enter so the
		// deterministic strategy (exhaustion_fade) owns those hours. Must mirror the
		// LLM cycle's ExcludeHoursJST so the two paths never overlap.
		EngineOwnedHoursJST: deps.BotConfig.LLMDecision.ExcludeHoursJST[symbol],
	}

	// Startup reconcile (every mode).
	startupReconcile := buildStartupReconcile(reconcileWiringDeps{
		Broker:            deps.ExecBroker,
		Repos:             deps.Repos,
		Notifier:          deps.Notifier,
		Symbol:            symbol,
		Logger:            log,
		EmergencyFlagPath: deps.EmergencyFlagPath,
		LiveMode:          mode,
		PendingTracker:    deps.PendingPositions,
		Counters:          deps.Counters,
	})
	// Runtime reconcile (Live only).
	var runtimeReconcile *command.Reconcile
	if mode == config.ModeLiveConfig {
		runtimeReconcile = buildRuntimeReconcile(reconcileWiringDeps{
			Broker:            deps.ExecBroker,
			Repos:             deps.Repos,
			Notifier:          deps.Notifier,
			Symbol:            symbol,
			Logger:            log,
			EmergencyFlagPath: deps.EmergencyFlagPath,
			LiveMode:          mode,
			PendingTracker:    deps.PendingPositions,
			Counters:          deps.Counters,
			OnPositionClosed:  onPositionClosed,
		})
	}

	worker := &app.Worker{
		Broker:            deps.ExecBroker,
		Aggregator:        aggregator,
		Symbol:            symbol,
		BotConfig:         deps.BotConfig,
		HardLimits:        deps.HardLimits,
		Promoter:          promoter,
		TradingCycle:      tradingCycle,
		Manager:           manager,
		Notifier:          deps.Notifier,
		Trades:            deps.Repos.Trades,
		Positions:         deps.Repos.Positions,
		Candles:           deps.Repos.Candles,
		Counters:          deps.Counters,
		Logger:            log,
		SummaryStore:      summaryStore,
		EmergencyFlagPath: deps.EmergencyFlagPath,
		EventCalendar:     deps.EventCalendar,
		SignalHolder:      deps.SignalHolder,
		EventRetrigger:    deps.EventRetrigger,
	}

	advisorCycle := &command.AdvisorCycle{
		Symbol:          symbol,
		Advisor:         cliAdvisor,
		Promoter:        promoter,
		Notifier:        deps.Notifier,
		AdvisorRuns:     deps.Repos.AdvisorRuns,
		MarketSummaries: deps.Repos.MarketSummaries,
		BotConfig:       deps.BotConfig,
		HardLimits:      deps.HardLimits,
		Logger:          log,
		BuildSummary: func() *market.MarketSummary {
			candles1m := aggregator.Candles(time.Minute)
			candles5m := aggregator.Candles(5 * time.Minute)
			ctxLocal, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tk, _ := deps.PublicBroker.GetTicker(ctxLocal, symbol)
			now := time.Now().UTC()

			// Claude に「過去の自分の判断」と「近接イベント」を渡す。
			var recentDecisions []market.RecentDecision
			if recs, err := deps.Repos.StrategyConfigs.ListRecent(ctxLocal, advisorRecentDecisionsLimit); err == nil {
				recentDecisions = usecase.BuildRecentDecisions(recs, now)
			}
			eventCtx := usecase.BuildEventContext(deps.EventCalendar, now, advisorEventHorizon)

			return usecase.BuildMarketSummary(usecase.BuildMarketSummaryInput{
				Symbol:               symbol,
				Mode:                 mode,
				Now:                  now,
				HardLimits:           deps.HardLimits,
				MaxDailyLossJPY:      deps.BotConfig.Risk.MaxDailyLossJPY,
				MaxConsecutiveLosses: deps.BotConfig.Risk.MaxConsecutiveLosses,
				// Advertise the spread gate that actually governs this summary's main
				// consumer (the LLM decision loop's max_spread_pips, e.g. 3.0) — not the
				// strategy hard-limit ceiling (1.5), which the LLM would mis-cite as a hard cap.
				MaxSpreadPipsOverride: deps.BotConfig.LLMDecision.MaxSpreadPips,
				Candles1m:             candles1m,
				Candles5m:             candles5m,
				Ticker:                tk,
				SpreadSamples:         worker.SpreadHistorySnapshot(),
				BotState:              worker.SnapshotForAdvisor(ctxLocal, getActive()),
				AllowedStrategies:     allowedStrategies,
				RecentDecisions:       recentDecisions,
				EventContext:          eventCtx,
			})
		},
		GetAccountState: func() (config.AccountState, error) {
			openCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return app.BuildPromotionAccountState(openCtx, deps.Repos.Positions, deps.Repos.Trades, app.BuildPromotionAccountStateInput{
				Symbol:               symbol,
				EmergencyFlagPath:    deps.EmergencyFlagPath,
				Timezone:             deps.BotConfig.Bot.Timezone,
				Now:                  time.Now,
				MaxDailyLossJPY:      deps.BotConfig.Risk.MaxDailyLossJPY,
				MaxConsecutiveLosses: deps.BotConfig.Risk.MaxConsecutiveLosses,
				MaxOpenPositions:     deps.BotConfig.Risk.MaxOpenPositions,
			})
		},
	}
	// cycle 完了ごとに status を bot-wide monitor に転送。
	if deps.ParseErrorMonitor != nil {
		advisorCycle.OnRunComplete = func(s port.AdvisorRunStatus, usageLimited bool) {
			deps.ParseErrorMonitor.Record(symbol, s, usageLimited)
		}
	}

	pip := market.PipSize(symbol)
	closeCmd := &command.ClosePositionCommand{
		Mode: mode, Symbol: symbol, PipSize: pip,
		Broker: deps.ExecBroker, Positions: deps.Repos.Positions, Closer: deps.Repos.Closer,
		Mutex: deps.CloseMutex, EmergencyFlagPath: deps.EmergencyFlagPath, Logger: log,
		Clock:    clock.System,
		OnClosed: onPositionClosed,
	}
	manualCmd := &command.ManualTradeCommand{
		Mode: mode, Symbol: symbol, PipSize: pip,
		Broker: deps.ExecBroker, Positions: deps.Repos.Positions,
		EmergencyFlagPath: deps.EmergencyFlagPath, Logger: log,
		EntryMutex:     deps.SharedEntryMutex,
		Admission:      admission,
		ActiveConfig:   getActive,
		HardLimits:     deps.HardLimits,
		PendingTracker: deps.PendingPositions,
	}

	// advisor v2 (signature-breakout, classic chart-breakout model).
	// advisor v2 runs ONLY for allowlisted symbols (advisor_v2.symbols; empty = all);
	// non-allowlisted symbols get a NIL cycle (the scheduler skips them = no v2 entry source
	// for that symbol).
	var signatureCycle *command.SignatureCycle
	if deps.BotConfig.AdvisorV2.V2SymbolEnabled(symbol) {
		// Judge = the LLM go/no-go gate. In deterministic mode it is nil → a found setup auto-enters
		// (only the deterministic rule itself can be backtested; the LLM gate cannot).
		// Safety stays deterministic via executor.OnSignal's risk Gate.
		var v2Judge command.SignatureJudgeFunc
		if !deps.BotConfig.AdvisorV2.Deterministic {
			j := advisor.NewBreakoutJudge(deps.Paths.ClaudeCLI, deps.ProjectRoot,
				deps.BotConfig.AIAdvisor.ClaudeCLITimeoutSeconds, log)
			v2Judge = j.JudgeFunc(symbol)
		}
		v2Qty := deps.HardLimits.Quantity.Min // default min size (1,000)
		if q := deps.BotConfig.AdvisorV2.Quantity; q > 0 {
			v2Qty = q
		}
		signatureCycle = &command.SignatureCycle{
			Symbol:   symbol,
			Pip:      pip,
			Quantity: v2Qty, // advisor_v2.quantity (>=hard_limits.min); per-trade loss guard still applies
			// FK the v2 position to the symbol's REAL active config (else the position INSERT FK-fails
			// -> orphan broker position with no ratchet management). In exclusive mode the engine config
			// does not itself trade, so positions recorded under it are advisor v2's.
			ActiveConfigID: func() string {
				if cfg := getActive(); cfg != nil {
					return cfg.ConfigID
				}
				return ""
			},
			Params:        strategy.DefaultSignatureParams(),
			MaxSpreadPips: deps.BotConfig.AdvisorV2.MaxSpreadPips, // skip wide-spread window (06-07 JST/news)
			MaxConcurrent: deps.BotConfig.AdvisorV2.MaxConcurrent, // 2 = add-to-winner (armed-only); 0/1 = single
			OpenPositions: func(ctx context.Context) ([]port.PositionRecord, error) { // concurrency / pyramid policy input
				return deps.Repos.Positions.ListOpenOrClosing(ctx, symbol)
			},
			DailyCandles: dailyCandleProvider(deps.PublicBroker, symbol, log),
			Judge:        v2Judge,
			GetTicker:    func(ctx context.Context) (*market.Ticker, error) { return deps.PublicBroker.GetTicker(ctx, symbol) },
			BuildSummary: advisorCycle.BuildSummary,
			Submit:       executor.OnSignal,
			Logger:       log,
		}
	}

	// Autonomous LLM trade loop — gated by llm_decision.enabled + allowlist
	// (default OFF). The LLM chooses go/side/TP/SL; the Signal still flows through executor.OnSignal
	// (risk Gate + broker OCO) at the forced qty. A reflection cycle rewrites the playbook it reads.
	var llmDecisionCycle *command.LLMDecisionCycle
	var reflectionCycle *command.ReflectionCycle
	if deps.BotConfig.LLMDecision.SymbolEnabled(symbol) {
		lc := deps.BotConfig.LLMDecision
		llmQty := deps.HardLimits.Quantity.Min // default min size (1,000)
		// Per-symbol size: quantity_by_symbol override → global quantity → floor.
		if q := lc.QuantityFor(symbol); q > 0 {
			llmQty = q
		}
		maxHold := lc.MaxHoldMinutes
		if maxHold <= 0 {
			maxHold = 1440
		}
		minTrades := lc.ReflectionMinTrades
		if minTrades <= 0 {
			minTrades = 20
		}
		pbStore := &playbook.FileStore{Dir: filepath.Join(deps.ProjectRoot, "runtime")}
		pbHolder := &app.PlaybookHolder{}
		if v, ok, lerr := pbStore.Latest(symbol); lerr == nil && ok {
			pbHolder.Set(symbol, v.Rules)
		}
		getPlaybook := func() string { return pbHolder.Get(symbol) }
		savePlaybook := func(rules string) error {
			if err := pbStore.Append(playbook.Version{Symbol: symbol, Rules: rules, CreatedAt: time.Now().UTC()}); err != nil {
				return err
			}
			pbHolder.Set(symbol, rules)
			return nil
		}
		// Armed plans: per-symbol holder (in-memory — a restart clears scenarios by
		// design). Expiry = 1.5× the decision interval so a delayed cycle can't leave a
		// stale scenario armed much past the context it was written in.
		armsHolder := &app.ArmedPlansHolder{}
		armInterval := time.Duration(lc.IntervalMinutes) * time.Minute
		if armInterval <= 0 {
			armInterval = time.Hour
		}
		armExpiry := armInterval * 3 / 2
		decider := advisor.NewLLMDecisionCLI(deps.Paths.ClaudeCLI, deps.ProjectRoot, deps.BotConfig.AIAdvisor.ClaudeCLITimeoutSeconds, log)
		decider.Journal = deps.LLMJournal              // persist raw stdout on a parse fallback
		decider.SingleAgent = lc.SingleAgentDecision() // single agent (default) vs 2-step sub-agent panel
		reflector := advisor.NewReflectionCLI(deps.Paths.ClaudeCLI, deps.ProjectRoot, deps.BotConfig.AIAdvisor.ClaudeCLITimeoutSeconds, log)
		emergencyFlagPath := deps.EmergencyFlagPath
		llmDecisionCycle = &command.LLMDecisionCycle{
			Symbol: symbol, Pip: pip, Quantity: llmQty,
			MaxHoldMinutes: maxHold, RatchetArmPips: lc.RatchetArmPips, RatchetGivebackPips: lc.RatchetGivebackPips,
			MaxSpreadPips: lc.MaxSpreadPips, MaxConcurrent: lc.MaxConcurrent,
			// emergency_stop 中はサイクル冒頭で止め、LLM を呼ばない。
			EmergencyActive: func() bool { return safety.Active(emergencyFlagPath) },
			// MTF veto — all pairs, judged on Summary24h.ChangePips. Unset exemption maps
			// (nil-map lookups → 0) mean no exemption; the mechanism stays wired for
			// per-currency lanes.
			HTFTrendVetoPips:           lc.HTFTrendVetoPips,
			HTFTrendVetoExemptSellRpos: lc.HTFTrendVetoExemptSellRpos[symbol],
			HTFTrendVetoExemptBuyRpos:  lc.HTFTrendVetoExemptBuyRpos[symbol],
			ExcludeHoursJST:            lc.ExcludeHoursJST[symbol], // cede these JST hours to a deterministic strategy (exhaustion_fade)
			// session guard: no_entry_hours_jst (例 02-05 時 JST) の帯は全 side 新規禁止 (05:45 スプレッド壁への
			// 滑走路なし建玉の排除)。cycle は LLM 呼び出し前に skip。
			NoEntryHoursJST: lc.NoEntryHoursJST,
			// Per-currency entry discipline: night-BUY /
			// high-chase-BUY / sell-low vetoes. Map lookups yield 0 for unset symbols = veto OFF.
			NightBuyVetoHoursJST: lc.NightBuyVetoHoursJST,
			MaxRangePos24hBuy:    lc.MaxRangePos24hBuy[symbol],
			MinRangePos24hSell:   lc.MinRangePos24hSell[symbol],
			// Exhaustion veto (same-direction chase after a spent 24h move)
			// and spike cooldown (no entry mid-spike). Unified across all pairs.
			ExhaustionVetoPips: lc.ExhaustionVetoPips,
			SpikeVetoPips15m:   lc.SpikeVetoPips15m,
			// Today's closed trades (06:00 JST trading-day floor) feed the playbook's
			// 同日2敗打ち止め box via the trimmed decision payload's today_closed_trades.
			TodayClosedTrades: func(ctx context.Context) ([]port.TradeRecord, error) {
				return deps.Repos.Trades.ListClosedBySymbolSince(ctx, symbol, market.TradingDayStartJST(time.Now()), 20)
			},
			ActiveConfigID: func() string {
				if cfg := getActive(); cfg != nil {
					return cfg.ConfigID
				}
				return ""
			},
			Playbook:     getPlaybook,
			Decide:       decider.DecideFunc(symbol),
			BuildSummary: advisorCycle.BuildSummary,
			// Arms: the hourly decision stores validated conditional plans; every valid
			// decision replaces them wholesale (ClearArms inside the cycle).
			ArmEnabled:         lc.ArmEnabled,
			ArmMaxDistancePips: lc.ArmMaxDistancePips,
			StoreArms: func(plans []strategy.ArmedPlan) {
				armsHolder.Set(symbol, plans, time.Now().Add(armExpiry))
			},
			ClearArms: func() { armsHolder.Clear(symbol) },
			GetTicker: func(ctx context.Context) (*market.Ticker, error) { return deps.PublicBroker.GetTicker(ctx, symbol) },
			OpenPositions: func(ctx context.Context) ([]port.PositionRecord, error) {
				return deps.Repos.Positions.ListOpenOrClosing(ctx, symbol)
			},
			Submit:  executor.OnSignal,
			Logger:  log,
			Journal: deps.LLMJournal, // append-only history of every cycle outcome
		}
		// Fire path: per-tick watcher executing the armed plans. Shares the exact veto
		// thresholds + submit path with the cycle; fire-time summary comes from the worker's
		// in-memory aggregator (no network in the 1s price loop).
		if lc.ArmEnabled {
			worker.ArmedFire = &command.ArmedFire{
				Symbol: symbol, Pip: pip, Quantity: llmQty,
				MaxHoldMinutes: maxHold, RatchetArmPips: lc.RatchetArmPips, RatchetGivebackPips: lc.RatchetGivebackPips,
				MaxSpreadPips: lc.MaxSpreadPips, MaxConcurrent: lc.MaxConcurrent,
				HTFTrendVetoPips:     lc.HTFTrendVetoPips,
				NightBuyVetoHoursJST: lc.NightBuyVetoHoursJST,
				NoEntryHoursJST:      lc.NoEntryHoursJST, // 発火時刻も同じ帯で拒否
				MaxRangePos24hBuy:    lc.MaxRangePos24hBuy[symbol],
				MinRangePos24hSell:   lc.MinRangePos24hSell[symbol],
				ExhaustionVetoPips:   lc.ExhaustionVetoPips,
				SpikeVetoPips15m:     lc.SpikeVetoPips15m,
				GetArms: func() ([]strategy.ArmedPlan, time.Time, bool) {
					ap, ok := armsHolder.Get(symbol)
					return ap.Plans, ap.ExpiresAt, ok
				},
				ClearArms: func() { armsHolder.Clear(symbol) },
				ActiveConfigID: func() string {
					if cfg := getActive(); cfg != nil {
						return cfg.ConfigID
					}
					return ""
				},
				BuildSummary: func(tk *market.Ticker) *market.MarketSummary {
					return worker.TickSummary(time.Now().UTC(), tk)
				},
				OpenPositions: func(ctx context.Context) ([]port.PositionRecord, error) {
					return deps.Repos.Positions.ListOpenOrClosing(ctx, symbol)
				},
				Submit:  executor.OnSignal,
				Journal: deps.LLMJournal,
				Logger:  log,
			}
		}
		reflectionCycle = &command.ReflectionCycle{
			Symbol: symbol, MinTrades: minTrades,
			RecentTrades: func(ctx context.Context) ([]port.TradeRecord, error) {
				// Floor the window at the LLM loop's go-live (lc.ReflectionStartAt) so reflection
				// only learns from THIS loop's own trades, not prior strategies' trades that share
				// the borrowed active config_id.
				since := lc.ReflectionSince(time.Now().UTC(), 30*24*time.Hour)
				return deps.Repos.Trades.ListClosedBySymbolSince(ctx, symbol, since, 100)
			},
			CurrentPlaybook: getPlaybook,
			Reflect:         reflector.Reflect,
			SavePlaybook:    savePlaybook,
			Logger:          log,
		}
	}

	return &app.SymbolBundle{
		Symbol:               symbol,
		Aggregator:           aggregator,
		Worker:               worker,
		Executor:             executor,
		Manager:              manager,
		Admission:            admission,
		TradingCycle:         tradingCycle,
		StartupReconcile:     startupReconcile,
		RuntimeReconcile:     runtimeReconcile,
		Advisor:              cliAdvisor,
		Promoter:             promoter,
		SummaryStore:         summaryStore,
		AdvisorCycle:         advisorCycle,
		ManualCommand:        manualCmd,
		ClosePositionCommand: closeCmd,
		SignatureCycle:       signatureCycle,
		LLMDecisionCycle:     llmDecisionCycle,
		ReflectionCycle:      reflectionCycle,
	}, nil
}

// dailyCandleProvider returns a func that fetches daily OHLC for the signature detector.
// GMO FX klines use date=YYYY for the 1day interval; GMO FX launched 2023-10 so the last ~3
// years comfortably exceed the 200-SMA + 20-bar slope window. Errors degrade to fewer bars
// (the detector then returns insufficient_history → a safe no-setup), never a crash.
func dailyCandleProvider(pub *broker.GmoBroker, symbol string, log *slog.Logger) func(context.Context) ([]market.Candle, error) {
	return func(ctx context.Context) ([]market.Candle, error) {
		year := time.Now().UTC().Year()
		seen := make(map[int64]bool)
		var out []market.Candle
		for y := year; y >= year-2; y-- {
			ks, err := pub.GetKlines(ctx, symbol, "1day", fmt.Sprintf("%d", y))
			if err != nil {
				log.Warn("v2_daily_klines_fetch_failed", "year", y, "err", err)
				continue
			}
			for _, k := range ks {
				key := k.OpenTime.UnixNano()
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, market.FromKline(k))
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].OpenTime.Before(out[j].OpenTime) })
		// Drop the still-forming current daily bar so the detector only judges a CONFIRMED daily
		// close (matches the backtest; avoids firing on intra-day pokes that reverse).
		out = strategy.CompletedDailyBars(out, time.Now().UTC())
		return out, nil
	}
}

// loadActiveConfigsForBundles populates the ActiveConfigHolder from the DB
// for every bundle. Errors are handled per bundle:
//   - Live: any DB load failure is fail-close (returns error).
//   - Paper: warn and continue with no active config. Only an enabled
//     ai_advisor promotes one automatically; otherwise seed one with
//     scripts/seed_active_config.sh.
func loadActiveConfigsForBundles(
	ctx context.Context,
	bundles map[string]*app.SymbolBundle,
	mode config.Mode,
	holder *app.ActiveConfigHolder,
	logger *slog.Logger,
) error {
	for _, b := range bundles {
		log := logger.With("symbol", b.Symbol)
		cfg, err := b.Promoter.LoadActiveFromDB(ctx, b.Symbol)
		switch {
		case err != nil && mode == config.ModeLiveConfig:
			return fmt.Errorf("active config DB load failed for %s in Live mode: %w", b.Symbol, err)
		case err != nil:
			log.Warn("active_config_db_load_failed_starting_without_active",
				"err", err,
				"hint", "seed an active config (scripts/seed_active_config.sh); only an enabled ai_advisor promotes one automatically")
		case cfg != nil:
			// The active strategy must be whitelisted, else a
			// future advisor cycle would reject/replace this live config.
			if werr := assertActiveStrategyWhitelisted(string(cfg.Strategy.Name)); werr != nil {
				if mode == config.ModeLiveConfig {
					return fmt.Errorf("active config whitelist assert failed for %s: %w", b.Symbol, werr)
				}
				log.Warn("active_config_strategy_not_whitelisted", "err", werr)
			}
			holder.Set(b.Symbol, cfg)
			log.Info("active_config_loaded_from_db",
				"config_id", cfg.ConfigID, "valid_until", cfg.ValidUntil)
		}
	}
	return nil
}

// runStartupReconcileForBundles invokes each bundle's StartupReconcile.Run
// and applies the per-mode fail-close policy:
//   - Live: any error OR any Tripped>0 aborts startup.
//   - Paper: warn and continue.
func runStartupReconcileForBundles(
	ctx context.Context,
	bundles map[string]*app.SymbolBundle,
	mode config.Mode,
	logger *slog.Logger,
) error {
	for _, b := range bundles {
		log := logger.With("symbol", b.Symbol)
		sum, err := b.StartupReconcile.Run(ctx)
		switch {
		case err != nil && mode == config.ModeLiveConfig:
			return fmt.Errorf("startup reconcile failed for %s in Live mode: %w", b.Symbol, err)
		case err != nil:
			log.Warn("startup_reconcile_failed", "err", err)
		default:
			if sum.Adopted+sum.MarkedDone+sum.Tripped > 0 {
				log.Info("startup_reconcile_synced",
					"broker_open", sum.BrokerCount, "db_open", sum.DBCount,
					"adopted", sum.Adopted, "marked_done", sum.MarkedDone, "tripped", sum.Tripped)
			}
			if mode == config.ModeLiveConfig && sum.Tripped > 0 {
				return fmt.Errorf("startup reconcile tripped emergency_stop %d time(s) for %s in Live mode; aborting startup", sum.Tripped, b.Symbol)
			}
		}
	}
	return nil
}
