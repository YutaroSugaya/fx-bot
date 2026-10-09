package backtest

import (
	"math"
	"testing"
	"time"
)

// --- multi-symbol account aggregation ---

func mkAccTrade(symbol string, pnl float64, closedAt time.Time) Trade {
	// Symbol is not a field on Trade — backtest.Trade is symbol-agnostic.
	// AggregateAccountResult identifies per-symbol trades by which Result
	// map key they came from, so the test seeds them via the map directly.
	_ = symbol
	return Trade{
		Side:          "BUY",
		ProfitLossJPY: pnl,
		OpenedAt:      closedAt.Add(-time.Minute),
		ClosedAt:      closedAt,
	}
}

func TestBuildEquityCurve_CumulativePnL(t *testing.T) {
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	trades := []Trade{
		mkAccTrade("USD_JPY", 100, t0),
		mkAccTrade("USD_JPY", -50, t0.Add(time.Minute)),
		mkAccTrade("USD_JPY", 200, t0.Add(2*time.Minute)),
	}
	got := BuildEquityCurve(trades)
	if len(got) != 3 {
		t.Fatalf("len: %d want 3", len(got))
	}
	want := []float64{100, 50, 250}
	for i, w := range want {
		if math.Abs(got[i].Equity-w) > 1e-9 {
			t.Errorf("equity[%d]: got %v want %v", i, got[i].Equity, w)
		}
		if !got[i].Time.Equal(trades[i].ClosedAt) {
			t.Errorf("time[%d]: got %v want %v", i, got[i].Time, trades[i].ClosedAt)
		}
	}
}

func TestAggregateAccountResult_MergesChronologically(t *testing.T) {
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	usd := Result{Trades: []Trade{
		mkAccTrade("USD_JPY", 100, t0),                    // 10:00
		mkAccTrade("USD_JPY", -50, t0.Add(2*time.Minute)), // 10:02
	}}
	eur := Result{Trades: []Trade{
		mkAccTrade("EUR_JPY", 80, t0.Add(time.Minute)),    // 10:01
		mkAccTrade("EUR_JPY", -30, t0.Add(3*time.Minute)), // 10:03
	}}
	in := map[string]Result{"USD_JPY": usd, "EUR_JPY": eur}

	got := AggregateAccountResult(in)

	// Symbols map preserved by reference (test alias check)
	if len(got.Symbols) != 2 {
		t.Fatalf("Symbols len: %d want 2", len(got.Symbols))
	}

	// Combined metrics: 4 trades, wins 100+80=180, losses 50+30=80
	if got.Combined.SampleSize != 4 {
		t.Errorf("SampleSize: %d want 4", got.Combined.SampleSize)
	}
	wantPF := 180.0 / 80.0
	if math.Abs(got.Combined.ProfitFactor-wantPF) > 1e-9 {
		t.Errorf("PF: got %v want %v", got.Combined.ProfitFactor, wantPF)
	}

	// Equity curve must be in chronological merge order:
	// 10:00 (USD +100) → 100
	// 10:01 (EUR +80)  → 180
	// 10:02 (USD -50)  → 130
	// 10:03 (EUR -30)  → 100
	wantEquity := []float64{100, 180, 130, 100}
	if len(got.Equity) != len(wantEquity) {
		t.Fatalf("equity len: %d want %d", len(got.Equity), len(wantEquity))
	}
	for i, w := range wantEquity {
		if math.Abs(got.Equity[i].Equity-w) > 1e-9 {
			t.Errorf("equity[%d]: got %v want %v", i, got.Equity[i].Equity, w)
		}
	}
}

func TestAggregateAccountResult_EmptyMapReturnsZero(t *testing.T) {
	got := AggregateAccountResult(map[string]Result{})
	if got.Combined.SampleSize != 0 {
		t.Errorf("empty: SampleSize=%d want 0", got.Combined.SampleSize)
	}
	if len(got.Equity) != 0 {
		t.Errorf("empty: equity len=%d want 0", len(got.Equity))
	}
}

func TestAggregateAccountResult_MaxDrawdownAcrossSymbols(t *testing.T) {
	// Two-symbol scenario where neither symbol alone has a large drawdown
	// but the chronological merge does: USD wins +500 at 10:00, EUR loses
	// -600 at 10:01 (account peak 500 → trough -100 → DD=600).
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	usd := Result{Trades: []Trade{mkAccTrade("USD_JPY", 500, t0)}}
	eur := Result{Trades: []Trade{mkAccTrade("EUR_JPY", -600, t0.Add(time.Minute))}}

	got := AggregateAccountResult(map[string]Result{"USD_JPY": usd, "EUR_JPY": eur})
	if math.Abs(got.Combined.MaxDrawdown-600) > 1e-9 {
		t.Errorf("MaxDrawdown: got %v want 600", got.Combined.MaxDrawdown)
	}
}
