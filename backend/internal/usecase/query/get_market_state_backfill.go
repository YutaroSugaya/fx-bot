package query

import (
	"context"
	"sort"
	"time"

	"fx-bot/backend/internal/domain/market"
)

// gmoBackfillJST is the calendar used to enumerate GMO's per-date kline endpoint
// (GMO keys intraday klines by JST date, YYYYMMDD).
var gmoBackfillJST = time.FixedZone("JST", 9*60*60)

// intradayBackfillSpec maps a db/resample timeframe to the GMO intraday interval
// and how many recent JST dates to stitch so the 200-period MA overlay has its
// full warmup. dates are generous (clear weekends) but capped to bound GMO load;
// the result is cached per (symbol, interval) for backfillTTL. Returns ok=false
// for timeframes with no intraday GMO source (1D/1M/3M handle their own).
func intradayBackfillSpec(s tfSpec) (gmoInterval string, dates int, ok bool) {
	base := s.dbTimeframe
	if s.source == "resample" {
		base = s.resampleFrom
		if base == "" {
			base = "1m"
		}
	}
	switch base {
	case "1m":
		return "1min", 2, true // 200 1m bars ≈ 3.5h → a day or two is plenty
	case "5m":
		return "5min", 11, true // covers 30M (200×30m = 1200×5m ≈ 5 trading days)
	case "1h":
		return "1hour", 18, true // 200 1h bars ≈ 9 trading days → ~18 calendar days
	}
	return "", 0, false
}

// backfillForMA tops up `bars` with GMO intraday history when the DB alone does
// not reach the 200-period MA window (a newly-added symbol). The DB bars are the
// fresh/authoritative recent end; GMO supplies the older warmup tail. No-op when
// the DB already has enough or the timeframe has no intraday GMO source.
func (q *GetMarketStateQuery) backfillForMA(ctx context.Context, symbol string, s tfSpec, bars []market.Candle, now time.Time) []market.Candle {
	if len(bars) >= maOverlayPeriod || q.Broker == nil {
		return bars
	}
	gmoInterval, dates, ok := intradayBackfillSpec(s)
	if !ok {
		return bars
	}
	gmo := q.intradayBackfillCached(ctx, symbol, gmoInterval, dates, now)
	if len(gmo) == 0 {
		return bars
	}
	if s.source == "resample" {
		gmo = market.Resample(gmo, s.resampleTo) // GMO 5min → the displayed 30m grid
	}
	return mergeBarsByTime(gmo, bars) // DB (bars) wins on overlap
}

// intradayBackfillCached fetches GMO klines over the last `dates` JST dates for
// the given intraday interval, caching the (sorted) result per (symbol,interval)
// for backfillTTL so the 5s-cached dashboard request never re-hits GMO per tick.
func (q *GetMarketStateQuery) intradayBackfillCached(ctx context.Context, symbol, gmoInterval string, dates int, now time.Time) []market.Candle {
	key := symbol + "|" + gmoInterval
	q.bfMu.Lock()
	if q.bf == nil {
		q.bf = map[string]backfillEntry{}
	}
	if e, ok := q.bf[key]; ok && now.Sub(e.at) < backfillTTL {
		bars := e.bars
		q.bfMu.Unlock()
		return bars
	}
	q.bfMu.Unlock()

	tokens := make([]string, 0, dates)
	for i := dates - 1; i >= 0; i-- {
		tokens = append(tokens, now.In(gmoBackfillJST).AddDate(0, 0, -i).Format("20060102"))
	}
	all := q.fetchAndSortKlines(ctx, symbol, gmoInterval, tokens)

	q.bfMu.Lock()
	if q.bf == nil {
		q.bf = map[string]backfillEntry{}
	}
	q.bf[key] = backfillEntry{at: now, bars: all}
	q.bfMu.Unlock()
	return all
}

// mergeBarsByTime merges two bar series by OpenTime, keeping the `recent` bar on
// any timestamp collision (it is the DB's own, fresher data). Returns ascending
// by time. Either input may be empty.
func mergeBarsByTime(older, recent []market.Candle) []market.Candle {
	byTime := make(map[int64]market.Candle, len(older)+len(recent))
	for _, c := range older {
		byTime[c.OpenTime.Unix()] = c
	}
	for _, c := range recent { // recent overwrites older on the same minute/bar
		byTime[c.OpenTime.Unix()] = c
	}
	out := make([]market.Candle, 0, len(byTime))
	for _, c := range byTime {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpenTime.Before(out[j].OpenTime) })
	return out
}
