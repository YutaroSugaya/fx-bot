package app

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
)

// newDiscardLogger returns a logger that drops every log line. Used in tests
// that touch worker methods which log warnings on edge cases.
func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// buildCurrentMarketSummary は priceTick / minuteTick の重複した
// BuildMarketSummary call site を統合する helper。
//   - ticker / botState は caller が用意 (priceTick は ticker 済み、
//     minuteTick は fetch する)
//   - candles 1m/5m と Mode/HardLimits は worker フィールドから埋める
func TestBuildCurrentMarketSummary_PopulatesFromWorker(t *testing.T) {
	agg := market.NewAggregator("USD_JPY", map[time.Duration]int{
		time.Minute:     60,
		5 * time.Minute: 12,
	})
	w := &Worker{
		Aggregator: agg,
		Symbol:     "USD_JPY",
		BotConfig: &config.BotConfig{
			Symbol: "USD_JPY",
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
		},
		HardLimits: &config.HardLimits{},
	}
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	ticker := &market.Ticker{Symbol: "USD_JPY", Bid: 150.10, Ask: 150.13, Timestamp: now}
	bs := market.BotState{ConsecutiveLosses: 2}

	s := w.buildCurrentMarketSummary(now, ticker, bs)
	if s == nil {
		t.Fatal("nil summary")
	}
	if s.Symbol != "USD_JPY" {
		t.Errorf("symbol: %s", s.Symbol)
	}
	if s.BotState.Mode != string(config.ModePaperConfig) {
		t.Errorf("mode override missing: %s", s.BotState.Mode)
	}
	if s.BotState.ConsecutiveLosses != 2 {
		t.Errorf("BotState passed through wrong: %+v", s.BotState)
	}
	if s.CurrentRate.Ask != 150.13 {
		t.Errorf("ticker ignored: %+v", s.CurrentRate)
	}
}

// minuteTick から呼ぶ場合 (BotState zero) でも summary が組めること。
func TestBuildCurrentMarketSummary_EmptyBotStateForMinuteTick(t *testing.T) {
	agg := market.NewAggregator("USD_JPY", map[time.Duration]int{time.Minute: 60})
	w := &Worker{
		Aggregator: agg,
		Symbol:     "USD_JPY",
		BotConfig: &config.BotConfig{
			Symbol: "USD_JPY",
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
		},
		HardLimits: &config.HardLimits{},
	}
	now := time.Now().UTC()
	s := w.buildCurrentMarketSummary(now, nil, market.BotState{})
	if s == nil {
		t.Fatal("nil summary")
	}
	if s.BotState.Mode != string(config.ModePaperConfig) {
		t.Errorf("mode override missing: %s", s.BotState.Mode)
	}
}

// minuteTick が aggregator の最新 candle を CandleRepository に
// UPSERT すること。1m / 5m / 15m / 1h の全 timeframe で 1 本ずつ保存される。
func TestMinuteTick_UpsertsLatestCandlesToRepo(t *testing.T) {
	agg := market.NewAggregator("USD_JPY", map[time.Duration]int{
		time.Minute:      60,
		5 * time.Minute:  12,
		15 * time.Minute: 4,
		time.Hour:        2,
	})
	// 直接 OnKline で aggregator の各 buffer に 1 本ずつ詰める。
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	for _, k := range []market.Kline{
		{Symbol: "USD_JPY", Interval: "1min", OpenTime: t0, Open: 150.01, High: 150.05, Low: 149.99, Close: 150.03},
		{Symbol: "USD_JPY", Interval: "5min", OpenTime: t0, Open: 150.00, High: 150.10, Low: 149.95, Close: 150.05},
		{Symbol: "USD_JPY", Interval: "15min", OpenTime: t0, Open: 150.00, High: 150.20, Low: 149.90, Close: 150.10},
		{Symbol: "USD_JPY", Interval: "1hour", OpenTime: t0, Open: 150.00, High: 150.30, Low: 149.80, Close: 150.15},
	} {
		agg.OnKline(k)
	}

	repo := backtest.NewInMemoryCandleRepo()
	w := &Worker{
		Aggregator: agg,
		Symbol:     "USD_JPY",
		BotConfig: &config.BotConfig{
			Symbol: "USD_JPY",
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
		},
		HardLimits: &config.HardLimits{},
		Candles:    repo,
		Logger:     newDiscardLogger(),
	}

	w.minuteTick(context.Background(), func() *config.StrategyConfig { return nil })

	for _, tf := range []string{"1m", "5m", "15m", "1h"} {
		got, err := repo.ListSince(context.Background(), "USD_JPY", tf, t0, 0)
		if err != nil {
			t.Fatalf("ListSince(%s): %v", tf, err)
		}
		if len(got) != 1 {
			t.Errorf("%s: expected 1 row, got %d", tf, len(got))
		}
	}
}
