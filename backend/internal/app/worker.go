package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"fx-bot/backend/internal/app/livesignal"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/risk"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/safety"
	"fx-bot/backend/internal/usecase"
	"fx-bot/backend/internal/usecase/command"
)

// Worker drives the per-tick and per-minute loops:
//
//	priceLoop:  1s — pull ticker → aggregator.OnTick → manage positions
//	               (TP/SL/MaxHold) → evaluate entry → execute order
//	minuteLoop: 1m — pull most-recent kline → aggregator → write summary JSON
//
// Worker depends only on port interfaces + domain types; cmd/bot wires the
// concrete adapters.
type Worker struct {
	Broker     port.Broker
	Aggregator *market.Aggregator
	Symbol     string
	BotConfig  *config.BotConfig
	HardLimits *config.HardLimits
	Promoter   *command.Promoter
	// TradingCycle wraps Evaluator + Executor so the same decision path is
	// reachable from REST ticks, future WebSocket pushes, or test harnesses.
	// Required at construction time — priceTick logs Counters.TradingCycleMissing
	// and skips entry evaluation if nil (defensive, but indicates a wiring bug).
	TradingCycle *command.TradingCycle
	Manager      *command.ManageOpenPositions
	// ArmedFire is the pre-placed (armed) plan executor (nil = feature off). Called once per
	// price tick AFTER position management and the ticker-recovery cooldown; cheap when idle.
	ArmedFire *command.ArmedFire
	// EventRetrigger is the event-driven LLM re-judgment coordinator (nil = feature off).
	// priceTick feeds it the current mid each tick so it can
	// fire an extra all-pairs decision cycle on a big move; internally throttled (10s per
	// symbol) and cooled down, so the per-tick cost is a mutex peek.
	EventRetrigger *command.LLMEventRetrigger

	// Counters surfaces priceTick anomalies (TradingCycleMissing) on /api/status.
	// nil-tolerant for tests.
	Counters  *Counters
	Notifier  port.Notifier
	Trades    port.TradeRepository
	Positions port.PositionRepository
	Candles   port.CandleRepository // nil-tolerant. minuteTick UPSERTs the latest bar per timeframe so restarts can replay (see bootstrap.go).
	Logger    *slog.Logger
	// Clock returns the current time; nil → time.Now. Injected only by tests
	// so the day-boundary (startOfDay) and snapshot timestamps in
	// accountSnapshot are deterministic — without it those tests flake in the
	// ~1h after midnight (bot TZ) when time.Now()-relative fixtures straddle
	// the boundary. Production leaves it nil, so behaviour is unchanged.
	Clock func() time.Time
	// SummaryStore is the artifact store that minuteTick publishes the
	// latest MarketSummary into (consumed by claude CLI). Keeps file I/O out
	// of the usecase layer. nil-tolerant.
	SummaryStore      port.MarketSummaryArtifactStore
	EmergencyFlagPath string // runtime/emergency_stop.flag — read each tick to gate entries
	// EventCalendar は経済指標発表帯 freeze 用の
	// 手書きカレンダー。accountSnapshot が現在時刻で InFreezeWindow を問い合わせ、
	// risk.AccountSnapshot.InEventFreeze に詰める。nil 許容 (= freeze 無効)。
	EventCalendar *config.EventCalendar

	// OnTickerError is called whenever Broker.GetTicker fails. cmd/bot wires
	// this to APIServer.IncrementTickerErrors so /api/status surfaces the count.
	OnTickerError func()

	// SignalHolder receives the latest live evaluation each tick (strategy +
	// gate outcome) so /api/strategy/signal can show the bot's real decision.
	// nil-tolerant (tests / minimal wiring leave it unset).
	SignalHolder *livesignal.Holder

	// single-goroutine state read/written only by priceTick.
	// 連続失敗のカウントと、復旧直後の新規エントリー停止期限を管理する。
	consecutiveTickerErrors int
	entryAllowedAt          time.Time

	// spread history rolling buffer — written by priceTick (single goroutine),
	// read by advisor BuildSummary callback (different goroutine) → mutex required.
	spreadMu      sync.Mutex
	spreadHistory []market.SpreadSample
}

// SpreadHistorySnapshot returns a copy of the rolling per-tick spread samples
// collected over the last 24h. Safe to call from any goroutine.
func (w *Worker) SpreadHistorySnapshot() []market.SpreadSample {
	w.spreadMu.Lock()
	defer w.spreadMu.Unlock()
	out := make([]market.SpreadSample, len(w.spreadHistory))
	copy(out, w.spreadHistory)
	return out
}

// recordSpread appends a sample and drops entries older than 24h. Called by
// priceTick whenever a ticker fetch succeeds.
func (w *Worker) recordSpread(t time.Time, pips float64) {
	w.spreadMu.Lock()
	defer w.spreadMu.Unlock()
	cutoff := t.Add(-24 * time.Hour)
	i := 0
	for i < len(w.spreadHistory) && w.spreadHistory[i].Time.Before(cutoff) {
		i++
	}
	if i > 0 {
		w.spreadHistory = w.spreadHistory[i:]
	}
	w.spreadHistory = append(w.spreadHistory, market.SpreadSample{Time: t, Pips: pips})
}

// TickerRecoveryCooldown は ticker のエラー連発 → 復旧した直後に
// 「新規エントリーを止めて様子を見る」秒数。価格が古い情報で大きく動いた可能性が
// あるので、一拍置いてから取引を再開する。テストから差し替え可能にしておく。
var (
	TickerRecoveryCooldown  = 10 * time.Second
	TickerErrorStreakWarnAt = 3 // この回数連続でエラーになったら WARN ログ
)

// PriceTick interval (1s). Exposed for tests.
var PriceTickInterval = time.Second

// MinuteTick interval (1m).
var MinuteTickInterval = time.Minute

// RunPriceLoop ticks every PriceTickInterval until ctx cancels. It pulls the
// latest ticker, feeds the aggregator, processes any open-position exits,
// evaluates a new entry, and (if approved) places the order.
func (w *Worker) RunPriceLoop(ctx context.Context, getActive func() *config.StrategyConfig) {
	t := time.NewTicker(PriceTickInterval)
	defer t.Stop()
	w.Logger.Info("price_loop_started")
	defer w.Logger.Info("price_loop_stopped")

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		w.priceTick(ctx, getActive)
	}
}

func (w *Worker) priceTick(ctx context.Context, getActive func() *config.StrategyConfig) {
	ticker, err := w.Broker.GetTicker(ctx, w.Symbol)
	if err != nil || ticker == nil {
		w.consecutiveTickerErrors++
		if w.OnTickerError != nil {
			w.OnTickerError()
		}
		// 同じエラーで毎秒ログを出すと騒がしいので、閾値ぴったりの時と
		// その先 10 回ごとだけ WARN。普段は Debug でカウンタに任せる。
		if w.consecutiveTickerErrors == TickerErrorStreakWarnAt ||
			(w.consecutiveTickerErrors > TickerErrorStreakWarnAt && w.consecutiveTickerErrors%10 == 0) {
			w.Logger.Warn("ticker_error_streak", "count", w.consecutiveTickerErrors, "err", err)
		} else {
			w.Logger.Debug("ticker_fetch_error", "err", err)
		}
		return
	}
	// 復旧検知: 直前まで連続エラーが続いていたら、復旧直後にすぐ取引せず
	// クールダウン期間を設けて価格情報が安定するのを待つ。
	if w.consecutiveTickerErrors >= TickerErrorStreakWarnAt {
		w.entryAllowedAt = time.Now().Add(TickerRecoveryCooldown)
		w.Logger.Info("ticker_recovered",
			"previous_error_streak", w.consecutiveTickerErrors,
			"new_entries_paused_until", w.entryAllowedAt.Format(time.RFC3339))
	}
	w.consecutiveTickerErrors = 0

	w.Aggregator.OnTick(*ticker)
	w.recordSpread(ticker.Timestamp, ticker.SpreadPips(market.PipSize(w.Symbol)))

	// Manage open positions first (TP/SL/MaxHold).
	if err := w.Manager.OnTick(ctx, *ticker); err != nil {
		w.Logger.Warn("manage_tick_failed", "err", err)
	}

	// Ticker 復旧直後の冷却期間中は新規エントリーをスキップ。既存ポジション管理は上で済ませる。
	if !w.entryAllowedAt.IsZero() && time.Now().Before(w.entryAllowedAt) {
		return
	}

	// Armed plans: fire the LLM's pre-placed conditional entries the moment the price
	// crosses their trigger (full fire-time re-validation inside; cheap no-op when idle).
	if w.ArmedFire != nil {
		if _, aerr := w.ArmedFire.OnTick(ctx, ticker.Bid, ticker.Ask); aerr != nil {
			w.Logger.Warn("armed_fire_failed", "err", aerr)
		}
	}

	// event_retrigger: let the coordinator see the fresh mid so a big move
	// can fire an extra LLM decision cycle. Candles are passed as a getter — the throttled
	// fast path never pays the aggregator snapshot copy.
	if w.EventRetrigger != nil {
		w.EventRetrigger.OnPriceTick(w.Symbol, (ticker.Bid+ticker.Ask)/2, func() []market.Candle {
			return w.Aggregator.Candles(time.Minute)
		})
	}

	// Evaluate next entry against active config.
	active := getActive()
	if active == nil {
		return
	}
	snap, err := w.accountSnapshot(ctx, active)
	if err != nil {
		w.Logger.Warn("account_snapshot_failed", "err", err)
		return
	}
	now := time.Now().UTC()
	summary := w.buildCurrentMarketSummary(now, ticker, market.BotState{
		TradesInCurrentWindow: snap.TradesInWindow,
		ConsecutiveLosses:     snap.ConsecutiveLosses,
		DailyPnLJPY:           -float64(snap.DailyLossJPY),
	})

	// Hand off the decision to TradingCycle. If wiring is broken (nil),
	// log + counter increment + skip this tick — entry stays paused until
	// the operator restarts with a properly-wired TradingCycle.
	if w.TradingCycle == nil {
		w.Logger.Error("trading_cycle_missing")
		if w.Counters != nil {
			w.Counters.TradingCycleMissing.Add(1)
		}
		return
	}
	res, _ := w.TradingCycle.Execute(ctx, command.TradingCycleInput{
		Now:             now,
		Ticker:          ticker,
		ActiveConfig:    active,
		Summary:         summary,
		Candles1m:       w.Aggregator.Candles(time.Minute),
		Candles5m:       w.Aggregator.Candles(5 * time.Minute),
		Candles1h:       w.Aggregator.Candles(time.Hour),
		AccountSnapshot: snap,
	})
	// Publish the real decision so /api/strategy/signal mirrors what the bot
	// actually decided this tick (strategy + risk gate), not an estimate.
	if w.SignalHolder != nil {
		w.SignalHolder.Set(BuildLiveSignalSnapshot(w.Symbol, now, res, summary, active, w.Aggregator.Candles(5*time.Minute), w.Aggregator.Candles(time.Hour)))
	}
}

// buildCurrentMarketSummary は priceTick / minuteTick の共通 path。
// candles と Mode/HardLimits は worker 状態から、ticker / BotState は caller が
// 用意する。priceTick は ticker を fetch 済みで snap.* から BotState を埋める。
// minuteTick は BotState 0 値 (Mode のみ BuildMarketSummary 内で上書きされる) で
// summary を JSON に書く用途。
func (w *Worker) buildCurrentMarketSummary(now time.Time, ticker *market.Ticker, botState market.BotState) *market.MarketSummary {
	return usecase.BuildMarketSummary(usecase.BuildMarketSummaryInput{
		Symbol:     w.Symbol,
		Mode:       w.BotConfig.Bot.Mode,
		Now:        now,
		HardLimits: w.HardLimits,
		Candles1m:  w.Aggregator.Candles(time.Minute),
		Candles5m:  w.Aggregator.Candles(5 * time.Minute),
		Ticker:     ticker,
		BotState:   botState,
	})
}

// TickSummary builds the lightweight FIRE-TIME market snapshot for the armed-plan
// watcher: in-memory aggregator windows + the given live ticker. No network, no DB — safe
// to call from the 1s price loop (only when a trigger actually crossed).
func (w *Worker) TickSummary(now time.Time, ticker *market.Ticker) *market.MarketSummary {
	return w.buildCurrentMarketSummary(now, ticker, market.BotState{})
}

// 連敗 streak / 直近 SL closed_at / per-side SL カウントは
// command.DeriveTradeAggregates に集約。Worker と EntryAdmission の両方が
// 同じ 1-pass helper を使うことでドリフトを防ぐ。

// now returns the worker clock (time.Now when Clock is unset). Used wherever
// accountSnapshot needs "current time" so tests can pin it deterministically.
func (w *Worker) now() time.Time {
	if w.Clock != nil {
		return w.Clock()
	}
	return time.Now()
}

// accountSnapshot reads the live counters used by the risk gate.
//
// 重要: いずれかの DB 集計が失敗したら snapshot 自体を error 返却する。
// 以前は err を捨てて 0 値で続行していたが、これは DB 一時障害中に
// 「損失 0 / トレード数 0」と誤認させ MaxDailyLossJPY / MaxConsecutiveLosses
// のリミットを実質無効化する穴になっていた。失敗時は entry gate 側で
// その tick の評価をスキップする想定。
func (w *Worker) accountSnapshot(ctx context.Context, active *config.StrategyConfig) (risk.AccountSnapshot, error) {
	open, err := w.Positions.ListOpenOrClosing(ctx, w.Symbol)
	if err != nil {
		return risk.AccountSnapshot{}, fmt.Errorf("list_open: %w", err)
	}
	// External positions (opened directly in the GMO app) are
	// display-only — the bot does not manage their lifecycle. Counting
	// them against per-symbol MaxOpenPositions here would freeze the bot
	// off a single app-side trade. Mirrors EntryAdmission.snapshot;
	// account-wide CountOpenAllSymbols below intentionally still counts
	// externals (margin protection).
	botOpenCount := 0
	// Side-aware counts INCLUDING external for the pyramiding
	// block (mirrors EntryAdmission.snapshot).
	openBuyInclExt := 0
	openSellInclExt := 0
	for _, p := range open {
		switch p.Side {
		case "BUY":
			openBuyInclExt++
		case "SELL":
			openSellInclExt++
		}
		if p.Source.IsExternal() {
			continue
		}
		botOpenCount++
	}
	tradesInWindow := 0
	lossInWindow := 0
	if active != nil && !active.ValidFrom.IsZero() {
		// Per-symbol window aggregates via *BySymbol so a sibling symbol's
		// trades do not pollute this symbol's per-symbol caps.
		tradesInWindow, err = w.Trades.CountClosedBySymbolSince(ctx, w.Symbol, active.ValidFrom)
		if err != nil {
			return risk.AccountSnapshot{}, fmt.Errorf("count_closed_in_window: %w", err)
		}
		lossInWindow, err = w.Trades.SumClosedLossJPYBySymbolSince(ctx, w.Symbol, active.ValidFrom)
		if err != nil {
			return risk.AccountSnapshot{}, fmt.Errorf("sum_closed_loss_in_window: %w", err)
		}
	}
	startOfDay := config.StartOfDayIn(w.now(), w.BotConfig.Bot.Timezone)
	dayLoss, err := w.Trades.SumClosedLossJPYBySymbolSince(ctx, w.Symbol, startOfDay)
	if err != nil {
		return risk.AccountSnapshot{}, fmt.Errorf("sum_closed_loss_day: %w", err)
	}

	// 連続損失カウント: per-symbol で closed_at DESC → 頭から loss を数える。
	recent, err := w.Trades.ListClosedBySymbolSince(ctx, w.Symbol, startOfDay, 50)
	if err != nil {
		return risk.AccountSnapshot{}, fmt.Errorf("list_recent_closed_trades: %w", err)
	}
	agg := command.DeriveTradeAggregates(recent)

	// Account-wide aggregates. Zero AccountMax* in BotConfig disables the
	// account gate; the values are still populated for observability.
	accountOpen, err := w.Positions.CountOpenAllSymbols(ctx)
	if err != nil {
		return risk.AccountSnapshot{}, fmt.Errorf("count_open_all_symbols: %w", err)
	}
	accountDayLoss, err := w.Trades.SumClosedLossJPYSince(ctx, startOfDay)
	if err != nil {
		return risk.AccountSnapshot{}, fmt.Errorf("sum_closed_loss_day_account: %w", err)
	}

	// Cooldown after the most-recent closed trade. HardLimits.Cooldown
	// nil → cooldown disabled (zero values throughout the snapshot).
	inCooldown, cdUntil, cdKind := ComputeCooldown(w.now(), w.HardLimits, w.Trades, ctx)

	return risk.AccountSnapshot{
		EmergencyStop:           safety.Active(w.EmergencyFlagPath),
		OpenPositions:           botOpenCount,
		OpenBuyInclExternal:     openBuyInclExt,
		OpenSellInclExternal:    openSellInclExt,
		TradesInWindow:          tradesInWindow,
		LossInWindowJPY:         lossInWindow,
		DailyLossJPY:            dayLoss,
		MaxDailyLossJPY:         w.BotConfig.Risk.MaxDailyLossJPY,
		ConsecutiveLosses:       agg.ConsecutiveLosses,
		MaxConsecutiveLosses:    w.BotConfig.Risk.MaxConsecutiveLosses,
		AccountOpenPositions:    accountOpen,
		AccountDailyLossJPY:     accountDayLoss,
		AccountMaxOpenPositions: w.BotConfig.Risk.AccountMaxOpenPositions,
		AccountMaxDailyLossJPY:  w.BotConfig.Risk.AccountMaxDailyLossJPY,
		InCooldown:              inCooldown,
		CooldownUntil:           cdUntil,
		CooldownKind:            cdKind,
		Now:                     w.now(),
		LastLossClosedAt:        agg.LastLossClosedAt,
		BuyStopLossesToday:      agg.BuyStopLossesToday,
		SellStopLossesToday:     agg.SellStopLossesToday,
		InEventFreeze:           w.EventCalendar.InFreezeWindowConsideringAdvisor(w.now(), w.BotConfig.AIAdvisor.Enabled),

		ConsecutiveLossGuardsDisabled: w.BotConfig.Risk.DisableConsecutiveLossGuards,
		PostLossFreeze:                time.Duration(w.BotConfig.Risk.PostLossFreezeMinutes) * time.Minute,

		// Reentry cooldown: 同 symbol 同 side は任意の決済から N 分
		// 新規禁止。入力は当日 (bot.timezone の 0:00〜、config.StartOfDayIn) の closed 集計 = agg
		// なので日跨ぎ持ち越しなし (0:00 直前の決済は 0:00 以降の再 IN を塞がない)。
		ReentryCooldown:  time.Duration(w.BotConfig.Risk.ReentryCooldownMinutes) * time.Minute,
		LastBuyClosedAt:  agg.LastBuyClosedAt,
		LastSellClosedAt: agg.LastSellClosedAt,
	}, nil
}

// ComputeCooldown checks the most-recent closed trade against HardLimits.Cooldown
// and returns the current cooldown state. Pure-ish: depends on Trades repo + clock.
// All zero values when cooldown is disabled or no recent trade.
//
// Uses ListClosedSince so we get the most-recent
// CLOSE rather than the most-recent OPEN. A long-held position that closes
// just now must trigger cooldown even if newer trades opened in between.
//
// Exported so EntryAdmission (in usecase/command)
// can call the SAME evaluator via wired closure — eliminates drift between
// worker pre-gate and admission final gate. Reverse-direction import
// (usecase → app) is forbidden, so wiring passes this as a func value.
func ComputeCooldown(now time.Time, hl *config.HardLimits, trades port.TradeRepository, ctx context.Context) (bool, time.Time, string) {
	if hl == nil || hl.Cooldown == nil || trades == nil {
		return false, time.Time{}, ""
	}
	cd := hl.Cooldown
	if cd.AfterEntrySeconds == 0 && cd.AfterLossSeconds == 0 && cd.AfterTakeProfitSeconds == 0 {
		return false, time.Time{}, ""
	}
	recent, err := trades.ListClosedSince(ctx, now.Add(-10*time.Minute), 50)
	if err != nil || len(recent) == 0 {
		return false, time.Time{}, ""
	}
	// ListClosedSince returns ORDER BY closed_at DESC; first row is the
	// freshest close.
	last := recent[0]

	// Pick the longest applicable cooldown for this outcome.
	secs := cd.AfterEntrySeconds
	kind := "after_entry"
	switch last.CloseReason {
	case "stop_loss":
		if cd.AfterLossSeconds > secs {
			secs = cd.AfterLossSeconds
			kind = "after_loss"
		}
	case "take_profit":
		if cd.AfterTakeProfitSeconds > secs {
			secs = cd.AfterTakeProfitSeconds
			kind = "after_take_profit"
		}
	}
	if secs <= 0 {
		return false, time.Time{}, ""
	}
	until := last.ClosedAt.Add(time.Duration(secs) * time.Second)
	if !now.Before(until) {
		return false, time.Time{}, ""
	}
	return true, until, kind
}

// SnapshotForAdvisor builds the BotState block the advisor passes into the
// MarketSummary JSON. Centralises the DB queries so we don't drift between
// risk-gate state and prompt input.
//
// DB エラーは Warn ログを残しつつ zero 値で続行する (Claude に渡す snapshot は
// 補助情報なので、最悪 0 値でも prompt 自体は組める)。ただし accountSnapshot
// が失敗した場合は EmergencyStop=true を強制してリスク側で必ず止める。
func (w *Worker) SnapshotForAdvisor(ctx context.Context, active *config.StrategyConfig) market.BotState {
	snap, snapErr := w.accountSnapshot(ctx, active)
	if snapErr != nil {
		w.Logger.Warn("advisor_snapshot_account_failed_using_safe_defaults", "err", snapErr)
		// Force emergency_stop so the prompt + downstream see "halted" state.
		snap.EmergencyStop = true
	}
	open, oerr := w.Positions.ListOpenOrClosing(ctx, w.Symbol)
	if oerr != nil {
		w.Logger.Warn("advisor_snapshot_list_open_failed", "err", oerr)
	}
	var currentPos *string
	if len(open) > 0 {
		side := open[0].Side
		currentPos = &side
	}
	startOfDay := config.StartOfDayIn(time.Now(), w.BotConfig.Bot.Timezone)
	tradesToday, terr := w.Trades.CountSince(ctx, startOfDay)
	if terr != nil {
		w.Logger.Warn("advisor_snapshot_count_today_failed", "err", terr)
	}
	return market.BotState{
		EmergencyStop:         snap.EmergencyStop,
		CurrentPosition:       currentPos,
		OpenPositionsCount:    snap.OpenPositions,
		DailyPnLJPY:           -float64(snap.DailyLossJPY),
		ConsecutiveLosses:     snap.ConsecutiveLosses,
		TradesToday:           tradesToday,
		TradesInCurrentWindow: snap.TradesInWindow,
	}
}

// RunMinuteLoop fires on each wall-clock minute boundary and updates the
// summary JSON so the advisor has fresh input when it eventually runs.
func (w *Worker) RunMinuteLoop(ctx context.Context, getActive func() *config.StrategyConfig) {
	// Sleep until the next aligned minute.
	now := time.Now()
	next := now.Truncate(time.Minute).Add(time.Minute)
	timer := time.NewTimer(next.Sub(now))
	defer timer.Stop()
	t := time.NewTicker(MinuteTickInterval)
	defer t.Stop()

	w.Logger.Info("minute_loop_started")
	defer w.Logger.Info("minute_loop_stopped")

	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	w.minuteTick(ctx, getActive)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.minuteTick(ctx, getActive)
		}
	}
}

func (w *Worker) minuteTick(ctx context.Context, getActive func() *config.StrategyConfig) {
	// Build summary from current aggregator state and atomically write to disk.
	// getActive() is intentionally unused — the minute summary is a market-only
	// snapshot; risk/bot-state fields are populated by priceTick before each
	// entry evaluation, and by SnapshotForAdvisor when Claude is invoked.
	_ = getActive
	if w.Broker != nil {
		ticker, terr := w.Broker.GetTicker(ctx, w.Symbol)
		if terr != nil || ticker == nil {
			// Ticker 取得失敗時は古い summary をそのまま残す。空 summary を上書
			// きすると Claude が「データ正常 / spread=0」と誤認して config を
			// 生成してしまう。
			w.Logger.Warn("minute_summary_ticker_unavailable_keeping_previous", "err", terr)
		} else {
			summary := w.buildCurrentMarketSummary(time.Now().UTC(), ticker, market.BotState{})
			if w.SummaryStore != nil {
				if err := usecase.PublishMarketSummary(ctx, w.SummaryStore, summary); err != nil {
					w.Logger.Warn("write_summary_failed", "err", err)
				}
			}
		}
	}
	// Persist the most-recent bar per timeframe so a restart can
	// restore from DB without re-fetching from GMO. Nil-tolerant for tests.
	if w.Candles != nil {
		w.upsertLatestCandles(ctx)
	}
}

// candleTimeframes maps Aggregator interval → DB timeframe label. Kept
// alongside minuteTick so the wire format and aggregator keys are visibly
// in sync.
var candleTimeframes = []struct {
	dur time.Duration
	tf  string
}{
	{time.Minute, "1m"},
	{5 * time.Minute, "5m"},
	{15 * time.Minute, "15m"},
	{time.Hour, "1h"},
}

// upsertLatestCandles writes the most-recent closed bar in each tracked
// timeframe. Re-Upsert of the same bar is a cheap no-op on the DB side
// (ON CONFLICT DO UPDATE), so calling every minute regardless of boundary
// is safe.
func (w *Worker) upsertLatestCandles(ctx context.Context) {
	for _, ct := range candleTimeframes {
		bars := w.Aggregator.Candles(ct.dur)
		if len(bars) == 0 {
			continue
		}
		last := bars[len(bars)-1]
		rec := port.CandleRecord{
			Symbol:    w.Symbol,
			Timeframe: ct.tf,
			OpenedAt:  last.OpenTime,
			Open:      last.Open,
			High:      last.High,
			Low:       last.Low,
			Close:     last.Close,
			Volume:    last.Volume,
		}
		if err := w.Candles.Upsert(ctx, rec); err != nil {
			w.Logger.Warn("candle_upsert_failed", "tf", ct.tf, "err", err)
		}
	}
}
