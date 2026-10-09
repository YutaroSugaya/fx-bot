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

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// --- per-symbol ticker for /api/positions ---
//
// Pre-fix: GetTickerFn was () → (*Ticker, error) and the same ticker was
// applied to every row. For multi-symbol setups that meant a EUR_JPY
// position got valued at USD_JPY's bid/ask (UnrealizedPips/JPY garbage).
//
// Fix: GetTickerFn takes the row's symbol so each row is valued against
// its own market. Implementations are expected to cache within one
// Execute call to avoid N round-trips when many rows share a symbol.

func TestListOpenPositions_TickerIsPerRowSymbol(t *testing.T) {
	repo := backtest.NewInMemoryPositionRepo()
	t0 := time.Now()
	for _, sym := range []string{"USD_JPY", "EUR_JPY"} {
		_, err := repo.Insert(context.Background(), port.PositionInsertInput{Position: port.PositionRecord{
			Symbol: sym, Side: "BUY", Quantity: 100, EntryPrice: 100.00,
			Status: port.PositionStatusOpen, OpenedAt: t0,
		}})
		if err != nil {
			t.Fatalf("seed %s: %v", sym, err)
		}
	}

	prices := map[string][2]float64{
		"USD_JPY": {150.10, 150.13}, // bid, ask — BUY closes at bid 150.10
		"EUR_JPY": {163.40, 163.43}, // BUY closes at bid 163.40
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
	if len(views) != 2 {
		t.Fatalf("len: %d want 2", len(views))
	}
	bySym := map[string]OpenPositionView{}
	for _, v := range views {
		bySym[v.Symbol] = v
	}
	if !almost(bySym["USD_JPY"].CurrentPrice, 150.10) {
		t.Errorf("USD_JPY CurrentPrice: got %v want 150.10 (BUY→bid)", bySym["USD_JPY"].CurrentPrice)
	}
	if !almost(bySym["EUR_JPY"].CurrentPrice, 163.40) {
		t.Errorf("EUR_JPY CurrentPrice: got %v want 163.40 (BUY→bid)", bySym["EUR_JPY"].CurrentPrice)
	}
}

func TestListOpenPositions_TickerCachedAcrossRows(t *testing.T) {
	// If 3 rows share the same symbol, GetTicker must be invoked once per
	// unique symbol — not per row. Protects against rate-limit pressure on
	// /api/positions polls (5s tick × N rows would be wasteful).
	repo := backtest.NewInMemoryPositionRepo()
	t0 := time.Now()
	for i := 0; i < 3; i++ {
		_, _ = repo.Insert(context.Background(), port.PositionInsertInput{Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 100,
			Status: port.PositionStatusOpen, OpenedAt: t0,
		}})
	}
	_, _ = repo.Insert(context.Background(), port.PositionInsertInput{Position: port.PositionRecord{
		Symbol: "EUR_JPY", Side: "BUY", Quantity: 100, EntryPrice: 100,
		Status: port.PositionStatusOpen, OpenedAt: t0,
	}})

	callsBySym := map[string]int{}
	q := &ListOpenPositionsQuery{
		Positions: repo,
		GetTicker: func(_ context.Context, sym string) (*market.Ticker, error) {
			callsBySym[sym]++
			return &market.Ticker{Symbol: sym, Bid: 100, Ask: 100.01}, nil
		},
	}
	_, _ = q.Execute(context.Background(), ListOpenPositionsInput{})
	if callsBySym["USD_JPY"] != 1 {
		t.Errorf("USD_JPY GetTicker calls: %d want 1 (cached)", callsBySym["USD_JPY"])
	}
	if callsBySym["EUR_JPY"] != 1 {
		t.Errorf("EUR_JPY GetTicker calls: %d want 1", callsBySym["EUR_JPY"])
	}
}
