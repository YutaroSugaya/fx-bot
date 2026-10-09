package query

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/ta"
	"fx-bot/backend/internal/port"
)

// GetMarketStateQuery powers GET /api/market/state — a multi-timeframe
// snapshot ("now がどんな感じか") for the manual-trade UI.
//
// For each timeframe (5M, 30M, 1H, 1D, 1M, 3M) it returns: OHLC of the
// current (in-progress) bar, change% since the bar's open, range_pips,
// a sparkline of the last ~24 closed bars, and compact candles for the chart.
//
// Source per TF:
//   - 5M / 1H: stored directly in candles table (Aggregator emits them).
//   - 30M: resampled on-the-fly from stored 1m bars.
//   - 1D / 1M: fetched from GMO klines (date=YYYY format) — the DB's 1m
//     history is typically too short for monthly granularity.
//   - 3M: fetched from GMO monthly klines and grouped into rolling 3-month bars.
//
// Stateless; caching is the handler's concern.
type GetMarketStateQuery struct {
	Candles port.CandleRepository
	Broker  port.Broker
	Clock   func() time.Time // nil → time.Now
	Logger  *slog.Logger     // nil → discard

	// bfMu/bf cache the per-date GMO intraday backfill (used only when a newly
	// added symbol lacks enough DB history for the 200MA overlay). Keyed by
	// "symbol|gmoInterval"; TTL'd so we don't re-hit GMO's per-date endpoint on
	// every (5s-cached) request.
	bfMu sync.Mutex
	bf   map[string]backfillEntry

	// lfMu/lf cache the long-TF GMO klines (1D/1M/3M). These change at most
	// daily/monthly, so without a process-lived cache a symbol switch (or any
	// 5s-TTL cache miss) re-fetched ~5 GMO date tokens serially — the bulk of the
	// "loading market state…" latency. Keyed by "symbol|interval|years|year";
	// TTL'd by longTFKlineTTL. Also dedups 1M vs 3M (same interval/years) within a
	// request, the role the old per-request map played.
	lfMu sync.Mutex
	lf   map[string]backfillEntry
}

type backfillEntry struct {
	at   time.Time
	bars []market.Candle
}

// backfillTTL bounds how long a GMO intraday backfill is reused. The recent bars
// always come fresh from the DB; this only refreshes the older warmup tail.
const backfillTTL = 10 * time.Minute

// longTFKlineTTL bounds how long the long-TF (1D/1M/3M) GMO klines are reused.
// Daily/monthly bars barely move intraday, so a few minutes of staleness on the
// overview panel is acceptable in exchange for eliminating the per-switch GMO
// round-trips.
const longTFKlineTTL = 10 * time.Minute

// CurrentRate is the live bid/ask snapshot from the broker.
type CurrentRate struct {
	Bid        float64 `json:"bid"`
	Ask        float64 `json:"ask"`
	SpreadPips float64 `json:"spread_pips"`
}

// CandleView is a compact OHLC bar used by the dashboard chart.
// Time is the bar's open time as UNIX seconds (UTC) — the frontend chart needs
// it for the time axis; lightweight-charts keys series points on it.
type CandleView struct {
	Time  int64   `json:"time"`
	Open  float64 `json:"open"`
	High  float64 `json:"high"`
	Low   float64 `json:"low"`
	Close float64 `json:"close"`
}

// TimeframeView is one card in the dashboard.
type TimeframeView struct {
	Name      string       `json:"name"` // "5M" | "30M" | "1H" | "1D" | "1M" | "3M"
	Open      float64      `json:"open"`
	High      float64      `json:"high"`
	Low       float64      `json:"low"`
	Close     float64      `json:"close"`
	RangePips float64      `json:"range_pips"`
	ChangePct float64      `json:"change_pct"` // (close-open)/open*100
	Direction string       `json:"direction"`  // "up" | "down" | "flat"
	Sparkline []float64    `json:"sparkline"`  // last ~24 closes, oldest→newest
	Candles   []CandleView `json:"candles,omitempty"`
	// MaSma200 / MaEma200 are the 200-period SMA / EMA aligned 1:1 with Candles
	// (computed over the FULL fetched history, not just the displayed window, so
	// the lines are correct from the first displayed bar). 0 at a bar means
	// "undefined" (fewer than 200 prior bars). Only populated where the fetched
	// history reaches 200 bars (1MIN/5M/30M/1H/1D); empty otherwise. Drives the overlay.
	MaSma200 []float64 `json:"ma_sma_200,omitempty"`
	MaEma200 []float64 `json:"ma_ema_200,omitempty"`
}

// MarketStateView is the full snapshot returned to the dashboard.
type MarketStateView struct {
	Symbol     string          `json:"symbol"`
	NowJST     time.Time       `json:"now_jst"`
	Current    CurrentRate     `json:"current"`
	Timeframes []TimeframeView `json:"timeframes"`
}

// sparkBars caps every sparkline at this many points so the JSON payload and
// the SVG render both stay bounded regardless of TF.
const sparkBars = 24

// candleBars caps the candlestick chart per timeframe. Sized for a full
// trading-chart view (TradingView-style), not the old 8-bar mini chart.
const candleBars = 120

// maOverlayPeriod is the 200-period MA the ma_pullback strategy watches; the
// 5M chart overlays it (computed over the full fetched history). Matches
// ta.SMA / ta.EMA so the chart agrees with the bot's own MA values.
const maOverlayPeriod = 200

// intradayFetchBars is how many of the most-recent bars the short intraday TFs
// (1MIN/5M) fetch: enough for the displayed window (candleBars) plus the 200-bar MA
// warmup behind it. Fetching by COUNT (not a fixed time window) is what keeps the
// chart continuous across the weekend market-closure gap — a fixed lookback shorter
// than the ~47h gap would, on Monday, land entirely in the closed weekend and drop
// last week's bars ("reset to bar 1").
const intradayFetchBars = candleBars + maOverlayPeriod

// closesOf extracts the close series from a candle slice so the chart-overlay
// MA can be computed by ta.SMASeries / ta.EMASeries — the single source of truth
// shared with the scalar ta.SMA / ta.EMA the bot trades on.
func closesOf(bars []market.Candle) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.Close
	}
	return out
}

// Execute composes the snapshot. Per-TF errors are logged and the TF is
// omitted from the response — the dashboard degrades gracefully when GMO
// klines fails for one interval.
func (q *GetMarketStateQuery) Execute(ctx context.Context, symbol string) (*MarketStateView, error) {
	if symbol == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	now := time.Now()
	if q.Clock != nil {
		now = q.Clock()
	}
	pip := market.PipSize(symbol)

	tk, err := q.Broker.GetTicker(ctx, symbol)
	if err != nil {
		return nil, fmt.Errorf("ticker: %w", err)
	}

	view := &MarketStateView{
		Symbol: symbol,
		NowJST: now,
		Current: CurrentRate{
			Bid: tk.Bid, Ask: tk.Ask,
			SpreadPips: (tk.Ask - tk.Bid) / pip,
		},
	}

	// Build every timeframe concurrently: each is an independent DB read and/or
	// GMO round-trip, so the wall-clock cost collapses from "sum of all TFs" to
	// "the slowest single TF". Results are placed by index to preserve the
	// short→long display order regardless of completion order.
	views := make([]*TimeframeView, len(tfSpecs))
	var wg sync.WaitGroup
	for i, spec := range tfSpecs {
		wg.Add(1)
		go func(i int, spec tfSpec) {
			defer wg.Done()
			tf, err := q.buildTimeframe(ctx, symbol, spec, now, pip)
			if err != nil {
				q.log().Warn("market_state: tf failed", "tf", spec.name, "err", err)
				return
			}
			views[i] = tf
		}(i, spec)
	}
	wg.Wait()
	for _, tf := range views {
		if tf != nil {
			view.Timeframes = append(view.Timeframes, *tf)
		}
	}
	return view, nil
}

// tfSpec describes how to source one timeframe.
type tfSpec struct {
	name         string
	source       string        // "db" | "resample" | "gmo_kline"
	dbTimeframe  string        // for source=db
	resampleFrom string        // for source=resample: base TF to read (default "1m")
	resampleTo   time.Duration // for source=resample (resample resampleFrom → this)
	gmoInterval  string        // for source=gmo_kline
	lookback     time.Duration // history window to fetch (DB sources only)
	fetchBars    int           // DB sources: cap to the newest N bars (0 = no cap / use full window). Count-based fetch survives the weekend gap.
	gmoYears     int           // for source=gmo_kline: how many recent years to stitch
	groupBars    int           // optional: aggregate every N fetched bars
}

// tfSpecs enumerates the timeframes in display order (short → long).
// Names: "1MIN" = 1-minute (NOT "1M", which is 1-month). Each source is sized so the
// 200-period MA overlay has its full 200-bar warmup BEHIND the ~120 displayed bars
// (display is still capped to candleBars; the surplus only feeds the MA), i.e. ≥320 bars.
//
// Short intraday TFs (1MIN/5M) fetch by COUNT (fetchBars = intradayFetchBars = 320) over a
// wide time floor: the floor only bounds the table scan, the LIMIT picks the newest N. This
// is what keeps the chart continuous across the ~47h weekend market-closure gap — a fixed
// window (e.g. 1MIN ≈8h / 5M ≈30h) shorter than the gap would, on Monday, land entirely
// in the closed weekend and drop last week's bars ("reset to bar 1").
// 30M (resample, ≈10d window) and 1H (≈21d window) are already long enough to span a weekend;
// 1D needs 2 years of dailies (200-day MA); 1M/3M stay short → no overlay. All come from GMO.
var tfSpecs = []tfSpec{
	{name: "1MIN", source: "db", dbTimeframe: "1m", lookback: 30 * 24 * time.Hour, fetchBars: intradayFetchBars},
	{name: "5M", source: "db", dbTimeframe: "5m", lookback: 45 * 24 * time.Hour, fetchBars: intradayFetchBars},
	{name: "30M", source: "resample", resampleFrom: "5m", resampleTo: 30 * time.Minute, lookback: 240 * time.Hour},
	{name: "1H", source: "db", dbTimeframe: "1h", lookback: 504 * time.Hour},
	{name: "1D", source: "gmo_kline", gmoInterval: "1day", gmoYears: 2},
	{name: "1M", source: "gmo_kline", gmoInterval: "1month", gmoYears: 3},
	{name: "3M", source: "gmo_kline", gmoInterval: "1month", gmoYears: 3, groupBars: 3},
}

func (q *GetMarketStateQuery) buildTimeframe(ctx context.Context, symbol string,
	s tfSpec, now time.Time, pip float64) (*TimeframeView, error) {

	var bars []market.Candle
	switch s.source {
	case "db":
		// fetchBars > 0 → count-based (newest N, weekend-gap-proof); 0 → full window.
		recs, err := q.Candles.ListSince(ctx, symbol, s.dbTimeframe, now.Add(-s.lookback), s.fetchBars)
		if err != nil {
			return nil, fmt.Errorf("db list: %w", err)
		}
		bars = candlesFromRecords(recs)
		bars = q.backfillForMA(ctx, symbol, s, bars, now)
	case "resample":
		from := s.resampleFrom
		if from == "" {
			from = "1m"
		}
		recs, err := q.Candles.ListSince(ctx, symbol, from, now.Add(-s.lookback), 0)
		if err != nil {
			return nil, fmt.Errorf("db list %s: %w", from, err)
		}
		bars = market.Resample(candlesFromRecords(recs), s.resampleTo)
		bars = q.backfillForMA(ctx, symbol, s, bars, now)
	case "gmo_kline":
		bars = q.fetchGmoKlinesCached(ctx, symbol, s.gmoInterval, s.gmoYears, now)
		if s.groupBars > 1 {
			bars = groupCandlesByCount(bars, s.groupBars)
		}
	default:
		return nil, fmt.Errorf("unknown source %q", s.source)
	}
	if len(bars) == 0 {
		return nil, fmt.Errorf("no bars")
	}
	return buildTimeframeView(s.name, bars, pip), nil
}

// fetchGmoKlinesCached serves the long-TF (1D/1M/3M) GMO klines from a
// process-lived TTL cache (q.lf), falling back to a live fetch on miss/expiry.
// The cache key folds in the year so a year rollover refetches; longTFKlineTTL
// bounds intraday staleness. This both removes the per-symbol-switch GMO
// round-trips and dedups 1M vs 3M (identical interval/years) within a request.
func (q *GetMarketStateQuery) fetchGmoKlinesCached(ctx context.Context, symbol, interval string,
	years int, now time.Time) []market.Candle {

	key := fmt.Sprintf("%s|%s|%d|%d", symbol, interval, years, now.Year())

	q.lfMu.Lock()
	if e, ok := q.lf[key]; ok && now.Sub(e.at) < longTFKlineTTL {
		bars := e.bars
		q.lfMu.Unlock()
		return bars
	}
	q.lfMu.Unlock()

	bars := q.fetchGmoKlines(ctx, symbol, interval, years, now)
	if len(bars) == 0 {
		return bars // don't cache a transient failure — retry on the next request
	}

	q.lfMu.Lock()
	if q.lf == nil {
		q.lf = map[string]backfillEntry{}
	}
	q.lf[key] = backfillEntry{at: now, bars: bars}
	q.lfMu.Unlock()
	return bars
}

// fetchGmoKlines stitches together the last N years of klines for long-TF
// intervals. GMO encodes date as YYYY for daily+. Best-effort: if a year
// fails (e.g. year-2024 returns empty for a symbol added in 2025) we skip it.
func (q *GetMarketStateQuery) fetchGmoKlines(ctx context.Context, symbol, interval string,
	years int, now time.Time) []market.Candle {

	dates := make([]string, 0, years)
	for offset := years - 1; offset >= 0; offset-- {
		dates = append(dates, fmt.Sprintf("%d", now.Year()-offset))
	}
	return q.fetchAndSortKlines(ctx, symbol, interval, dates)
}

// fetchAndSortKlines fetches GMO klines for each date token (GMO's date param is
// YYYY for daily+ and YYYYMMDD for intraday), converts each to a domain Candle
// via market.FromKline, and returns them ascending by OpenTime. A failed token
// is logged best-effort and skipped (e.g. a year before the symbol existed, or a
// weekend with no data). Shared by fetchGmoKlines (long-TF) and the intraday
// backfill so the loop / log / convert / sort skeleton lives in one place.
func (q *GetMarketStateQuery) fetchAndSortKlines(ctx context.Context, symbol, interval string, dates []string) []market.Candle {
	var all []market.Candle
	for _, d := range dates {
		klines, err := q.Broker.GetKlines(ctx, symbol, interval, d)
		if err != nil {
			q.log().Warn("market_state: gmo kline failed", "interval", interval, "date", d, "err", err)
			continue
		}
		for _, k := range klines {
			all = append(all, market.FromKline(k))
		}
	}
	// GMO returns oldest→newest within a token and we appended in order; sort
	// defensively in case a token contains overlap or ordering surprises.
	sort.Slice(all, func(i, j int) bool { return all[i].OpenTime.Before(all[j].OpenTime) })
	return all
}

// groupCandlesByCount aggregates chronological bars into fixed-size rolling
// groups aligned from the newest side. For 3-month display, this means the
// latest candle always represents the latest three monthly bars when available.
func groupCandlesByCount(candles []market.Candle, size int) []market.Candle {
	if len(candles) == 0 || size <= 1 {
		return candles
	}
	firstLen := len(candles) % size
	if firstLen == 0 {
		firstLen = size
	}
	out := make([]market.Candle, 0, (len(candles)+size-1)/size)
	for start, width := 0, firstLen; start < len(candles); width = size {
		end := start + width
		if end > len(candles) {
			end = len(candles)
		}
		out = append(out, aggregateCandles(candles[start:end]))
		start = end
	}
	return out
}

func aggregateCandles(candles []market.Candle) market.Candle {
	first := candles[0]
	last := candles[len(candles)-1]
	out := market.Candle{
		Symbol:   first.Symbol,
		OpenTime: first.OpenTime,
		Open:     first.Open,
		High:     first.High,
		Low:      first.Low,
		Close:    last.Close,
	}
	for _, c := range candles {
		if c.High > out.High {
			out.High = c.High
		}
		if c.Low < out.Low {
			out.Low = c.Low
		}
		out.Volume += c.Volume
	}
	return out
}

// candlesFromRecords converts DB records to domain candles. The DB returns
// most-recent-first; the engine + builder expect oldest-first.
func candlesFromRecords(recs []port.CandleRecord) []market.Candle {
	out := make([]market.Candle, len(recs))
	for i, r := range recs {
		out[len(recs)-1-i] = market.Candle{
			Symbol: r.Symbol, OpenTime: r.OpenedAt,
			Open: r.Open, High: r.High, Low: r.Low, Close: r.Close, Volume: r.Volume,
		}
	}
	return out
}

// buildTimeframeView derives the display fields from a chronological bar slice.
// "Current bar" = the most recent (possibly in-progress) bar. ChangePct uses
// its open vs close. Sparkline is the last sparkBars closes (or fewer if the
// history is short).
func buildTimeframeView(name string, bars []market.Candle, pip float64) *TimeframeView {
	last := bars[len(bars)-1]
	change := 0.0
	if last.Open != 0 {
		change = (last.Close - last.Open) / last.Open * 100
	}
	dir := "flat"
	switch {
	case last.Close > last.Open:
		dir = "up"
	case last.Close < last.Open:
		dir = "down"
	}
	start := len(bars) - sparkBars
	if start < 0 {
		start = 0
	}
	spark := make([]float64, 0, len(bars)-start)
	for _, b := range bars[start:] {
		spark = append(spark, b.Close)
	}
	candleStart := len(bars) - candleBars
	if candleStart < 0 {
		candleStart = 0
	}
	candles := make([]CandleView, 0, len(bars)-candleStart)
	for _, b := range bars[candleStart:] {
		candles = append(candles, CandleView{
			Time: b.OpenTime.Unix(),
			Open: b.Open, High: b.High, Low: b.Low, Close: b.Close,
		})
	}
	// 200MA overlay: compute the full series over all fetched bars, then
	// expose the slice aligned with the displayed candles. Only when there are
	// enough bars for a full 200-window somewhere in the displayed range.
	var maSma, maEma []float64
	if len(bars) >= maOverlayPeriod {
		closes := closesOf(bars)
		maSma = ta.SMASeries(closes, maOverlayPeriod)[candleStart:]
		maEma = ta.EMASeries(closes, maOverlayPeriod)[candleStart:]
	}
	return &TimeframeView{
		Name:      name,
		Open:      last.Open,
		High:      last.High,
		Low:       last.Low,
		Close:     last.Close,
		RangePips: (last.High - last.Low) / pip,
		ChangePct: change,
		Direction: dir,
		Sparkline: spark,
		Candles:   candles,
		MaSma200:  maSma,
		MaEma200:  maEma,
	}
}

func (q *GetMarketStateQuery) log() *slog.Logger {
	if q.Logger != nil {
		return q.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
