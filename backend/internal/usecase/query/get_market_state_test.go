package query

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
)

// fakeBroker is a minimal port.Broker that returns scripted ticker + klines.
//
// MOCK rationale (TESTING.md §2 古典派 3 用途):
//   - §1 システム境界: GMO API は test で本物を叩けない。
//   - §2 失敗注入: tickerErr / klinesErr で各 API 失敗時の query 挙動を確認する。
type fakeBroker struct {
	ticker     *market.Ticker
	tickerErr  error
	klinesByIv map[string][]market.Kline // interval -> bars
	klinesErr  error

	mu        sync.Mutex     // guards callsByIv (GetKlines runs concurrently per TF)
	callsByIv map[string]int // interval -> times GetKlines was invoked
}

func (b *fakeBroker) GetTicker(_ context.Context, _ string) (*market.Ticker, error) {
	return b.ticker, b.tickerErr
}
func (b *fakeBroker) GetKlines(_ context.Context, _, interval, _ string) ([]market.Kline, error) {
	b.mu.Lock()
	if b.callsByIv == nil {
		b.callsByIv = map[string]int{}
	}
	b.callsByIv[interval]++
	b.mu.Unlock()
	if b.klinesErr != nil {
		return nil, b.klinesErr
	}
	return b.klinesByIv[interval], nil
}

// calls reports how many times GetKlines was invoked for an interval.
func (b *fakeBroker) calls(interval string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.callsByIv[interval]
}
func (b *fakeBroker) GetAccountMargin(context.Context) (*order.AccountMargin, error) { return nil, nil }
func (b *fakeBroker) GetOpenPositions(context.Context, string) ([]position.Position, error) {
	return nil, nil
}
func (b *fakeBroker) GetActiveOrders(context.Context, string) ([]order.Order, error) {
	return nil, nil
}
func (b *fakeBroker) GetExecutions(context.Context, string) ([]order.Execution, error) {
	return nil, nil
}
func (b *fakeBroker) PlaceOrder(context.Context, order.PlaceOrderRequest) (*order.Order, error) {
	return nil, nil
}
func (b *fakeBroker) ClosePosition(context.Context, position.Position) (*order.Order, error) {
	return nil, nil
}
func (b *fakeBroker) CancelOrder(context.Context, string) error { return nil }

// seedCandles inserts n 1m bars walking up from start. Returns the seeded bars
// so tests can assert on derived numbers.
func seedCandles(t *testing.T, repo *backtest.InMemoryCandleRepo, symbol, tf string, start time.Time, n int, openPrice, step float64) {
	t.Helper()
	for i := 0; i < n; i++ {
		op := openPrice + step*float64(i)
		_ = repo.Upsert(context.Background(), port.CandleRecord{
			Symbol: symbol, Timeframe: tf,
			OpenedAt: start.Add(time.Duration(i) * time.Minute),
			Open:     op, High: op + 0.05, Low: op - 0.03, Close: op + 0.02,
		})
	}
}

func TestGetMarketState_Execute_ReturnsAllAvailableTimeframes(t *testing.T) {
	repo := backtest.NewInMemoryCandleRepo()
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	// Seed 1m bars for the past 14h so the 30M resample path has data.
	seedCandles(t, repo, "USD_JPY", "1m", now.Add(-14*time.Hour), 14*60, 159.00, 0.001)
	// Seed 5m and 1h bars directly.
	seedCandles(t, repo, "USD_JPY", "5m", now.Add(-3*time.Hour), 36, 159.10, 0.005)
	seedCandles(t, repo, "USD_JPY", "1h", now.Add(-28*time.Hour), 28, 158.50, 0.05)

	// GMO klines fixture for the 3 long TFs.
	klines := func(start time.Time, n int, base, step float64, dur time.Duration) []market.Kline {
		out := make([]market.Kline, n)
		for i := 0; i < n; i++ {
			op := base + step*float64(i)
			out[i] = market.Kline{
				Symbol: "USD_JPY", OpenTime: start.Add(time.Duration(i) * dur),
				Open: op, High: op + 0.3, Low: op - 0.2, Close: op + 0.1,
			}
		}
		return out
	}
	br := &fakeBroker{
		ticker: &market.Ticker{Symbol: "USD_JPY", Bid: 159.230, Ask: 159.232, Timestamp: now},
		klinesByIv: map[string][]market.Kline{
			"1day":   klines(now.AddDate(0, 0, -90), 90, 158.0, 0.01, 24*time.Hour),
			"1month": klines(now.AddDate(0, -36, 0), 36, 150.0, 0.20, 30*24*time.Hour),
		},
	}

	q := &GetMarketStateQuery{
		Candles: repo, Broker: br,
		Clock: func() time.Time { return now },
	}
	view, err := q.Execute(context.Background(), "USD_JPY")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if view.Symbol != "USD_JPY" {
		t.Errorf("symbol: %q", view.Symbol)
	}
	if view.Current.SpreadPips == 0 {
		t.Error("spread_pips should be derived from bid/ask")
	}
	gotNames := make([]string, 0, len(view.Timeframes))
	for _, tf := range view.Timeframes {
		gotNames = append(gotNames, tf.Name)
	}
	want := []string{"1MIN", "5M", "30M", "1H", "1D", "1M", "3M"}
	if !equalStrings(gotNames, want) {
		t.Errorf("timeframes: got %v want %v", gotNames, want)
	}
	for _, tf := range view.Timeframes {
		if len(tf.Sparkline) == 0 {
			t.Errorf("%s: empty sparkline", tf.Name)
		}
		if len(tf.Sparkline) > sparkBars {
			t.Errorf("%s: sparkline len %d > cap %d", tf.Name, len(tf.Sparkline), sparkBars)
		}
		if len(tf.Candles) == 0 {
			t.Errorf("%s: empty candles", tf.Name)
		}
		if len(tf.Candles) > candleBars {
			t.Errorf("%s: candle len %d > cap %d", tf.Name, len(tf.Candles), candleBars)
		}
		if tf.Close == 0 {
			t.Errorf("%s: zero close", tf.Name)
		}
	}
}

// Long-TF klines (1D/1M/3M) come from GMO and change at most daily/monthly, yet a
// symbol switch back-and-forth (or any cache-missed 5s-TTL refresh) used to re-hit
// GMO for every one of them — the bulk of the "loading market state…" latency. A
// process-lived TTL cache must serve a repeated request without re-fetching them.
func TestGetMarketState_Execute_CachesLongTFKlinesAcrossRequests(t *testing.T) {
	repo := backtest.NewInMemoryCandleRepo()
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	seedCandles(t, repo, "USD_JPY", "5m", now.Add(-3*time.Hour), 36, 159.10, 0.005)

	klines := func(start time.Time, n int, base, step float64, dur time.Duration) []market.Kline {
		out := make([]market.Kline, n)
		for i := 0; i < n; i++ {
			op := base + step*float64(i)
			out[i] = market.Kline{
				Symbol: "USD_JPY", OpenTime: start.Add(time.Duration(i) * dur),
				Open: op, High: op + 0.3, Low: op - 0.2, Close: op + 0.1,
			}
		}
		return out
	}
	br := &fakeBroker{
		ticker: &market.Ticker{Symbol: "USD_JPY", Bid: 159.230, Ask: 159.232, Timestamp: now},
		klinesByIv: map[string][]market.Kline{
			"1day":   klines(now.AddDate(0, 0, -90), 90, 158.0, 0.01, 24*time.Hour),
			"1month": klines(now.AddDate(0, -36, 0), 36, 150.0, 0.20, 30*24*time.Hour),
		},
	}
	q := &GetMarketStateQuery{Candles: repo, Broker: br, Clock: func() time.Time { return now }}

	if _, err := q.Execute(context.Background(), "USD_JPY"); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	firstDay, firstMonth := br.calls("1day"), br.calls("1month")
	if firstDay == 0 || firstMonth == 0 {
		t.Fatalf("expected long-TF GMO fetches on first request (1day=%d 1month=%d)", firstDay, firstMonth)
	}

	if _, err := q.Execute(context.Background(), "USD_JPY"); err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if got := br.calls("1day"); got != firstDay {
		t.Errorf("1day klines re-fetched on cached second request: got %d want %d (cached)", got, firstDay)
	}
	if got := br.calls("1month"); got != firstMonth {
		t.Errorf("1month klines re-fetched on cached second request: got %d want %d (cached)", got, firstMonth)
	}
}

// A transient GMO failure (empty long-TF result) must NOT be cached, or the 1D
// card would stay missing for the full TTL even after GMO recovers. Only a
// successful (non-empty) fetch is worth caching.
func TestGetMarketState_Execute_DoesNotCacheEmptyLongTF(t *testing.T) {
	repo := backtest.NewInMemoryCandleRepo()
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	seedCandles(t, repo, "USD_JPY", "5m", now.Add(-3*time.Hour), 36, 159.10, 0.005)

	br := &fakeBroker{
		ticker:     &market.Ticker{Symbol: "USD_JPY", Bid: 159.230, Ask: 159.232, Timestamp: now},
		klinesByIv: map[string][]market.Kline{}, // GMO returns nothing at first (transient)
	}
	q := &GetMarketStateQuery{Candles: repo, Broker: br, Clock: func() time.Time { return now }}

	if _, err := q.Execute(context.Background(), "USD_JPY"); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	day1 := br.calls("1day")

	// GMO recovers; a follow-up request at the SAME instant must re-attempt the
	// fetch rather than serve the cached empty result.
	br.klinesByIv["1day"] = []market.Kline{{
		Symbol: "USD_JPY", OpenTime: now.AddDate(0, 0, -1), Open: 158, High: 158.3, Low: 157.8, Close: 158.1,
	}}
	if _, err := q.Execute(context.Background(), "USD_JPY"); err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if br.calls("1day") == day1 {
		t.Errorf("empty long-TF result was cached: 1day not re-fetched after recovery (calls stayed %d)", day1)
	}
}

func TestGetMarketState_Execute_TickerErrorBubbles(t *testing.T) {
	q := &GetMarketStateQuery{
		Candles: backtest.NewInMemoryCandleRepo(),
		Broker:  &fakeBroker{tickerErr: errors.New("network")},
	}
	_, err := q.Execute(context.Background(), "USD_JPY")
	if err == nil {
		t.Fatal("expected error from ticker failure")
	}
}

func TestGetMarketState_Execute_DegradesGracefullyWhenTFMissing(t *testing.T) {
	// Only 5M data seeded; 30M/1H/1D/1M/3M should be omitted, not error.
	repo := backtest.NewInMemoryCandleRepo()
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	seedCandles(t, repo, "USD_JPY", "5m", now.Add(-3*time.Hour), 36, 159.10, 0.005)

	br := &fakeBroker{
		ticker: &market.Ticker{Symbol: "USD_JPY", Bid: 159.230, Ask: 159.232, Timestamp: now},
		// klinesByIv empty → returns empty for every interval → TF gets dropped.
	}
	q := &GetMarketStateQuery{Candles: repo, Broker: br, Clock: func() time.Time { return now }}
	view, err := q.Execute(context.Background(), "USD_JPY")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(view.Timeframes) == 0 {
		t.Fatal("at least 5M should survive")
	}
	if view.Timeframes[0].Name != "5M" {
		t.Errorf("first TF: %s", view.Timeframes[0].Name)
	}
}

func TestBuildTimeframeView_DirectionAndChange(t *testing.T) {
	cases := []struct {
		name     string
		open     float64
		close    float64
		wantDir  string
		wantSign string // "+" or "-" of change
	}{
		{"up", 100, 101, "up", "+"},
		{"down", 100, 99, "down", "-"},
		{"flat", 100, 100, "flat", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bars := []market.Candle{{Open: tc.open, High: tc.close, Low: tc.open, Close: tc.close}}
			tf := buildTimeframeView("X", bars, 0.01)
			if tf.Direction != tc.wantDir {
				t.Errorf("direction: got %s want %s", tf.Direction, tc.wantDir)
			}
			if len(tf.Candles) != 1 {
				t.Errorf("candles len: got %d want 1", len(tf.Candles))
			}
			if tc.wantSign == "+" && tf.ChangePct <= 0 {
				t.Errorf("change should be positive, got %f", tf.ChangePct)
			}
			if tc.wantSign == "-" && tf.ChangePct >= 0 {
				t.Errorf("change should be negative, got %f", tf.ChangePct)
			}
		})
	}
}

func TestBuildTimeframeView_SetsCandleTime(t *testing.T) {
	start := time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC)
	bars := []market.Candle{
		{OpenTime: start, Open: 100, High: 101, Low: 99, Close: 100.5},
		{OpenTime: start.Add(5 * time.Minute), Open: 100.5, High: 101.5, Low: 100, Close: 101},
	}
	tf := buildTimeframeView("5M", bars, 0.01)
	if len(tf.Candles) != 2 {
		t.Fatalf("candles: got %d want 2", len(tf.Candles))
	}
	if tf.Candles[0].Time != start.Unix() {
		t.Errorf("candle[0].Time: got %d want %d", tf.Candles[0].Time, start.Unix())
	}
	if tf.Candles[1].Time != start.Add(5*time.Minute).Unix() {
		t.Errorf("candle[1].Time: got %d want %d", tf.Candles[1].Time, start.Add(5*time.Minute).Unix())
	}
}

// A real trading chart needs far more than the old 8-bar mini chart. The view
// must surface all available bars (up to candleBars) so the frontend chart has
// enough history to look like an actual platform.
func TestBuildTimeframeView_ReturnsManyCandles(t *testing.T) {
	start := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	var bars []market.Candle
	for i := 0; i < 50; i++ {
		op := 100.0 + float64(i)*0.01
		bars = append(bars, market.Candle{
			OpenTime: start.Add(time.Duration(i) * time.Minute),
			Open:     op, High: op + 0.05, Low: op - 0.03, Close: op + 0.02,
		})
	}
	tf := buildTimeframeView("5M", bars, 0.01)
	if len(tf.Candles) < 50 {
		t.Errorf("expected >= 50 candles for a real chart, got %d", len(tf.Candles))
	}
}

// Bug: at the start of a new week the short-TF charts (1分/5分)
// "reset" — they show only this week's bars (from bar 1) instead of continuous history,
// while 1H/1D are fine. Root cause: the short-TF DB fetch was bounded by a fixed time
// WINDOW (5M = now-30h) that the ~47h weekend market-closure gap swallows, so on Monday
// `now - lookback` lands in the closed weekend and last week's bars fall out of range.
// The fix fetches by BAR COUNT (newest N) so the weekend gap is collapsed and pre-weekend
// bars stay on the chart. This test seeds Friday + Monday 5m bars with a weekend gap and
// asserts the 5M chart still shows the Friday (pre-weekend) bars on Monday morning.
func TestGetMarketState_ShortTF_RetainsHistoryAcrossWeekendGap(t *testing.T) {
	repo := backtest.NewInMemoryCandleRepo()
	now := time.Date(2026, 6, 22, 9, 5, 0, 0, time.UTC) // Monday morning
	weekendStart := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)

	// Friday 5m bars (pre-weekend) — these must NOT vanish at week start.
	fridayStart := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	seedCandles(t, repo, "USD_JPY", "5m", fridayStart, 100, 159.00, 0.005)
	// Monday post-open 5m bars (the only ones inside the old 30h window).
	mondayStart := time.Date(2026, 6, 22, 6, 30, 0, 0, time.UTC)
	seedCandles(t, repo, "USD_JPY", "5m", mondayStart, 30, 159.50, 0.005)

	// No GMO klines → long TFs drop, and the MA backfill is a no-op (so the only
	// 5m history is what the DB fetch returns — isolating the window-vs-count bug).
	br := &fakeBroker{ticker: &market.Ticker{Symbol: "USD_JPY", Bid: 159.230, Ask: 159.232, Timestamp: now}}
	q := &GetMarketStateQuery{Candles: repo, Broker: br, Clock: func() time.Time { return now }}

	view, err := q.Execute(context.Background(), "USD_JPY")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var tf5 *TimeframeView
	for i := range view.Timeframes {
		if view.Timeframes[i].Name == "5M" {
			tf5 = &view.Timeframes[i]
		}
	}
	if tf5 == nil || len(tf5.Candles) == 0 {
		t.Fatal("5M timeframe missing or empty")
	}
	earliest := tf5.Candles[0].Time
	if earliest >= weekendStart.Unix() {
		t.Errorf("5M chart reset at week start: earliest bar @%d is AFTER the weekend gap @%d — "+
			"pre-weekend (Friday) history was dropped (want a Friday bar as the oldest)",
			earliest, weekendStart.Unix())
	}
}

func TestGroupCandlesByCount_AlignsLatestThreeMonthGroup(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var bars []market.Candle
	for i := 0; i < 5; i++ {
		open := 100.0 + float64(i)
		bars = append(bars, market.Candle{
			Symbol: "USD_JPY", OpenTime: start.AddDate(0, i, 0),
			Open: open, High: open + 1, Low: open - 1, Close: open + 0.5, Volume: 10,
		})
	}

	grouped := groupCandlesByCount(bars, 3)
	if len(grouped) != 2 {
		t.Fatalf("groups: got %d want 2", len(grouped))
	}
	last := grouped[1]
	if !last.OpenTime.Equal(bars[2].OpenTime) {
		t.Errorf("last group open time: got %s want %s", last.OpenTime, bars[2].OpenTime)
	}
	if last.Open != bars[2].Open || last.Close != bars[4].Close {
		t.Errorf("last group OHLC: open %.3f close %.3f", last.Open, last.Close)
	}
	if last.High != bars[4].High || last.Low != bars[2].Low {
		t.Errorf("last group high/low: high %.3f low %.3f", last.High, last.Low)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
