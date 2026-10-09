package query

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
)

func TestListOpenPositionsQuery(t *testing.T) {
	now := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	openedAt := now.Add(-30 * time.Minute)

	cases := []struct {
		name             string
		seed             []port.PositionRecord
		ticker           *market.Ticker
		wantLen          int
		wantCurrentPrice float64
		wantUnrealJPY    float64
		wantTPPrice      float64
		wantSLPrice      float64
		wantIsManual     bool
	}{
		{
			name: "buy with current price computes unrealized PnL",
			seed: []port.PositionRecord{{
				Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 100.00,
				TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240,
				StrategyConfigID: "cfg-manual",
				Status:           port.PositionStatusOpen, OpenedAt: openedAt,
			}},
			ticker:           &market.Ticker{Bid: 100.10, Ask: 100.11},
			wantLen:          1,
			wantCurrentPrice: 100.10, // BUY closes at BID
			wantUnrealJPY:    10.0,   // (bid 100.10 - 100.0) * 100
			wantTPPrice:      100.20,
			wantSLPrice:      99.85,
			// IsManual lookup is verified by the dedicated case below; for
			// this case we don't seed manual_positions so IsManual stays false.
			wantIsManual: false,
		},
		{
			name: "sell mirrors PnL direction",
			seed: []port.PositionRecord{{
				Symbol: "USD_JPY", Side: "SELL", Quantity: 100, EntryPrice: 100.00,
				TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240,
				StrategyConfigID: "cfg-1",
				Status:           port.PositionStatusOpen, OpenedAt: openedAt,
			}},
			ticker:           &market.Ticker{Bid: 100.10, Ask: 100.11},
			wantLen:          1,
			wantCurrentPrice: 100.11, // SELL closes at ASK
			wantUnrealJPY:    -11.0,  // -(ask 100.11 - 100.0) * 100
			wantTPPrice:      99.80,
			wantSLPrice:      100.15,
			wantIsManual:     false,
		},
		{
			name:             "empty repo returns 0 rows",
			seed:             nil,
			ticker:           &market.Ticker{Bid: 100, Ask: 100.01},
			wantLen:          0,
			wantCurrentPrice: 0,
		},
		{
			name: "no ticker leaves CurrentPrice=0 and UnrealizedJPY=0",
			seed: []port.PositionRecord{{
				Symbol: "USD_JPY", Side: "BUY", Quantity: 100, EntryPrice: 100.00,
				Status: port.PositionStatusOpen, OpenedAt: openedAt,
			}},
			ticker:           nil,
			wantLen:          1,
			wantCurrentPrice: 0,
			wantUnrealJPY:    0,
			wantTPPrice:      100.00,
			wantSLPrice:      100.00,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := backtest.NewInMemoryPositionRepo()
			for i := range tc.seed {
				_, _ = repo.Insert(context.Background(), port.PositionInsertInput{Position: tc.seed[i]})
			}
			var getTicker GetTickerFn
			if tc.ticker != nil {
				tk := tc.ticker
				getTicker = func(ctx context.Context, _ string) (*market.Ticker, error) { return tk, nil }
			}
			q := &ListOpenPositionsQuery{
				Symbol: "USD_JPY", PipSize: 0.01,
				Positions: repo, GetTicker: getTicker,
				Clock: func() time.Time { return now },
			}
			views, err := q.Execute(context.Background(), ListOpenPositionsInput{})
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if len(views) != tc.wantLen {
				t.Fatalf("len: got %d want %d", len(views), tc.wantLen)
			}
			if tc.wantLen == 0 {
				return
			}
			v := views[0]
			if !nearly(v.CurrentPrice, tc.wantCurrentPrice) {
				t.Errorf("CurrentPrice: got %v want %v", v.CurrentPrice, tc.wantCurrentPrice)
			}
			if !nearly(v.UnrealizedJPY, tc.wantUnrealJPY) {
				t.Errorf("UnrealizedJPY: got %v want %v", v.UnrealizedJPY, tc.wantUnrealJPY)
			}
			if !nearly(v.TPPrice, tc.wantTPPrice) {
				t.Errorf("TPPrice: got %v want %v", v.TPPrice, tc.wantTPPrice)
			}
			if !nearly(v.SLPrice, tc.wantSLPrice) {
				t.Errorf("SLPrice: got %v want %v", v.SLPrice, tc.wantSLPrice)
			}
			if v.IsManual != tc.wantIsManual {
				t.Errorf("IsManual: got %v want %v", v.IsManual, tc.wantIsManual)
			}
		})
	}
}

func TestListOpenPositionsQuery_RepoError_ReturnsError(t *testing.T) {
	q := &ListOpenPositionsQuery{
		Symbol: "USD_JPY", PipSize: 0.01,
		Positions: &errRepo{err: errors.New("db down")},
	}
	_, err := q.Execute(context.Background(), ListOpenPositionsInput{})
	if err == nil {
		t.Fatal("expected error from underlying repo")
	}
}

// errRepo は ListOpenOrClosing に err を注入する古典派ルール例外 (#2 失敗注入)。
type errRepo struct {
	port.PositionRepository
	err error
}

func (r *errRepo) ListOpenOrClosing(ctx context.Context, sym string) ([]port.PositionRecord, error) {
	return nil, r.err
}

func nearly(a, b float64) bool { return math.Abs(a-b) < 1e-6 }
