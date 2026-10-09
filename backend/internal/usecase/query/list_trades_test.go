package query

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/port"
)

func TestListTradesQuery(t *testing.T) {
	now := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	recent := port.TradeRecord{Symbol: "USD_JPY", Side: "BUY", OpenedAt: now.Add(-time.Hour), ClosedAt: now.Add(-30 * time.Minute), ProfitLossJPY: 50}
	old := port.TradeRecord{Symbol: "USD_JPY", Side: "SELL", OpenedAt: now.Add(-60 * 24 * time.Hour), ClosedAt: now.Add(-60 * 24 * time.Hour), ProfitLossJPY: -10}

	cases := []struct {
		name     string
		seed     []port.TradeRecord
		input    ListTradesInput
		wantRows int
	}{
		{"recent within 30 days included", []port.TradeRecord{recent}, ListTradesInput{}, 1},
		{"old over 30 days excluded", []port.TradeRecord{old}, ListTradesInput{}, 0},
		{"limit caps at 500", manyTrades(now, 600), ListTradesInput{Limit: 9999}, 500},
		{"limit defaults to 50 when 0", manyTrades(now, 100), ListTradesInput{Limit: 0}, 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := backtest.NewInMemoryTradeRepo()
			for _, r := range tc.seed {
				_ = repo.Insert(context.Background(), r)
			}
			q := &ListTradesQuery{Trades: repo, Clock: func() time.Time { return now }}
			got, err := q.Execute(context.Background(), tc.input)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if len(got) != tc.wantRows {
				t.Errorf("rows: got %d want %d", len(got), tc.wantRows)
			}
		})
	}
}

// Epoch floors closed_at so the dashboard's P&L/win-rate aggregates count only
// from the current strategy regime. Trades closed before the
// epoch are excluded even when within the 30-day default window.
func TestListTradesQuery_EpochFloorsOlderTrades(t *testing.T) {
	now := time.Date(2026, 6, 22, 4, 0, 0, 0, time.UTC)
	epoch := time.Date(2026, 6, 19, 16, 0, 0, 0, time.UTC)
	preEpoch := port.TradeRecord{Symbol: "USD_JPY", Side: "BUY", OpenedAt: now.Add(-72 * time.Hour),
		ClosedAt: time.Date(2026, 6, 19, 15, 30, 0, 0, time.UTC), ProfitLossJPY: -100} // before epoch
	postEpoch := port.TradeRecord{Symbol: "USD_JPY", Side: "SELL", OpenedAt: now.Add(-40 * time.Hour),
		ClosedAt: time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC), ProfitLossJPY: 50} // after epoch

	repo := backtest.NewInMemoryTradeRepo()
	_ = repo.Insert(context.Background(), preEpoch)
	_ = repo.Insert(context.Background(), postEpoch)

	// Without epoch: both within 30 days → both returned.
	noFloor := &ListTradesQuery{Trades: repo, Clock: func() time.Time { return now }}
	if got, _ := noFloor.Execute(context.Background(), ListTradesInput{}); len(got) != 2 {
		t.Fatalf("baseline (no epoch): got %d rows, want 2", len(got))
	}

	// With epoch: pre-epoch trade is excluded.
	q := &ListTradesQuery{Trades: repo, Clock: func() time.Time { return now }, Epoch: epoch}
	got, err := q.Execute(context.Background(), ListTradesInput{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("with epoch: got %d rows, want 1 (pre-epoch excluded)", len(got))
	}
	if got[0].ProfitLossJPY != 50 {
		t.Errorf("surviving trade should be the post-epoch one (+50), got %+v", got[0])
	}
}

func manyTrades(now time.Time, n int) []port.TradeRecord {
	out := make([]port.TradeRecord, n)
	for i := range out {
		out[i] = port.TradeRecord{
			Symbol:   "USD_JPY",
			Side:     "BUY",
			OpenedAt: now.Add(-time.Duration(i) * time.Minute),
			ClosedAt: now.Add(-time.Duration(i) * time.Minute),
		}
	}
	return out
}
