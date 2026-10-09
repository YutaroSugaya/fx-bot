package backtest

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/port"
)

func TestInMemoryPositionRepo_InsertListMarkClosed(t *testing.T) {
	ctx := context.Background()
	repo := NewInMemoryPositionRepo()

	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	in := port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol:         "USD_JPY",
			Side:           "BUY",
			Quantity:       100,
			EntryPrice:     150.10,
			TakeProfitPips: 30,
			StopLossPips:   20,
			MaxHoldMinutes: 60,
			Status:         port.PositionStatusOpen,
			OpenedAt:       t0,
		},
	}
	id, err := repo.Insert(ctx, in)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id == 0 {
		t.Fatal("Insert should return non-zero id")
	}

	open, err := repo.ListOpenOrClosing(ctx, "USD_JPY")
	if err != nil {
		t.Fatalf("ListOpenOrClosing: %v", err)
	}
	if len(open) != 1 || open[0].EntryPrice != 150.10 {
		t.Fatalf("expected 1 open position with entry 150.10, got %+v", open)
	}

	// MarkClosed → ListOpenOrClosing should drop it.
	if err := repo.MarkClosed(ctx, id, t0.Add(time.Hour)); err != nil {
		t.Fatalf("MarkClosed: %v", err)
	}
	open, _ = repo.ListOpenOrClosing(ctx, "USD_JPY")
	if len(open) != 0 {
		t.Fatalf("expected 0 open after MarkClosed, got %d", len(open))
	}
}

func TestInMemoryPositionRepo_SymbolFilter(t *testing.T) {
	ctx := context.Background()
	repo := NewInMemoryPositionRepo()
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	_, _ = repo.Insert(ctx, port.PositionInsertInput{Position: port.PositionRecord{Symbol: "USD_JPY", Status: port.PositionStatusOpen, OpenedAt: t0}})
	_, _ = repo.Insert(ctx, port.PositionInsertInput{Position: port.PositionRecord{Symbol: "EUR_JPY", Status: port.PositionStatusOpen, OpenedAt: t0}})

	usd, _ := repo.ListOpenOrClosing(ctx, "USD_JPY")
	if len(usd) != 1 || usd[0].Symbol != "USD_JPY" {
		t.Errorf("symbol filter broken: %+v", usd)
	}
}

func TestInMemoryTradeRepo_InsertAndAggregates(t *testing.T) {
	ctx := context.Background()
	repo := NewInMemoryTradeRepo()
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)

	for i, pnl := range []float64{100, -50, 200, -75} {
		_ = repo.Insert(ctx, port.TradeRecord{
			Symbol:        "USD_JPY",
			Side:          "BUY",
			ProfitLossJPY: pnl,
			OpenedAt:      t0.Add(time.Duration(i) * time.Minute),
			ClosedAt:      t0.Add(time.Duration(i+1) * time.Minute),
		})
	}

	count, err := repo.CountSince(ctx, t0)
	if err != nil || count != 4 {
		t.Errorf("CountSince: got %d err=%v want 4", count, err)
	}

	loss, err := repo.SumLossJPYSince(ctx, t0)
	if err != nil {
		t.Fatalf("SumLossJPYSince: %v", err)
	}
	// sum of negatives' absolute values: 50 + 75 = 125
	if loss != 125 {
		t.Errorf("SumLossJPYSince: got %d want 125", loss)
	}

	list, err := repo.ListSince(ctx, t0, 10)
	if err != nil {
		t.Fatalf("ListSince: %v", err)
	}
	if len(list) != 4 {
		t.Errorf("ListSince: got %d want 4", len(list))
	}
	// Must be ordered most-recent-first (matches port contract used by worker.accountSnapshot).
	if !(list[0].OpenedAt.After(list[1].OpenedAt) && list[1].OpenedAt.After(list[2].OpenedAt)) {
		t.Errorf("ListSince should return DESC by OpenedAt; got %+v", list)
	}
}

// CandleRepository tests ------------------------------------------

// --- per-symbol vs account-wide trade aggregates ---

func TestInMemoryTradeRepo_BySymbolAggregatesFilter(t *testing.T) {
	ctx := context.Background()
	repo := NewInMemoryTradeRepo()
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)

	mk := func(sym string, pnl float64, off time.Duration) port.TradeRecord {
		return port.TradeRecord{
			Symbol: sym, Side: "BUY", Quantity: 100,
			EntryPrice: 100, ExitPrice: 100 + pnl, ProfitLossJPY: pnl,
			OpenedAt: t0.Add(off - 10*time.Minute),
			ClosedAt: t0.Add(off),
		}
	}
	for _, rec := range []port.TradeRecord{
		mk("USD_JPY", -300, time.Minute),
		mk("USD_JPY", 500, 2*time.Minute),
		mk("EUR_JPY", -700, 3*time.Minute),
		mk("EUR_JPY", -200, 4*time.Minute),
	} {
		if err := repo.Insert(ctx, rec); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	// CountClosedBySymbolSince — per-symbol filter
	usdCnt, _ := repo.CountClosedBySymbolSince(ctx, "USD_JPY", t0)
	if usdCnt != 2 {
		t.Errorf("CountClosedBySymbolSince(USD_JPY) = %d, want 2", usdCnt)
	}
	eurCnt, _ := repo.CountClosedBySymbolSince(ctx, "EUR_JPY", t0)
	if eurCnt != 2 {
		t.Errorf("CountClosedBySymbolSince(EUR_JPY) = %d, want 2", eurCnt)
	}

	// SumClosedLossJPYBySymbolSince — only loss magnitude, per symbol
	usdLoss, _ := repo.SumClosedLossJPYBySymbolSince(ctx, "USD_JPY", t0)
	if usdLoss != 300 {
		t.Errorf("SumClosedLossJPYBySymbolSince(USD_JPY) = %d, want 300", usdLoss)
	}
	eurLoss, _ := repo.SumClosedLossJPYBySymbolSince(ctx, "EUR_JPY", t0)
	if eurLoss != 900 {
		t.Errorf("SumClosedLossJPYBySymbolSince(EUR_JPY) = %d, want 900", eurLoss)
	}

	// ListClosedBySymbolSince — per-symbol filter, closed_at DESC
	usdList, _ := repo.ListClosedBySymbolSince(ctx, "USD_JPY", t0, 0)
	if len(usdList) != 2 {
		t.Fatalf("ListClosedBySymbolSince(USD_JPY) len = %d, want 2", len(usdList))
	}
	if !usdList[0].ClosedAt.After(usdList[1].ClosedAt) {
		t.Errorf("ListClosedBySymbolSince should be DESC by closed_at")
	}

	// Account-wide methods (existing) must still aggregate across symbols.
	allCnt, _ := repo.CountClosedSince(ctx, t0)
	if allCnt != 4 {
		t.Errorf("CountClosedSince = %d, want 4 (account-wide)", allCnt)
	}
	allLoss, _ := repo.SumClosedLossJPYSince(ctx, t0)
	if allLoss != 1200 {
		t.Errorf("SumClosedLossJPYSince = %d, want 1200 (account-wide)", allLoss)
	}
}

func TestInMemoryPositionRepo_CountOpenAllSymbols(t *testing.T) {
	ctx := context.Background()
	repo := NewInMemoryPositionRepo()
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	for _, sym := range []string{"USD_JPY", "USD_JPY", "EUR_JPY"} {
		_, _ = repo.Insert(ctx, port.PositionInsertInput{Position: port.PositionRecord{
			Symbol: sym, Status: port.PositionStatusOpen, OpenedAt: t0,
		}})
	}
	// CLOSING also counts (still holds broker capacity).
	id, _ := repo.Insert(ctx, port.PositionInsertInput{Position: port.PositionRecord{
		Symbol: "GBP_JPY", Status: port.PositionStatusOpen, OpenedAt: t0,
	}})
	if _, err := repo.ClaimForClose(ctx, id, t0.Add(time.Minute)); err != nil {
		t.Fatalf("ClaimForClose: %v", err)
	}

	got, err := repo.CountOpenAllSymbols(ctx)
	if err != nil {
		t.Fatalf("CountOpenAllSymbols: %v", err)
	}
	if got != 4 {
		t.Errorf("CountOpenAllSymbols = %d, want 4 (3 OPEN + 1 CLOSING across 3 symbols)", got)
	}
}

func TestInMemoryCandleRepo_UpsertAndList(t *testing.T) {
	ctx := context.Background()
	repo := NewInMemoryCandleRepo()
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)

	for i, close := range []float64{150.10, 150.15, 150.20} {
		_ = repo.Upsert(ctx, port.CandleRecord{
			Symbol:    "USD_JPY",
			Timeframe: "1m",
			OpenedAt:  t0.Add(time.Duration(i) * time.Minute),
			Open:      close - 0.01, High: close + 0.05, Low: close - 0.05, Close: close,
		})
	}

	got, err := repo.ListSince(ctx, "USD_JPY", "1m", t0, 0)
	if err != nil {
		t.Fatalf("ListSince: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 candles, got %d", len(got))
	}
	// DESC by OpenedAt: index 0 is the latest.
	if !got[0].OpenedAt.After(got[1].OpenedAt) || !got[1].OpenedAt.After(got[2].OpenedAt) {
		t.Errorf("ListSince should return DESC; got %+v", got)
	}
	if got[0].Close != 150.20 {
		t.Errorf("latest close: got %v want 150.20", got[0].Close)
	}
}

func TestInMemoryCandleRepo_UpsertOverwrites(t *testing.T) {
	ctx := context.Background()
	repo := NewInMemoryCandleRepo()
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)

	_ = repo.Upsert(ctx, port.CandleRecord{Symbol: "USD_JPY", Timeframe: "1m", OpenedAt: t0, Close: 150.10})
	// Re-upsert same key with new value.
	_ = repo.Upsert(ctx, port.CandleRecord{Symbol: "USD_JPY", Timeframe: "1m", OpenedAt: t0, Close: 150.30})

	got, _ := repo.ListSince(ctx, "USD_JPY", "1m", t0, 0)
	if len(got) != 1 {
		t.Fatalf("expected 1 row (upsert), got %d", len(got))
	}
	if got[0].Close != 150.30 {
		t.Errorf("Upsert should overwrite Close; got %v want 150.30", got[0].Close)
	}
}

func TestInMemoryCandleRepo_FiltersBySymbolAndTimeframe(t *testing.T) {
	ctx := context.Background()
	repo := NewInMemoryCandleRepo()
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	_ = repo.Upsert(ctx, port.CandleRecord{Symbol: "USD_JPY", Timeframe: "1m", OpenedAt: t0, Close: 150.0})
	_ = repo.Upsert(ctx, port.CandleRecord{Symbol: "USD_JPY", Timeframe: "5m", OpenedAt: t0, Close: 150.5})
	_ = repo.Upsert(ctx, port.CandleRecord{Symbol: "EUR_USD", Timeframe: "1m", OpenedAt: t0, Close: 1.1})

	got, _ := repo.ListSince(ctx, "USD_JPY", "1m", t0, 0)
	if len(got) != 1 || got[0].Close != 150.0 {
		t.Errorf("symbol/timeframe filter broken: %+v", got)
	}
}

func TestInMemoryCandleRepo_ListSinceCutoffAndLimit(t *testing.T) {
	ctx := context.Background()
	repo := NewInMemoryCandleRepo()
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		_ = repo.Upsert(ctx, port.CandleRecord{
			Symbol: "USD_JPY", Timeframe: "1m",
			OpenedAt: t0.Add(time.Duration(i) * time.Minute), Close: 150.0,
		})
	}

	// `since` cuts off the first half.
	got, _ := repo.ListSince(ctx, "USD_JPY", "1m", t0.Add(5*time.Minute), 0)
	if len(got) != 5 {
		t.Errorf("cutoff: got %d want 5", len(got))
	}
	// `limit` caps the output.
	got, _ = repo.ListSince(ctx, "USD_JPY", "1m", t0, 3)
	if len(got) != 3 {
		t.Errorf("limit: got %d want 3", len(got))
	}
}

func TestInMemoryCandleRepo_UpsertBatch(t *testing.T) {
	ctx := context.Background()
	repo := NewInMemoryCandleRepo()
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	recs := make([]port.CandleRecord, 0, 5)
	for i := 0; i < 5; i++ {
		recs = append(recs, port.CandleRecord{
			Symbol: "USD_JPY", Timeframe: "1m",
			OpenedAt: t0.Add(time.Duration(i) * time.Minute), Close: float64(i),
		})
	}
	if err := repo.UpsertBatch(ctx, recs); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}
	got, _ := repo.ListSince(ctx, "USD_JPY", "1m", t0, 0)
	if len(got) != 5 {
		t.Errorf("UpsertBatch: got %d want 5", len(got))
	}
}
