// Package usecase contains the application-level orchestration. Each file
// implements one cohesive flow (e.g. BuildMarketSummary, UpdateStrategyConfig,
// EvaluateEntry, ExecuteOrder, Reconcile).
//
// Usecases depend only on:
//   - domain types
//   - port interfaces (broker, repository, advisor, notifier, clock)
//
// They never import adapter packages directly — wiring happens in cmd/bot.
package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
)

// summary5mLookback is the window of 5-minute candles aggregated into summary_5m.
// 100 min ≈ 20 candles — enough for a stable ATR(5m) and to back the playbook's
// "直近レンジ(20本5m)" reference.
const summary5mLookback = 100 * time.Minute

// BuildMarketSummaryInput collects the parameters needed by BuildMarketSummary.
// We pass these as a struct so callers can fill the bot-state portion without
// going through 7 positional args.
type BuildMarketSummaryInput struct {
	Symbol     string
	Mode       config.Mode
	Now        time.Time
	HardLimits *config.HardLimits

	// Account-wide risk caps from bot_config.yaml (not strategy hard_limits).
	// Surfaced to Claude under hard_limits.{max_daily_loss_jpy,max_consecutive_losses}
	// so prompts can reference a single 'hard_limits' block.
	MaxDailyLossJPY      int
	MaxConsecutiveLosses int

	// MaxSpreadPipsOverride: the advertised hard_limits.max_spread_pips
	// must be the gate that actually governs the consuming path. The LLM decision loop
	// defers at llm_decision.max_spread_pips (e.g. 3.0), not at the strategy hard-limit
	// ceiling (e.g. 1.5) — advertising the latter makes the LLM cite "1.5 hard cap" in its
	// reasons and invites spurious no_trades in the 1.5-3.0 band. >0 replaces the
	// advertised value; 0 keeps the hard-limits value.
	MaxSpreadPipsOverride float64

	// Snapshot candles (1m + optional 5m) used for window stats. The caller
	// (worker) pulls these from the in-memory Aggregator before each summary.
	Candles1m []market.Candle
	Candles5m []market.Candle

	// Latest ticker — used to populate current_rate.
	Ticker *market.Ticker

	// Per-tick spread samples (chronological) used to compute summary_*.avg_spread_pips
	// and max_spread_pips. Caller (worker) keeps a rolling 24h history.
	// Nil → spread fields are omitted.
	SpreadSamples []market.SpreadSample

	// Bot-state derived data (counted by repository callers).
	BotState market.BotState

	// Recent trades + rejections, optional. The caller may pass nil for the
	// first run.
	RecentTrades     []market.RecentTrade
	RecentRejections []market.RecentRejection

	// AllowedStrategies passed back into the JSON so Claude knows the menu.
	AllowedStrategies []string

	// RecentDecisions / EventContext. Optional. The caller
	// (wiring) fills these from a recent-config query + the event calendar so
	// Claude sees its own past decisions and any nearby economic event.
	RecentDecisions []market.RecentDecision
	EventContext    *market.EventContext
}

// BuildMarketSummary composes a MarketSummary from the inputs. It does not
// touch I/O — the caller passes WriteMarketSummaryJSON to persist the result.
func BuildMarketSummary(in BuildMarketSummaryInput) *market.MarketSummary {
	pip := market.PipSize(in.Symbol)
	s := &market.MarketSummary{
		Symbol:            in.Symbol,
		Time:              in.Now,
		NextValidFrom:     in.Now.Truncate(time.Minute).Add(time.Minute),
		BotState:          in.BotState,
		RecentTrades:      in.RecentTrades,
		RecentRejections:  in.RecentRejections,
		AllowedStrategies: in.AllowedStrategies,
		RecentDecisions:   in.RecentDecisions,
		EventContext:      in.EventContext,
	}
	s.BotState.Mode = string(in.Mode) // domain/market.BotState.Mode is string (JSON boundary toward Claude)

	if in.Ticker != nil {
		s.CurrentRate = market.CurrentRate{
			Bid:        in.Ticker.Bid,
			Ask:        in.Ticker.Ask,
			SpreadPips: in.Ticker.SpreadPips(pip),
			Timestamp:  in.Ticker.Timestamp,
		}
	}

	// 現在値 mid (= range_position の基準)。Ticker が無ければ 0.5(中立) のまま。
	// 5m / 1m 両方の window で使うので候補ブロックの前に出す。
	mid := 0.0
	haveMid := in.Ticker != nil
	if haveMid {
		mid = (in.Ticker.Bid + in.Ticker.Ask) / 2
	}

	// summary_5m は本物の 5 分足ストリームから集計する (atr_pips = トレーダーの言う ATR(5m))。
	// playbook の ATR(5m) 条件はこれを参照する。1m 由来の atr_pips とは別物。
	if len(in.Candles5m) > 0 {
		c5 := market.SinceWindow(in.Candles5m, in.Now, summary5mLookback)
		s.Summary5m = market.ApplyWindowSpreads(
			market.BuildWindowSummary(c5, pip),
			spreadsInWindow(in.SpreadSamples, in.Now, summary5mLookback))
		if haveMid && s.Summary5m.NumCandles > 0 {
			s.Summary5m.RangePositionPct = market.RangePosition(mid, s.Summary5m.Low, s.Summary5m.High)
		} else {
			s.Summary5m.RangePositionPct = 0.5
		}
	}

	// Windowed summaries computed from the 1m candle stream + per-tick spread samples.
	// duration → 書き込み先 field のペアでループ。並びは summary.go の宣言順に揃える。
	if len(in.Candles1m) > 0 {
		windows := []struct {
			d   time.Duration
			dst *market.WindowSummary
		}{
			{15 * time.Minute, &s.Summary15m},
			{time.Hour, &s.Summary1h},
			{6 * time.Hour, &s.Summary6h},
			{24 * time.Hour, &s.Summary24h},
		}
		for _, w := range windows {
			*w.dst = market.ApplyWindowSpreads(
				market.BuildWindowSummary(market.SinceWindow(in.Candles1m, in.Now, w.d), pip),
				spreadsInWindow(in.SpreadSamples, in.Now, w.d))
			// range_position は現在値が window の [low,high] のどこにいるか。
			// 0=安値圏(支持線), 1=高値圏(抵抗線)。num_candles>0 のときだけ意味を持つ。
			if haveMid && w.dst.NumCandles > 0 {
				w.dst.RangePositionPct = market.RangePosition(mid, w.dst.Low, w.dst.High)
			} else {
				w.dst.RangePositionPct = 0.5 // 中立 (基準不明 or データ無し)
			}
		}
	}

	// Limit info exposed to Claude (NOT a path to leak secrets).
	if in.HardLimits != nil {
		advertisedMaxSpread := in.HardLimits.MaxSpreadPips.Max
		if in.MaxSpreadPipsOverride > 0 {
			advertisedMaxSpread = in.MaxSpreadPipsOverride
		}
		s.HardLimits = &market.HardLimitsForJSON{
			Quantity:               market.IntBound{Min: in.HardLimits.Quantity.Min, Max: in.HardLimits.Quantity.Max},
			MaxSpreadPips:          advertisedMaxSpread,
			MaxTradesInThisWindow:  in.HardLimits.MaxTradesInThisWindow.Max,
			MaxLossInThisWindowJPY: in.HardLimits.MaxLossInThisWindowJPY.Max,
			MaxDailyLossJPY:        in.MaxDailyLossJPY,
			MaxConsecutiveLosses:   in.MaxConsecutiveLosses,
			StopLossPips: market.FloatBound{
				Min: in.HardLimits.StopLossPips.Min, Max: in.HardLimits.StopLossPips.Max,
			},
			TakeProfitPips: market.FloatBound{
				Min: in.HardLimits.TakeProfitPips.Min, Max: in.HardLimits.TakeProfitPips.Max,
			},
		}
	}
	return s
}

// spreadsInWindow extracts spread Pips values from samples whose Time falls
// within [now-d, now]. Returns nil when none match (caller's ApplyWindowSpreads
// is a no-op on nil).
func spreadsInWindow(samples []market.SpreadSample, now time.Time, d time.Duration) []float64 {
	if len(samples) == 0 {
		return nil
	}
	cutoff := now.Add(-d)
	out := make([]float64, 0, len(samples))
	for _, s := range samples {
		if s.Time.Before(cutoff) || s.Time.After(now) {
			continue
		}
		out = append(out, s.Pips)
	}
	return out
}

// PublishMarketSummary serialises s to JSON and writes it via the
// port.MarketSummaryArtifactStore, so usecase does no direct file I/O
// ("usecase has no file I/O", docs/architecture/layers/usecase.md). The
// wiring layer passes adapter/artifact.NewFileMarketSummaryStore(path)
// for the file backend; tests pass in-memory stores.
func PublishMarketSummary(ctx context.Context, store port.MarketSummaryArtifactStore, s *market.MarketSummary) error {
	if store == nil {
		return fmt.Errorf("market summary store not configured")
	}
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal summary: %w", err)
	}
	return store.WriteLatestSummary(ctx, body)
}

// SaveMarketSummaryDB persists the summary as a JSONB blob to the
// market_summaries table for audit. It's optional — callers may pass nil repo.
func SaveMarketSummaryDB(ctx context.Context, repo port.MarketSummaryRepository, s *market.MarketSummary, window string) error {
	if repo == nil {
		return nil
	}
	body, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal summary: %w", err)
	}
	return repo.Insert(ctx, port.MarketSummaryRecord{
		Symbol:        s.Symbol,
		SummaryWindow: window,
		RawJSON:       body,
	})
}
