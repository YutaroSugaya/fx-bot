package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/safety"
)

// fakeKlineBroker is a minimal port.Broker implementation for bootstrap tests.
// Only GetKlines is meaningful; the other methods return zero-values / errors.
//
// MOCK rationale (TESTING.md §2 古典派 3 用途):
//   - §1 システム境界: GMO klines API は test で本物を叩けない。
//   - §2 失敗注入: failAll=true で GMO outage を再現し、emergency_stop trip を検証する。
type klineCall struct{ interval, date string }

type fakeKlineBroker struct {
	klines  map[string][]market.Kline // keyed by interval (e.g. "1min")
	failAll bool                      // when true, every GetKlines returns an error
	calls   []klineCall               // records every (interval, date) requested
}

func (f *fakeKlineBroker) GetTicker(_ context.Context, _ string) (*market.Ticker, error) {
	return nil, nil
}
func (f *fakeKlineBroker) GetKlines(_ context.Context, _ string, interval string, date string) ([]market.Kline, error) {
	f.calls = append(f.calls, klineCall{interval, date})
	if f.failAll {
		return nil, errors.New("simulated GMO outage")
	}
	return f.klines[interval], nil
}
func (f *fakeKlineBroker) GetAccountMargin(_ context.Context) (int, error) { return 0, nil }
func (f *fakeKlineBroker) GetOpenPositions(_ context.Context, _ string) (interface{}, error) {
	return nil, nil
}

// ↑ port.Broker has more methods. We only need GetKlines for bootstrap, so a
// minimal interface is used in BootstrapCandles (not full port.Broker).

// TestBootstrapCandles_DBEmptyGMOFails_TripsEmergencyStop is the DB-empty
// emergency-fallback core path: when neither DB nor GMO yields candles, the bot should
// trip the emergency-stop flag so the entry gate blocks all new trades.
func TestBootstrapCandles_DBEmptyGMOFails_TripsEmergencyStop(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	agg := market.NewAggregator("USD_JPY", map[time.Duration]int{
		time.Minute:      60,
		5 * time.Minute:  12,
		15 * time.Minute: 4,
		time.Hour:        2,
	})
	repo := backtest.NewInMemoryCandleRepo() // empty
	broker := &fakeKlineBroker{failAll: true}

	BootstrapCandles(ctx, broker, agg, repo, "USD_JPY", flagPath, newDiscardLogger())

	if !safety.Active(flagPath) {
		t.Fatalf("expected emergency_stop to be tripped when no candles available")
	}
}

// TestBootstrapCandles_DBHasData_NoTrip: when DB returns 24h of 1m bars, GMO
// returning nothing is fine — we have what we need.
func TestBootstrapCandles_DBHasData_NoTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")
	t0 := time.Now().UTC().Add(-30 * time.Minute)

	repo := backtest.NewInMemoryCandleRepo()
	for i := 0; i < 30; i++ {
		_ = repo.Upsert(ctx, port.CandleRecord{
			Symbol: "USD_JPY", Timeframe: "1m",
			OpenedAt: t0.Add(time.Duration(i) * time.Minute),
			Open:     150.10, High: 150.15, Low: 150.05, Close: 150.12,
		})
	}
	agg := market.NewAggregator("USD_JPY", map[time.Duration]int{time.Minute: 60})
	broker := &fakeKlineBroker{failAll: true}

	BootstrapCandles(ctx, broker, agg, repo, "USD_JPY", flagPath, newDiscardLogger())

	if safety.Active(flagPath) {
		t.Errorf("emergency_stop should NOT be tripped when DB has data")
	}
	if len(agg.Candles(time.Minute)) == 0 {
		t.Errorf("aggregator should be populated from DB")
	}
}

func TestBootstrapCandles_DBRestoreKeepsChronologicalOrder(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	now := time.Now().UTC().Truncate(time.Minute)
	start := now.Add(-120 * time.Minute)
	repo := backtest.NewInMemoryCandleRepo()
	for i := 0; i < 120; i++ {
		openedAt := start.Add(time.Duration(i) * time.Minute)
		_ = repo.Upsert(ctx, port.CandleRecord{
			Symbol: "USD_JPY", Timeframe: "1m",
			OpenedAt: openedAt,
			Open:     150.00 + float64(i)*0.001,
			High:     150.01 + float64(i)*0.001,
			Low:      149.99 + float64(i)*0.001,
			Close:    150.00 + float64(i)*0.001,
		})
	}
	agg := market.NewAggregator("USD_JPY", map[time.Duration]int{time.Minute: 200})
	broker := &fakeKlineBroker{failAll: true}

	BootstrapCandles(ctx, broker, agg, repo, "USD_JPY", flagPath, newDiscardLogger())

	snap := agg.Candles(time.Minute)
	if len(snap) != 120 {
		t.Fatalf("snapshot len: got %d want 120", len(snap))
	}
	for i := 1; i < len(snap); i++ {
		if snap[i].OpenTime.Before(snap[i-1].OpenTime) {
			t.Fatalf("snapshot not chronological at %d: %s before %s",
				i, snap[i].OpenTime, snap[i-1].OpenTime)
		}
	}
	got1h := market.SinceWindow(snap, start.Add(120*time.Minute), time.Hour)
	if len(got1h) != 60 {
		t.Fatalf("1h window len: got %d want 60", len(got1h))
	}
}

func TestBootstrapCandles_DedupesDBAndGMOOverlap(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	jst := time.FixedZone("Asia/Tokyo", 9*60*60)
	now := time.Now().UTC().Truncate(time.Minute)
	start := now.Add(-30 * time.Minute)
	repo := backtest.NewInMemoryCandleRepo()
	klines := make([]market.Kline, 0, 30)
	for i := 0; i < 30; i++ {
		openedAt := start.Add(time.Duration(i) * time.Minute).In(jst)
		_ = repo.Upsert(ctx, port.CandleRecord{
			Symbol: "USD_JPY", Timeframe: "1m",
			OpenedAt: openedAt,
			Open:     150.00, High: 150.10, Low: 149.90, Close: 150.05,
		})
		klines = append(klines, market.Kline{
			Symbol: "USD_JPY", Interval: "1min",
			OpenTime: openedAt.UTC(),
			Open:     150.00, High: 150.10, Low: 149.90, Close: 150.05,
		})
	}
	agg := market.NewAggregator("USD_JPY", map[time.Duration]int{time.Minute: 100})
	broker := &fakeKlineBroker{klines: map[string][]market.Kline{"1min": klines}}

	BootstrapCandles(ctx, broker, agg, repo, "USD_JPY", flagPath, newDiscardLogger())

	if got := len(agg.Candles(time.Minute)); got != 30 {
		t.Fatalf("deduped candle count: got %d want 30", got)
	}
}

// TestBootstrapCandles_Restores1hBeyondLast24h: the MTF pullback strategy needs
// >=12 1h bars (lookback 48). A weekend gap can leave <12 within the last 24h
// even though the DB holds days of 1h history, so the 1h restore must reach
// further back than 24h (1m/5m/15m keep 24h — they have plenty within it).
func TestBootstrapCandles_Restores1hBeyondLast24h(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	now := time.Now().UTC().Truncate(time.Hour)
	repo := backtest.NewInMemoryCandleRepo()
	// 48 hourly bars: now-48h .. now-1h. Only ~24 fall within the last 24h.
	for i := 48; i >= 1; i-- {
		_ = repo.Upsert(ctx, port.CandleRecord{
			Symbol: "USD_JPY", Timeframe: "1h",
			OpenedAt: now.Add(-time.Duration(i) * time.Hour),
			Open:     150.00, High: 150.10, Low: 149.90, Close: 150.05,
		})
	}
	agg := market.NewAggregator("USD_JPY", map[time.Duration]int{time.Hour: 200})
	broker := &fakeKlineBroker{failAll: true} // DB restore only, no GMO

	BootstrapCandles(ctx, broker, agg, repo, "USD_JPY", flagPath, newDiscardLogger())

	if got := len(agg.Candles(time.Hour)); got < 40 {
		t.Fatalf("1h restore: got %d bars, want >=40 (must reach beyond the last 24h to survive weekend gaps)", got)
	}
}

// TestBootstrapCandles_GMOBackfill1hReachesBeyond24h: for a recently-added
// symbol the DB has almost no 1h history, so GMO is the only source. The 1h
// backfill must request several days of dates (clearing weekend gaps), while
// the intraday timeframes stay at ~2 days.
func TestBootstrapCandles_GMOBackfill1hReachesBeyond24h(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")
	agg := market.NewAggregator("USD_JPY", map[time.Duration]int{time.Minute: 60, time.Hour: 200})
	repo := backtest.NewInMemoryCandleRepo()  // empty → only GMO can supply history
	broker := &fakeKlineBroker{failAll: true} // we only inspect which dates were requested

	BootstrapCandles(ctx, broker, agg, repo, "USD_JPY", flagPath, newDiscardLogger())

	dates1h := map[string]bool{}
	dates1m := map[string]bool{}
	for _, c := range broker.calls {
		if c.interval == "1hour" {
			dates1h[c.date] = true
		}
		if c.interval == "1min" {
			dates1m[c.date] = true
		}
	}
	if len(dates1h) < 5 {
		t.Fatalf("1h GMO backfill should span several days, got %d dates", len(dates1h))
	}
	if len(dates1h) <= len(dates1m) {
		t.Fatalf("1h must reach back further than 1m: 1h=%d 1m=%d dates", len(dates1h), len(dates1m))
	}
}

// TestBootstrapCandles_GMOHasData_NoTrip: opposite scenario — fresh deploy
// with empty DB, GMO klines work → backfill OK, no trip.
func TestBootstrapCandles_GMOHasData_NoTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "emergency_stop.flag")

	t0 := time.Now().UTC().Truncate(time.Minute).Add(-30 * time.Minute)
	klines := []market.Kline{}
	for i := 0; i < 30; i++ {
		klines = append(klines, market.Kline{
			Symbol: "USD_JPY", Interval: "1min",
			OpenTime: t0.Add(time.Duration(i) * time.Minute),
			Open:     150.10, High: 150.15, Low: 150.05, Close: 150.12,
		})
	}
	agg := market.NewAggregator("USD_JPY", map[time.Duration]int{time.Minute: 60})
	repo := backtest.NewInMemoryCandleRepo()
	broker := &fakeKlineBroker{klines: map[string][]market.Kline{"1min": klines}}

	BootstrapCandles(ctx, broker, agg, repo, "USD_JPY", flagPath, newDiscardLogger())

	if safety.Active(flagPath) {
		t.Errorf("emergency_stop should NOT be tripped when GMO backfill succeeds")
	}
}
