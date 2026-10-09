package main

import (
	"context"
	"log/slog"
	"time"

	"fx-bot/backend/internal/app"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/usecase/command"
)

// ops_alert_loop.go — 放置運用の監視ループ。定期的に per-symbol の
// 当日損益 / 最終約定からの経過 / edge 指標を集計し、判定 (app.DailyDDAlert /
// NoTradeAlert / DailySummaryEvent) に通して Notifier へ emit する。
//
// 判定ロジックは app パッケージの純粋関数 (テスト済み)。本ループは I/O と
// 頻度制御 (1 日 1 回 / dedup) の glue。
//
// 注意: Notifier 実体は現状 stdout (slog)。emit はログに出る。実際に
// スマホへ push するには LINE / Slack webhook の Notifier アダプタを足す。

const (
	opsAlertInterval  = 30 * time.Minute
	opsNoTradeAfter   = 6 * time.Hour
	opsSummaryHour    = 7 // bot-TZ 07:00 以降に日次サマリを 1 回
	opsEdgeWindowDays = 90
	opsEdgeLimit      = 30
)

type opsAlertConfig struct {
	Logger          *slog.Logger
	Notifier        port.Notifier
	Trades          port.TradeRepository
	Symbols         []string
	Loc             *time.Location
	MaxDailyLossJPY int
	NoTradeAfter    time.Duration
	SummaryHour     int
	Interval        time.Duration
	EdgeWindowDays  int
	EdgeLimit       int
	Now             func() time.Time // テスト用; nil → time.Now
}

// runOpsAlertLoop は ctx が切れるまで Interval ごとに監視チェックを回す。
func runOpsAlertLoop(ctx context.Context, cfg opsAlertConfig) {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = opsAlertInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	cfg.Logger.Info("ops_alert_loop_started",
		"interval", interval.String(), "summary_hour", cfg.SummaryHour,
		"no_trade_after", cfg.NoTradeAfter.String(), "max_daily_loss_jpy", cfg.MaxDailyLossJPY)
	defer cfg.Logger.Info("ops_alert_loop_stopped")

	// dedup state: 同じ alert を毎 tick 連打しないよう、symbol→発火済み日付で 1 日 1 回に。
	st := newOpsAlertState()

	emit := func(ev *port.Event) {
		if ev == nil {
			return
		}
		if err := cfg.Notifier.Notify(ctx, *ev); err != nil {
			cfg.Logger.Warn("ops_alert_notify_failed", "title", ev.Title, "err", err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			opsAlertCheck(ctx, cfg, now, st, emit)
		}
	}
}

// opsAlertState は per-loop の dedup 簿記。各 alert を bot-TZ 日付ごとに 1 回だけ
// 発火させる (毎 tick の連打防止)。
type opsAlertState struct {
	ddAlerted       map[string]string
	noTradeAlerted  map[string]string
	lastSummaryDate string
}

func newOpsAlertState() *opsAlertState {
	return &opsAlertState{
		ddAlerted:      map[string]string{},
		noTradeAlerted: map[string]string{},
	}
}

// opsAlertCheck は全 symbol を 1 周して due な監視イベントを emit する。
// runOpsAlertLoop の per-tick 本体を切り出したもの — ticker 無しで決定論的に
// テストできるようにするため (loops_test.go の他ヘルパと同方針)。
func opsAlertCheck(ctx context.Context, cfg opsAlertConfig, now func() time.Time, st *opsAlertState, emit func(*port.Event)) {
	t0 := now().In(cfg.Loc)
	date := t0.Format("2006-01-02")
	startOfDay := time.Date(t0.Year(), t0.Month(), t0.Day(), 0, 0, 0, 0, cfg.Loc)
	since := now().Add(-time.Duration(cfg.EdgeWindowDays) * 24 * time.Hour)
	doSummary := t0.Hour() >= cfg.SummaryHour && st.lastSummaryDate != date

	for _, sym := range cfg.Symbols {
		dailyPnL, err := cfg.Trades.SumPnLJPYClosedSinceBySymbol(ctx, sym, startOfDay)
		if err != nil {
			cfg.Logger.Warn("ops_daily_pnl_failed", "symbol", sym, "err", err)
			continue
		}
		// The daily-DD alert measures GROSS realized loss (the entry-gate brake's
		// SumClosedLossJPYSince measure), NOT signed net PnL — feeding net would make
		// it fire on profitable days and stay silent on losing ones. dailyPnL (signed net) above is still used for the summary below.
		if st.ddAlerted[sym] != date {
			grossLoss, gerr := cfg.Trades.SumClosedLossJPYBySymbolSince(ctx, sym, startOfDay)
			if gerr != nil {
				cfg.Logger.Warn("ops_daily_gross_loss_failed", "symbol", sym, "err", gerr)
			} else if ev := app.DailyDDAlert(sym, float64(grossLoss), cfg.MaxDailyLossJPY); ev != nil {
				emit(ev)
				st.ddAlerted[sym] = date
			}
		}

		recent, err := cfg.Trades.ListClosedBySymbolSince(ctx, sym, since, cfg.EdgeLimit)
		if err != nil {
			cfg.Logger.Warn("ops_recent_trades_failed", "symbol", sym, "err", err)
			continue
		}
		ago := time.Duration(-1) // < 0 = まだ約定なし → NoTradeAlert は nil
		if len(recent) > 0 {
			ago = now().Sub(recent[0].ClosedAt)
		}
		if st.noTradeAlerted[sym] != date {
			if ev := app.NoTradeAlert(sym, ago, cfg.NoTradeAfter); ev != nil {
				emit(ev)
				st.noTradeAlerted[sym] = date
			}
		}

		if doSummary {
			// Bot edge only — the operator's discretionary trades must not
			// flatter (or worsen) the bot's daily edge summary. `recent` is left
			// intact above so the no-trade alert still sees any account activity.
			m := command.DeriveEdgeMetrics(command.BotTrades(recent))
			ev := app.DailySummaryEvent(app.OpsSummary{
				Symbol: sym, TradeCount: m.TradeCount, WinRatePct: m.WinRatePct,
				ProfitFactor: m.ProfitFactor, RewardRisk: m.RewardRisk,
				ExpectancyJPY: m.ExpectancyJPY, NetPnLJPY: m.NetPnLJPY, DailyPnLJPY: dailyPnL,
			})
			emit(&ev)
		}
	}
	if doSummary {
		st.lastSummaryDate = date
	}
}
