package app

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/usecase/command"
)

type retriggerTickBroker struct {
	port.Broker
	ticker *market.Ticker
}

func (b *retriggerTickBroker) GetTicker(_ context.Context, _ string) (*market.Ticker, error) {
	return b.ticker, nil
}

// priceTick が EventRetrigger.OnPriceTick を毎 tick 呼ぶ配線の検証
// (event_retrigger)。判定ロジック本体は usecase 側で単体テスト済み —
// ここは「30分で+30pipsの価格系列を流すと Trigger まで到達する」ことだけ担保する。
func TestPriceTick_FeedsEventRetrigger(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	agg := market.NewAggregator("USD_JPY", map[time.Duration]int{time.Minute: 120})
	// 60分前〜1分前まで 145.00 の 1m 足を埋める。
	for i := 60; i >= 1; i-- {
		ts := now.Add(-time.Duration(i) * time.Minute)
		agg.OnTick(market.Ticker{Symbol: "USD_JPY", Bid: 144.995, Ask: 145.005, Timestamp: ts})
	}

	fired := []string{}
	retrigger := &command.LLMEventRetrigger{
		MovePips:     25,
		MoveWindow:   30 * time.Minute,
		MoveCooldown: 20 * time.Minute,
		Trigger:      func(reason string) error { fired = append(fired, reason); return nil },
		Now:          func() time.Time { return now },
		Logger:       newDiscardLogger(),
	}

	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	br := &retriggerTickBroker{
		// mid 145.30 = 30 分前の 145.00 から +30pips ≥ 閾値 25。
		ticker: &market.Ticker{Symbol: "USD_JPY", Bid: 145.295, Ask: 145.305, Timestamp: now},
	}
	w := &Worker{
		Broker:     br,
		Aggregator: agg,
		Symbol:     "USD_JPY",
		BotConfig: &config.BotConfig{
			Symbol: "USD_JPY",
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
		},
		HardLimits:     &config.HardLimits{},
		Manager:        command.NewManageOpenPositions(br, posRepo, tradeRepo, "USD_JPY", config.ModePaperConfig, newDiscardLogger()),
		Logger:         newDiscardLogger(),
		EventRetrigger: retrigger,
	}

	w.priceTick(context.Background(), func() *config.StrategyConfig { return nil })

	if len(fired) != 1 {
		t.Fatalf("Trigger fired = %v, want exactly 1 (priceTick must feed EventRetrigger)", fired)
	}
}

// EventRetrigger 未配線 (nil) の priceTick は従来どおり panic せず動く。
func TestPriceTick_NilEventRetriggerSafe(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	agg := market.NewAggregator("USD_JPY", map[time.Duration]int{time.Minute: 10})
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	br := &retriggerTickBroker{ticker: &market.Ticker{Symbol: "USD_JPY", Bid: 145.0, Ask: 145.01, Timestamp: now}}
	w := &Worker{
		Broker:     br,
		Aggregator: agg,
		Symbol:     "USD_JPY",
		BotConfig: &config.BotConfig{
			Symbol: "USD_JPY",
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
		},
		HardLimits: &config.HardLimits{},
		Manager:    command.NewManageOpenPositions(br, posRepo, tradeRepo, "USD_JPY", config.ModePaperConfig, newDiscardLogger()),
		Logger:     newDiscardLogger(),
	}
	w.priceTick(context.Background(), func() *config.StrategyConfig { return nil })
}
