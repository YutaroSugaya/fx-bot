package query

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/domain/market"
)

func TestMergeBarsByTime(t *testing.T) {
	mk := func(min int, c float64) market.Candle {
		return market.Candle{OpenTime: time.Unix(int64(min)*60, 0).UTC(), Close: c}
	}
	older := []market.Candle{mk(1, 1.0), mk(2, 2.0), mk(3, 3.0)}
	recent := []market.Candle{mk(2, 22.0), mk(4, 4.0)} // overlaps at min 2 (recent wins) + a new bar

	out := mergeBarsByTime(older, recent)
	if len(out) != 4 {
		t.Fatalf("expected 4 unique bars, got %d", len(out))
	}
	// ascending by time
	for i := 1; i < len(out); i++ {
		if !out[i-1].OpenTime.Before(out[i].OpenTime) {
			t.Fatalf("not ascending at %d", i)
		}
	}
	// the overlapping minute keeps the recent close (22.0)
	if out[1].Close != 22.0 {
		t.Errorf("overlap should keep recent close 22.0, got %v", out[1].Close)
	}
}

// TestBackfillForMA_1H_FromGMO: a symbol with NO 1h bars in the DB (just added)
// still gets a 200MA overlay on the 1H chart, sourced from GMO klines. Before the
// backfill the 1H TF would have been dropped entirely (no bars) or shown no MA.
func TestBackfillForMA_1H_FromGMO(t *testing.T) {
	repo := backtest.NewInMemoryCandleRepo()
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)

	// 330 hourly GMO klines ending at now-1h → comfortably past the 200 window.
	hourly := make([]market.Kline, 330)
	for i := range hourly {
		op := 158.0 + 0.01*float64(i)
		hourly[i] = market.Kline{
			Symbol:   "USD_JPY",
			OpenTime: now.Add(-time.Duration(330-i) * time.Hour),
			Open:     op, High: op + 0.1, Low: op - 0.1, Close: op + 0.02,
		}
	}
	br := &fakeBroker{
		ticker:     &market.Ticker{Symbol: "USD_JPY", Bid: 160.0, Ask: 160.002, Timestamp: now},
		klinesByIv: map[string][]market.Kline{"1hour": hourly},
	}
	q := &GetMarketStateQuery{Candles: repo, Broker: br, Clock: func() time.Time { return now }}

	view, err := q.Execute(context.Background(), "USD_JPY")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var oneH *TimeframeView
	for i := range view.Timeframes {
		if view.Timeframes[i].Name == "1H" {
			oneH = &view.Timeframes[i]
		}
	}
	if oneH == nil {
		t.Fatal("1H timeframe missing (backfill should have supplied bars)")
	}
	nz := 0
	for _, v := range oneH.MaSma200 {
		if v > 0 {
			nz++
		}
	}
	if nz < 100 {
		t.Errorf("1H 200MA overlay: only %d/%d bars have a value — GMO backfill did not fill the warmup", nz, len(oneH.Candles))
	}
}
