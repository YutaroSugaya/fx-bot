// fx-bot main entry point.
//
// Wiring: env + config + DB + broker + per-symbol bundles + runtime loops
// (see loops.go) + REST API server.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/adapter/advisor"
	"fx-bot/backend/internal/adapter/broker"
	"fx-bot/backend/internal/adapter/journal"
	"fx-bot/backend/internal/adapter/notifier"
	"fx-bot/backend/internal/adapter/repository"
	"fx-bot/backend/internal/app"
	"fx-bot/backend/internal/app/handler"
	"fx-bot/backend/internal/app/livesignal"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/safety"
	"fx-bot/backend/internal/usecase/command"
	"fx-bot/backend/internal/usecase/query"
)

func main() {
	if err := run(); err != nil {
		// 標準 log は使わず stderr + exit (log/slog を混在させない)。
		// slog は run() 内で初期化されるため main() の早期失敗ではまだ使えない。
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runtimePaths は env から読むパス / アドレス群をまとめて持つ。run() が
// 配列のように扱える形でまとめた (個別 string で取り回すと引数が膨らむため)。
type runtimePaths struct {
	BotConfig     string
	HardLimits    string
	ActiveConfig  string
	NextConfig    string
	Prompt        string
	Summary       string
	AIOutDir      string
	EmergencyFlag string
	APIAddr       string
	ClaudeCLI     string
	// EventCalendar は経済指標 freeze カレンダー。既定は bot_config と同じディレクトリの
	// event_calendar.yaml。ファイル不在は空 calendar 扱い (= freeze 無効)。
	EventCalendar string
	// AdvisorV2Status は advisor v2 が毎サイクル書く発火条件スナップショット(trend/trigger/
	// 距離/ATR/stage)の出力先。runLoops が書き、/api/advisor-v2 が同じファイルを返す。
	AdvisorV2Status string
}

func loadRuntimePaths() runtimePaths {
	p := runtimePaths{
		BotConfig:     config.Env("BOT_CONFIG_PATH", "configs/bot_config.yaml"),
		HardLimits:    config.Env("HARD_LIMITS_PATH", "configs/hard_limits.yaml"),
		ActiveConfig:  config.Env("ACTIVE_CONFIG_PATH", "configs/strategy_config.active.yaml"),
		NextConfig:    config.Env("NEXT_CONFIG_PATH", "configs/strategy_config.next.yaml"),
		Prompt:        config.Env("PROMPT_PATH", "prompts/generate_strategy_config.md"),
		Summary:       config.Env("SUMMARY_INPUT_PATH", "runtime/ai_input/latest_summary.json"),
		AIOutDir:      config.Env("AI_OUTPUT_DIR", "runtime/ai_output"),
		EmergencyFlag: config.Env("EMERGENCY_FLAG_PATH", "runtime/emergency_stop.flag"),
		// 既定を loopback に縛る: ":8080" だと 0.0.0.0:8080 で全 interface に bind
		// され外部 expose される。明示的に external 公開したいときだけ
		// API_ADDR=0.0.0.0:8080 等を設定する (非 loopback bind では認証バイパス
		// (DASHBOARD_AUTH_DISABLE 等) が効かず、併用すると起動を拒否する)。
		APIAddr:   config.Env("API_ADDR", "127.0.0.1:8080"),
		ClaudeCLI: config.Env("CLAUDE_CLI_PATH", "claude"),
	}
	// The calendar lives next to bot_config, so derive its default from BOT_CONFIG_PATH: make start
	// runs the bot in backend/ and passes ../configs/…, and a fixed repo-root-relative default would
	// silently load an empty calendar (missing file = no freeze).
	p.EventCalendar = config.Env("EVENT_CALENDAR_PATH",
		filepath.Join(filepath.Dir(p.BotConfig), "event_calendar.yaml"))
	// Absolute path so the scheduler (writer) and /api/advisor-v2 (reader) agree regardless of CWD.
	p.AdvisorV2Status = config.Env("ADVISOR_V2_STATUS_PATH",
		filepath.Join(resolveProjectRoot(p), "runtime", "advisor_v2_status.json"))
	return p
}

func ensureRuntimeDirs(p runtimePaths) {
	for _, d := range []string{filepath.Dir(p.Summary), p.AIOutDir, filepath.Dir(p.EmergencyFlag)} {
		if d != "" {
			_ = os.MkdirAll(d, 0o755)
		}
	}
}

// setupBrokers は paper 用の public price source + paper executor を組む。
// publicBroker は ticker/klines 専用で、paper モードでは private endpoint を
// 叩かないので auth 無しで動く。
//
// hardLimits.Paper があれば、PaperBroker にスリッページ / 手数料を注入して
// Live と Paper の PnL 乖離を縮める。
func setupBrokers(botCfg *config.BotConfig, hardLimits *config.HardLimits, logger *slog.Logger) (*broker.GmoBroker, *broker.PaperBroker) {
	pub := broker.NewGmoBroker(broker.GmoBrokerConfig{
		PublicBaseURL:  botCfg.GMO.PublicBaseURL,
		PrivateBaseURL: botCfg.GMO.PrivateBaseURL,
		Logger:         logger,
	})
	var slipPips, feeJPY float64
	if hardLimits != nil && hardLimits.Paper != nil {
		slipPips = hardLimits.Paper.SimulatedSlippagePips
		feeJPY = hardLimits.Paper.APIFeeJPYPerTrade
	}
	paper := broker.NewPaperBroker(broker.PaperBrokerConfig{
		SlippagePips:   slipPips,
		FeeJPYPerTrade: feeJPY,
		Pricer: func(ctx context.Context, sym string) (float64, float64, error) {
			tk, err := pub.GetTicker(ctx, sym)
			if err != nil {
				return 0, 0, err
			}
			return tk.Bid, tk.Ask, nil
		},
	})
	return pub, paper
}

// restorePaperPositions は再起動直後に DB の OPEN ポジションを PaperBroker
// のメモリへ戻す。戻さないと TP/SL/MaxHold で ClosePosition が "unknown
// position" になり決済が通らない。失敗しても起動は止めない (warn のみ)。
//
// CLOSING の row は restore しない。CLOSING は
// 前回の close saga が途中で落ちた痕跡で、reconcile の stale-DB 分岐
// (broker にいないが DB にいる → CloseAndRecord で synthetic close) で
// finalise する。ここで broker に OPEN として戻してしまうと reconcile が
// "broker にもいる" 判定で skip し、ManageOpenPositions も CLOSING を skip
// するため永久に stuck になる。
//
// BrokerPositionID は positions 本体ではなく positions_live junction に
// 入っているので、行ごとに GetLive で引き直す。Paper でも broker は ID を
// 振る (broker.PaperBroker.PlaceOrder で OrderID 採番) ので、Live junction
// が無い paper position は基本的に存在しないが、欠落していても restore は
// 続行する (ClosePosition の引き当てだけが効かなくなる)。
func restorePaperPositions(ctx context.Context, paper *broker.PaperBroker, repo port.PositionRepository, symbol string, logger *slog.Logger) {
	openRecs, err := repo.ListOpenOrClosing(ctx, symbol)
	if err != nil {
		logger.Warn("position_restore_failed", "err", err)
		return
	}
	if len(openRecs) == 0 {
		return
	}
	restored := make([]position.Position, 0, len(openRecs))
	for _, p := range openRecs {
		// Skip CLOSING — reconcile will finalise
		// it via the stale-DB branch (synthetic close in paper mode).
		if p.Status != port.PositionStatusOpen {
			logger.Info("position_restore_skipped_non_open",
				"id", p.ID, "status", string(p.Status))
			continue
		}
		brokerID := ""
		if live, lerr := repo.GetLive(ctx, p.ID); lerr != nil {
			logger.Warn("position_restore_live_lookup_failed", "id", p.ID, "err", lerr)
		} else if live != nil {
			brokerID = live.BrokerPositionID
		}
		restored = append(restored, position.Position{
			ID:               p.ID,
			BrokerPositionID: brokerID,
			Symbol:           p.Symbol,
			Side:             order.Side(p.Side),
			Quantity:         p.Quantity,
			EntryPrice:       p.EntryPrice,
			TakeProfitPips:   p.TakeProfitPips,
			StopLossPips:     p.StopLossPips,
			MaxHoldMinutes:   p.MaxHoldMinutes,
			StrategyConfigID: p.StrategyConfigID,
			Status:           position.StatusOpen,
			OpenedAt:         p.OpenedAt,
		})
	}
	paper.Restore(restored)
	logger.Info("paper_positions_restored", "count", len(restored))
}

// setupCLIAdvisor は claude CLI advisor adapter を組み、WorkingDir を
// プロジェクトルートに固定する。BOT_PROJECT_ROOT で override 可能、
// 無ければ promptPath の絶対パスから 2 階層上 (= リポジトリルート) を採用。
func setupCLIAdvisor(p runtimePaths, botCfg *config.BotConfig, logger *slog.Logger) *advisor.ClaudeCLIAdvisor {
	a := advisor.New(p.ClaudeCLI, p.Prompt, p.AIOutDir, p.NextConfig, botCfg.AIAdvisor.ClaudeCLITimeoutSeconds)
	a.Logger = logger
	a.WorkingDir = resolveProjectRoot(p)
	return a
}

// resolveProjectRoot returns the repo root used as the advisor subprocess CWD (so subagents can
// Read .claude/agents + prompts). BOT_PROJECT_ROOT overrides; else the prompt path's 2-up dir.
func resolveProjectRoot(p runtimePaths) string {
	if root := config.Env("BOT_PROJECT_ROOT", ""); root != "" {
		return root
	}
	if absPrompt, err := filepath.Abs(p.Prompt); err == nil {
		return filepath.Dir(filepath.Dir(absPrompt))
	}
	return ""
}

func run() error {
	_ = config.LoadDotEnv(".env")
	logger := config.NewLogger()

	paths := loadRuntimePaths()

	botCfg, err := config.LoadBotConfig(paths.BotConfig)
	if err != nil {
		return err
	}
	// effective mode と source を起動時に明示。
	// `BOT_CONFIG_PATH=configs/bot_config.live.yaml` を渡したかどうかを
	// この 1 行で確認できる (= うっかり Live の早期検出)。
	logger.Info("bot_mode_effective",
		"mode", string(botCfg.Bot.Mode),
		"source", paths.BotConfig)
	hardLimits, err := config.LoadHardLimits(paths.HardLimits)
	if err != nil {
		return err
	}
	// Cross-validate that every configured symbol is whitelisted in
	// hard_limits.allowed_symbols. Fail-close on YAML drift.
	if err := botCfg.ValidateAgainstHardLimits(hardLimits); err != nil {
		return fmt.Errorf("bot_config / hard_limits mismatch: %w", err)
	}
	symbols := botCfg.ResolveSymbols()
	primaryOnly := len(symbols) == 1
	primarySymbol := symbols[0]

	// Disable mode short-circuit: log and just wait.
	if botCfg.Bot.Mode == config.ModeDisabled {
		logger.Info("bot_disabled_mode")
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		<-ctx.Done()
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ----- DB -----
	pool, err := connectDB(ctx)
	if err != nil {
		return fmt.Errorf("db: %w", err)
	}
	defer pool.Close()
	repos := repository.NewRepositories(pool)

	// ----- broker -----
	publicBroker, paperBroker := setupBrokers(botCfg, hardLimits, logger)

	// Live mode guard + exec broker selection を helper に集約。
	// botCfg.Bot.Mode は副作用的に書き換わる (Live 不可なら paper に降格) ことに注意。
	execBroker, err := selectExecBroker(botCfg, hardLimits, paperBroker, logger)
	if err != nil {
		return err
	}

	// ----- shared cross-symbol state -----
	validator := config.NewValidator(hardLimits)
	stdoutNotifier := notifier.NewStdout(logger)

	engine := strategy.NewEngine()
	// Domain stays deterministic — id generator lives at wiring.
	engine.SignalIDFn = newSignalIDGenerator()

	// closeMu serializes broker.ClosePosition across the price loop and API
	// close paths (prevents double-close races).
	var closeMu sync.Mutex
	// sharedEntryMu serializes ALL entries account-wide — both per-symbol
	// auto signals AND manual entries across every symbol. Shared by every
	// bundle's ExecuteOrder.EntryMutex AND EntryAdmission.Mutex so
	// account-wide caps can serialize concurrent multi-symbol
	// entry attempts.
	var sharedEntryMu sync.Mutex

	holder := &app.ActiveConfigHolder{}

	// Counters is read by every bundle's worker + API server + safety hook.
	counters := &app.Counters{}
	safety.SetOnTrip(func(reason string) {
		counters.EmergencyTrips.Add(1)
		logger.Warn("emergency_trip_counted", "reason", reason,
			"total_trips", counters.EmergencyTrips.Load())
	})

	// Pending position tracker: 1 instance per bot process, shared across
	// every symbol bundle so reconcile sees the same entry-saga-in-flight
	// state as ExecuteOrder / ManualTradeCommand (closes the entry-saga race window).
	pendingPositions := safety.NewPendingPositions()

	// bot-wide parse_error monitor。各 bundle の AdvisorCycle が
	// OnRunComplete でここに status を転送し、scheduler の
	// nextScheduleInterval が「parse_error 中なら 10 分 cadence」で再走する。
	parseErrorMonitor := app.NewParseErrorMonitor()

	// 経済指標 freeze 用の手書きカレンダー。
	// ファイル不在は空 calendar (= freeze 無効) で起動。Worker / EntryAdmission
	// の snapshot に同じ instance を渡す。カレンダーは運用者が手で更新する。
	eventCalendar, err := config.LoadEventCalendar(paths.EventCalendar)
	if err != nil {
		return fmt.Errorf("load event calendar: %w", err)
	}
	logger.Info("event_calendar_loaded",
		"path", paths.EventCalendar, "events", len(eventCalendar.Events))

	// ----- per-symbol bundle construction -----
	// Shared live-signal store: workers write the real per-tick decision,
	// /api/strategy/signal reads it. One per bot process.
	liveSignalHolder := livesignal.NewHolder()
	// Single shared decision journal (append-only history of every LLM cycle + parse-fallback),
	// one instance across all pairs so its write mutex genuinely serialises the parallel cycles.
	llmJournal := &journal.FileJournal{
		Path: filepath.Join(resolveProjectRoot(paths), "runtime", "logs", "llm_decisions.jsonl"),
	}
	// event_retrigger: 決済確定・急変動のイベントで
	// LLM 再判断を追加起動するコーディネータ。全 bundle で 1 instance を共有
	// (cooldown を global に効かせる)。Trigger は手動ボタンと同じ経路 (週末ゲート +
	// llmCycleRunning ガード) なので、定期サイクル実行中のイベントは拒否される =
	// 「1時間の定期が動いていたらイベント側はストップ」。Trigger の実体は bundles
	// 構築後 (triggerLLM 定義後) に配線する — それまでは nil-safe に no-op。
	var eventRetrigger *command.LLMEventRetrigger
	if erCfg := botCfg.LLMDecision.EventRetrigger; botCfg.LLMDecision.Enabled && erCfg.Enabled &&
		(erCfg.OnPositionClose || erCfg.MovePips > 0) {
		eventRetrigger = &command.LLMEventRetrigger{
			OnPositionClose: erCfg.OnPositionClose,
			MovePips:        erCfg.MovePips,
			MoveWindow:      erCfg.MoveWindow(),
			MoveCooldown:    erCfg.MoveCooldown(),
			CloseCooldown:   erCfg.CloseCooldown(),
			Logger:          logger,
		}
		logger.Info("llm_event_retrigger_enabled",
			"on_position_close", erCfg.OnPositionClose,
			"move_pips", erCfg.MovePips,
			"move_window", erCfg.MoveWindow().String(),
			"move_cooldown", erCfg.MoveCooldown().String(),
			"close_cooldown", erCfg.CloseCooldown().String())
	}

	bundleDeps := bundleWiringDeps{
		BotConfig:         botCfg,
		HardLimits:        hardLimits,
		Paths:             paths,
		LLMJournal:        llmJournal,
		Validator:         validator,
		Holder:            holder,
		Repos:             repos,
		PublicBroker:      publicBroker,
		ExecBroker:        execBroker,
		Notifier:          stdoutNotifier,
		Engine:            engine,
		Counters:          counters,
		CloseMutex:        &closeMu,
		SharedEntryMutex:  &sharedEntryMu,
		Logger:            logger,
		EmergencyFlagPath: paths.EmergencyFlag,
		PendingPositions:  pendingPositions,
		ParseErrorMonitor: parseErrorMonitor,
		EventCalendar:     eventCalendar,
		SignalHolder:      liveSignalHolder,
		ProjectRoot:       resolveProjectRoot(paths),
		EventRetrigger:    eventRetrigger,
	}
	bundles := make(map[string]*app.SymbolBundle, len(symbols))
	for _, sym := range symbols {
		b, err := buildSymbolBundle(ctx, sym, bundleDeps, primaryOnly)
		if err != nil {
			return fmt.Errorf("build bundle %s: %w", sym, err)
		}
		bundles[sym] = b
	}

	// Startup reconcile per bundle (fail-close on Live tripped/error).
	if err := runStartupReconcileForBundles(ctx, bundles, botCfg.Bot.Mode, logger); err != nil {
		return err
	}

	// Load active configs per bundle from DB into the holder.
	if err := loadActiveConfigsForBundles(ctx, bundles, botCfg.Bot.Mode, holder, logger); err != nil {
		return err
	}

	// At SIZE (effective qty >= 10000) Live must NOT run with
	// the loss-streak guards disabled or no after-loss cooldown. Keyed off the
	// effective (largest active) qty, so qty=1000 screening is unaffected.
	afterLossCD := 0
	if hardLimits.Cooldown != nil {
		afterLossCD = hardLimits.Cooldown.AfterLossSeconds
	}
	if err := app.AssertLiveQtyGuards(botCfg.Bot.Mode, app.MaxActiveConfigQuantity(holder),
		botCfg.Risk.DisableConsecutiveLossGuards, afterLossCD); err != nil {
		return err
	}

	// Single-bundle API objects (the ask-claude summary store) come from the
	// primary symbol's bundle.
	primary := bundles[primarySymbol]

	// advisorFireMu serializes manual-trigger AND scheduler-tick fires.
	// Within a single fire each bundle's AdvisorCycle runs in parallel via
	// RunAdvisorCyclesParallel; this mutex only prevents two fires from
	// overlapping (which would race on holder.Set / Claude CLI credit).
	var advisorFireMu sync.Mutex

	// runBundleAdvisor invokes one bundle's AdvisorCycle and updates the
	// holder slot on a successful promotion. Used both as the per-symbol
	// callable for parallel fires and as the single-bundle path for manual
	// triggers.
	runBundleAdvisor := func(b *app.SymbolBundle) func(ctx context.Context, src port.AdvisorRunSource) (*command.PromotionResult, error) {
		return func(ctx context.Context, src port.AdvisorRunSource) (*command.PromotionResult, error) {
			res, err := b.AdvisorCycle.Run(ctx, src)
			if err == nil && res != nil && res.Promoted && res.Parsed != nil {
				holder.Set(res.Parsed.Symbol, res.Parsed)
			}
			return res, err
		}
	}

	// advisorOpts builds the parallel-fire options shared by both fire paths
	// (scheduler tick + manual trigger): the YAML overrides for per-symbol
	// timeout and max-concurrency win, else the ClaudeCLITimeoutSeconds+30s
	// grace (so the per-symbol deadline is always strictly longer than Claude's
	// own timeout) and the bundle-count heuristic. Centralized so the two fire
	// paths cannot drift.
	advisorOpts := func() app.AdvisorParallelOpts {
		perSymbolTimeout := time.Duration(botCfg.AIAdvisor.ClaudeCLITimeoutSeconds+30) * time.Second
		if t := botCfg.AIAdvisor.PerSymbolTimeoutSeconds; t > 0 {
			perSymbolTimeout = time.Duration(t) * time.Second
		}
		maxConcurrent := advisorMaxConcurrent(len(bundles))
		if n := botCfg.AIAdvisor.MaxConcurrentSymbols; n > 0 {
			maxConcurrent = n
		}
		return app.AdvisorParallelOpts{
			PerSymbolTimeout: perSymbolTimeout,
			MaxConcurrent:    maxConcurrent,
			Logger:           logger,
		}
	}

	// fireAllAdvisorsParallel is the scheduler-tick entry point. Locks the
	// fire mutex, dispatches every bundle in parallel with per-symbol
	// timeouts, and returns when all bundles finish (success or per-symbol
	// error/timeout).
	fireAllAdvisorsParallel := func(ctx context.Context, src port.AdvisorRunSource) {
		advisorFireMu.Lock()
		defer advisorFireMu.Unlock()
		runners := make(map[string]app.AdvisorRunner, len(bundles))
		for sym, b := range bundles {
			run := runBundleAdvisor(b)
			runners[sym] = func(c context.Context) error {
				res, err := run(c, src)
				if err == nil && res != nil && res.Promoted {
					logger.Info("active_config_swapped",
						"symbol", b.Symbol, "config_id", res.ConfigID, "source", string(src))
				}
				return err
			}
		}
		app.RunAdvisorCyclesParallel(ctx, runners, advisorOpts(), src)
	}

	// triggerResultFromPromotion は単一 symbol fire の PromotionResult を
	// handler.TriggerResult に変換するヘルパ。symbol param 経由で fire した
	// パスでも fan-out の per-symbol path でも同じ形に揃える。
	triggerResultFromPromotion := func(symbol string, res *command.PromotionResult) handler.TriggerResult {
		out := handler.TriggerResult{Symbol: symbol}
		if res == nil {
			out.RejectReason = "advisor returned no result (claude failed or timed out)"
			return out
		}
		out.Promoted = res.Promoted
		out.ConfigID = res.ConfigID
		out.RejectReason = res.RejectReason
		if res.Parsed != nil {
			out.Strategy = string(res.Parsed.Strategy.Name)
			out.Enabled = res.Parsed.Enabled
		}
		return out
	}

	// triggerAdvisor (manual API) — symbol="" なら全 bundle を並列 fire し
	// PerSymbol に結果を詰める。symbol 指定時はその bundle のみ発火。
	//
	// Top-level (Promoted/ConfigID/...) は後方互換のため、symbol 指定時は
	// その symbol の結果、fan-out 時は primarySymbol の結果を入れる。
	triggerAdvisor := func(c context.Context, symbol string) (handler.TriggerResult, error) {
		advisorFireMu.Lock()
		defer advisorFireMu.Unlock()
		// Single-symbol path.
		if symbol != "" {
			b, ok := bundles[symbol]
			if !ok {
				return handler.TriggerResult{}, fmt.Errorf("unknown symbol %q (configured: %v)", symbol, symbols)
			}
			res, err := runBundleAdvisor(b)(c, port.AdvisorRunSourceManual)
			if err != nil {
				return handler.TriggerResult{Symbol: symbol}, err
			}
			return triggerResultFromPromotion(symbol, res), nil
		}
		// Fan-out: every bundle fires in parallel, each into its own slot.
		type slot struct {
			res *command.PromotionResult
			err error
		}
		results := make(map[string]*slot, len(bundles))
		var resMu sync.Mutex
		runners := make(map[string]app.AdvisorRunner, len(bundles))
		for sym, b := range bundles {
			run := runBundleAdvisor(b)
			results[sym] = &slot{}
			runners[sym] = func(cc context.Context) error {
				r, e := run(cc, port.AdvisorRunSourceManual)
				resMu.Lock()
				results[sym].res = r
				results[sym].err = e
				resMu.Unlock()
				return nil
			}
		}
		app.RunAdvisorCyclesParallel(c, runners, advisorOpts(), port.AdvisorRunSourceManual)

		out := handler.TriggerResult{
			Symbol:    primarySymbol,
			PerSymbol: make(map[string]handler.SymbolTriggerInfo, len(results)),
		}
		for sym, s := range results {
			info := handler.SymbolTriggerInfo{}
			if s.err != nil {
				info.Error = s.err.Error()
			} else if s.res != nil {
				info.Promoted = s.res.Promoted
				info.ConfigID = s.res.ConfigID
				info.RejectReason = s.res.RejectReason
				if s.res.Parsed != nil {
					info.Strategy = string(s.res.Parsed.Strategy.Name)
					info.Enabled = s.res.Parsed.Enabled
				}
			} else {
				info.RejectReason = "advisor returned no result (claude failed or timed out)"
			}
			out.PerSymbol[sym] = info
		}
		// Top-level alias = primary symbol の結果 (互換のため)。
		if p, ok := out.PerSymbol[primarySymbol]; ok {
			out.Promoted = p.Promoted
			out.ConfigID = p.ConfigID
			out.Strategy = p.Strategy
			out.Enabled = p.Enabled
			out.RejectReason = p.RejectReason
		}
		return out, nil
	}

	// CQRS Query (primary bundle's summary store).
	askClaudeQuery := &query.AskClaudeQuery{
		SummaryStore: primary.SummaryStore,
		Runner:       newClaudeRunner(paths.ClaudeCLI, botCfg.AIAdvisor.ClaudeCLITimeoutSeconds, logger),
		Logger:       logger,
	}

	manualCommands := make(map[string]*command.ManualTradeCommand, len(bundles))
	closeCommands := make(map[string]*command.ClosePositionCommand, len(bundles))
	for sym, b := range bundles {
		manualCommands[sym] = b.ManualCommand
		closeCommands[sym] = b.ClosePositionCommand
	}
	// Manual "全ペア再判断" button (POST /api/llm-decision/trigger): run one LLM decision
	// cycle for ALL pairs NOW, sharing the scheduler's path/interval/bundles + run-guard.
	llmStatusPath := filepath.Join(filepath.Dir(paths.AdvisorV2Status), "llm_decision_status.json")
	llmInterval := time.Duration(botCfg.LLMDecision.IntervalMinutes) * time.Minute
	triggerLLM := func() error {
		if !botCfg.LLMDecision.Enabled {
			return fmt.Errorf("LLM判断ループが無効です(llm_decision.enabled=false)")
		}
		return triggerLLMDecisionCycle(ctx, llmStatusPath, llmInterval, bundles, logger)
	}
	// event_retrigger: fire through the SAME path as the manual button (weekend gate +
	// llmCycleRunning run-guard inside triggerLLMDecisionCycle). The coordinator logs the
	// event reason itself (llm_event_retrigger_fired / _refused).
	if eventRetrigger != nil {
		eventRetrigger.Trigger = func(string) error { return triggerLLM() }
	}
	apiSrv := wireHTTPHandlers(httpWiringInput{
		BotConfig:      botCfg,
		Paths:          paths,
		Logger:         logger,
		Counters:       counters,
		Repos:          repos,
		PublicBroker:   publicBroker,
		Holder:         holder,
		CloseCommands:  closeCommands,
		ManualCommands: manualCommands,
		AskClaudeQuery: askClaudeQuery,
		TriggerAdvisor: triggerAdvisor,
		TriggerLLM:     triggerLLM,
		SignalHolder:   liveSignalHolder,
		StartedAt:      time.Now(),
		Auth:           buildAPIAuth(botCfg, logger),
		CORS:           buildAPICORS(),
		AllowedHosts:   buildAllowedHosts(),
	})
	// Wire ticker_errors counter to every bundle's worker.
	for _, b := range bundles {
		b.Worker.OnTickerError = apiSrv.IncrementTickerErrors
	}

	sched := configureScheduler(botCfg, logger, holder, parseErrorMonitor)

	ensureRuntimeDirs(paths)

	logger.Info("bot_started",
		"mode", botCfg.Bot.Mode,
		"symbols", symbols,
		"api_addr", paths.APIAddr,
		"interval_minutes", botCfg.AIAdvisor.IntervalMinutes,
	)

	// 監視 alert ループ: 日次サマリ / 当日DD超 / N時間ノートレードを Notifier へ。
	// 判定は app の純粋関数 (テスト済み)。stdout Notifier なので現状はログ出力。
	opsAlert := func(c context.Context) {
		runOpsAlertLoop(c, opsAlertConfig{
			Logger:          logger,
			Notifier:        stdoutNotifier,
			Trades:          repos.Trades,
			Symbols:         symbols,
			Loc:             config.LoadTimezoneOrUTC(botCfg.Bot.Timezone),
			MaxDailyLossJPY: botCfg.Risk.MaxDailyLossJPY,
			NoTradeAfter:    opsNoTradeAfter,
			SummaryHour:     opsSummaryHour,
			Interval:        opsAlertInterval,
			EdgeWindowDays:  opsEdgeWindowDays,
			EdgeLimit:       opsEdgeLimit,
		})
	}

	// Assurance log: whether Claude (LLM) can be invoked AUTOMATICALLY. The LLM auto-runs ONLY when
	// the v1 advisor is enabled, OR advisor v2 is enabled in non-deterministic (LLM-judge) mode.
	// In deterministic v2 + advisor-off, neither advisor path starts a Claude process. The
	// autonomous LLM decision loop (llm_decision.enabled, opt-in) is a separate path and logs
	// its own llm_decision_scheduler_started / llm_decision_disabled_scheduler_not_started line.
	claudeAutoRuns := botCfg.AIAdvisor.Enabled || (botCfg.AdvisorV2.Enabled && !botCfg.AdvisorV2.Deterministic)
	logger.Info("advisor_llm_status",
		"v1_advisor_enabled", botCfg.AIAdvisor.Enabled,
		"v2_enabled", botCfg.AdvisorV2.Enabled,
		"v2_deterministic", botCfg.AdvisorV2.Deterministic,
		"claude_auto_invocation", claudeAutoRuns,
		"note", "Claude(LLM) auto-runs only when v1_advisor_enabled OR (v2_enabled AND NOT v2_deterministic); /api/ask-claude stays manual-only")

	// ----- per-symbol price/minute/reconcile loops + scheduler + API (see loops.go) -----
	runLoops(ctx, logger, bundles, sched, apiSrv, fireAllAdvisorsParallel, botCfg.AIAdvisor.Enabled,
		botCfg.AdvisorV2.Enabled, time.Duration(botCfg.AdvisorV2.IntervalMinutes)*time.Minute,
		paths.AdvisorV2Status,
		botCfg.LLMDecision.Enabled,
		// Reflexion loop gate: decision loop AND reflection_enabled. reflection_enabled:false
		// keeps the playbook from ever being rewritten autonomously.
		botCfg.LLMDecision.Enabled && botCfg.LLMDecision.ReflectionOn(),
		llmInterval,
		time.Duration(botCfg.LLMDecision.ReflectionIntervalMinutes)*time.Minute,
		llmStatusPath,
		opsAlert)
	return nil
}

// advisorMaxConcurrent caps simultaneous Claude CLI invocations during a
// scheduler-fire fan-out. Single-bundle setups don't need a limit.
// Multi-symbol setups cap at half the bundle count (rounded up) to keep
// credit spikes and OS process count predictable; minimum 2 so 2-symbol
// runs go fully parallel.
func advisorMaxConcurrent(n int) int {
	if n <= 1 {
		return 0
	}
	half := (n + 1) / 2
	if half < 2 {
		return 2
	}
	return half
}

func connectDB(ctx context.Context) (*pgxpool.Pool, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, errors.New("DATABASE_URL not set")
	}
	connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(connectCtx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(connectCtx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// PaperBroker が port.Broker を実装し続けることをコンパイル時に保証する。
var _ port.Broker = (*broker.PaperBroker)(nil)

// newClaudeRunner は query.AskClaudeQuery に渡す PromptRunner を組み立てる。
// claude CLI を `-p` モードで起動して stdin にプロンプトを流し込み、stdout を
// 返す。timeout は bot_config の ClaudeCLITimeoutSeconds、未指定なら 120 秒。
func newClaudeRunner(cliPath string, timeoutSeconds int, logger *slog.Logger) query.PromptRunner {
	if cliPath == "" {
		cliPath = "claude"
	}
	timeout := 120
	if timeoutSeconds > 0 {
		timeout = timeoutSeconds
	}
	return func(ctx context.Context, prompt string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
		// 入力はダッシュボード経由の任意文 → ツール無し(--tools "")で起動し、
		// プロンプトインジェクションからファイル読取/コマンド実行に至らせない。
		// bot の秘密(broker の API キー・DSN・ダッシュボードの認証)は渡さず、repo の hooks も走らせない。
		args := append([]string{"-p", "--no-session-persistence", "--tools", ""}, advisor.ClaudeIsolationArgs()...)
		cmd := exec.CommandContext(cctx, cliPath, args...)
		cmd.Env = advisor.ClaudeEnv(os.Environ())
		cmd.Stdin = strings.NewReader(prompt)
		if root := config.Env("BOT_PROJECT_ROOT", ""); root != "" {
			cmd.Dir = root
		}
		var out, stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			logger.Error("ask_claude_cli_error", "err", err, "stderr", stderr.String())
			return "", fmt.Errorf("claude CLI: %w", err)
		}
		return strings.TrimSpace(out.String()), nil
	}
}
