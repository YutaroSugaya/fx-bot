package query

import (
	"context"
	"math"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
)

// UnrealizedJPY for a USD-quote pair (EUR_USD) must convert the USD-denominated
// move to JPY via USD/JPY. Pre-fix it was priceDiff*qty (= USD treated as JPY),
// so a real +N pip move showed as ~0 円 on the dashboard.
func TestListOpenPositions_USDQuoteUnrealizedConvertedToJPY(t *testing.T) {
	repo := backtest.NewInMemoryPositionRepo()
	t0 := time.Now()
	// EUR_USD BUY 1000 @1.08000.
	_, err := repo.Insert(context.Background(), port.PositionInsertInput{Position: port.PositionRecord{
		Symbol: "EUR_USD", Side: "BUY", Quantity: 1000, EntryPrice: 1.08000,
		Status: port.PositionStatusOpen, OpenedAt: t0,
	}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	const usdjpy = 157.0
	prices := map[string][2]float64{
		"EUR_USD": {1.08090, 1.08110}, // BUY closes at bid 1.08090 → +0.00090 = +9 pips
		"USD_JPY": {usdjpy, usdjpy},   // mid 157.0 (the conversion rate)
	}
	q := &ListOpenPositionsQuery{
		Positions: repo,
		GetTicker: func(_ context.Context, sym string) (*market.Ticker, error) {
			p, ok := prices[sym]
			if !ok {
				return nil, nil
			}
			return &market.Ticker{Symbol: sym, Bid: p[0], Ask: p[1]}, nil
		},
	}
	views, err := q.Execute(context.Background(), ListOpenPositionsInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("len: %d want 1", len(views))
	}
	v := views[0]
	// pips is currency-agnostic. BUY valued at bid 1.08090 → +9 pips.
	if math.Abs(v.UnrealizedPips-9) > 1e-6 {
		t.Errorf("UnrealizedPips: got %v want 9 (bid valuation)", v.UnrealizedPips)
	}
	// +0.00090 × 1000 = 0.9 USD → × 157 = 141.3 JPY (NOT 0.9).
	if math.Abs(v.UnrealizedJPY-141.3) > 1e-6 {
		t.Errorf("UnrealizedJPY: got %v want 141.3 (0.9 USD × %.0f) — quote→JPY not applied", v.UnrealizedJPY, usdjpy)
	}
}

// Regression: a JPY-quote pair must be unchanged (no USD/JPY multiply).
func TestListOpenPositions_JPYQuoteUnrealizedUnchanged(t *testing.T) {
	repo := backtest.NewInMemoryPositionRepo()
	t0 := time.Now()
	// USD_JPY BUY 1000 @150.000. BUY closes at bid 150.095 → +0.095 = +9.5 pips → 95 JPY.
	_, err := repo.Insert(context.Background(), port.PositionInsertInput{Position: port.PositionRecord{
		Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 150.000,
		Status: port.PositionStatusOpen, OpenedAt: t0,
	}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	q := &ListOpenPositionsQuery{
		Positions: repo,
		GetTicker: func(_ context.Context, sym string) (*market.Ticker, error) {
			if sym == "USD_JPY" {
				return &market.Ticker{Symbol: sym, Bid: 150.095, Ask: 150.105}, nil // BUY→bid 150.095
			}
			return nil, nil
		},
	}
	views, err := q.Execute(context.Background(), ListOpenPositionsInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if math.Abs(views[0].UnrealizedJPY-95.0) > 1e-6 {
		t.Errorf("UnrealizedJPY: got %v want 95 (JPY quote unchanged, bid valuation)", views[0].UnrealizedJPY)
	}
}
