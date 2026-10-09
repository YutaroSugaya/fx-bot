package app

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/safety"
)

// KlineFetcher is the minimal Broker surface BootstrapCandles needs. Lets
// tests use a thin fake without implementing all of port.Broker.
type KlineFetcher interface {
	GetKlines(ctx context.Context, symbol, interval, date string) ([]market.Kline, error)
}

// gmoBackfillMaxDays caps how many per-date GMO kline requests a single
// timeframe makes at startup (GMO's API is per-date). 10 covers a week of
// trading for 1h while bounding cold-start API calls.
const gmoBackfillMaxDays = 10

// jstDates returns the YYYYMMDD JST date strings covering [now-window, now],
// newest first, capped at maxDays entries (always at least 2 so the day
// boundary's in-flight bar is covered).
func jstDates(now time.Time, window time.Duration, loc *time.Location, maxDays int) []string {
	days := int(window/(24*time.Hour)) + 1
	if days > maxDays {
		days = maxDays
	}
	if days < 2 {
		days = 2
	}
	nowJST := now.In(loc)
	out := make([]string, 0, days)
	for i := 0; i < days; i++ {
		out = append(out, nowJST.AddDate(0, 0, -i).Format("20060102"))
	}
	return out
}

// BootstrapCandles fills the Aggregator with recent 1m/5m/15m/1h bars on bot
// startup (1h reaches back further — see the intervals table). Strategy:
//
//  1. **DB-first restore** (CandleRepository.ListSince). If the DB has the
//     bars from a prior run, zero GMO calls are needed.
//  2. **GMO klines backfill** for any gap. UPSERTs result back to DB so the
//     next restart is faster.
//  3. **DB-empty emergency fallback**: if BOTH DB and GMO return 0 bars overall, trip
//     the emergency-stop flag so the entry gate blocks all new trades.
//     The bot keeps running (so the operator can investigate via the
//     dashboard), but it won't open positions without data.
//
// candlesRepo == nil is tolerated (logs "no repo" and skips persistence;
// useful in tests that don't need a DB).
func BootstrapCandles(
	ctx context.Context,
	b KlineFetcher,
	agg *market.Aggregator,
	candlesRepo port.CandleRepository,
	symbol, emergencyFlagPath string,
	logger *slog.Logger,
) {
	loc := config.LoadTimezoneOrUTC("Asia/Tokyo")
	now := time.Now().UTC()
	intervals := []struct {
		api     string
		tf      string
		dur     time.Duration
		restore time.Duration // how far back to restore from DB (per-timeframe)
	}{
		// 1m/5m/15m: 24h of trading holds plenty of bars. 1h is different — a
		// weekend (or holiday) gap can leave <12 bars within the last 24h even
		// though the DB has days of history, so reach back 14 days. The MTF
		// pullback strategy needs >=12 1h bars (lookback 48); the 1h ring caps
		// the surplus. (A 24h-only 1h restore after a Sunday gap leaves the
		// strategy stuck on insufficient_1h_candles.)
		{"1min", "1m", time.Minute, 24 * time.Hour},
		{"5min", "5m", 5 * time.Minute, 24 * time.Hour},
		{"15min", "15m", 15 * time.Minute, 24 * time.Hour},
		{"1hour", "1h", time.Hour, 14 * 24 * time.Hour},
	}

	collected := map[time.Duration]map[time.Time]market.Kline{}
	candleKey := func(t time.Time) time.Time {
		return t.UTC().Round(0)
	}
	ensureBucket := func(d time.Duration) map[time.Time]market.Kline {
		if collected[d] == nil {
			collected[d] = map[time.Time]market.Kline{}
		}
		return collected[d]
	}

	// Step 1: DB restore.
	dbLoaded := map[time.Duration]int{}
	totalDB := 0
	if candlesRepo != nil {
		for _, iv := range intervals {
			recs, err := candlesRepo.ListSince(ctx, symbol, iv.tf, now.Add(-iv.restore), 0)
			if err != nil {
				logger.Warn("candle_db_restore_failed", "interval", iv.tf, "err", err)
				continue
			}
			bucket := ensureBucket(iv.dur)
			for _, rec := range recs {
				openedAt := candleKey(rec.OpenedAt)
				bucket[openedAt] = market.Kline{
					Symbol:   rec.Symbol,
					Interval: iv.api,
					OpenTime: openedAt,
					Open:     rec.Open, High: rec.High, Low: rec.Low, Close: rec.Close,
					Volume: rec.Volume,
				}
			}
			dbLoaded[iv.dur] = len(recs)
			totalDB += len(recs)
			logger.Info("candle_db_restore_loaded", "interval", iv.tf, "count", len(recs))
		}
	}

	// Step 2: GMO klines backfill (always — UPSERT is idempotent). Each
	// timeframe spans as many JST dates as its restore window needs: the
	// intraday frames stay at ~2 days, 1h reaches back several days so a
	// recently-added symbol (or a post-weekend cold start) backfills enough
	// 1h history from GMO when the DB has none.
	totalGMO := 0
	for _, iv := range intervals {
		inFlight := now.Truncate(iv.dur)
		ivCutoff := now.Add(-iv.restore)
		dates := jstDates(now, iv.restore, loc, gmoBackfillMaxDays)
		loaded := 0
		var batch []port.CandleRecord
		bucket := ensureBucket(iv.dur)
		for _, d := range dates {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			ks, err := b.GetKlines(cctx, symbol, iv.api, d)
			cancel()
			if err != nil {
				logger.Warn("kline_backfill_failed", "interval", iv.api, "date", d, "err", err)
				continue
			}
			for _, k := range ks {
				k.OpenTime = candleKey(k.OpenTime)
				if k.OpenTime.Before(ivCutoff) || !k.OpenTime.Before(inFlight) {
					continue
				}
				bucket[k.OpenTime] = k
				loaded++
				if candlesRepo != nil {
					batch = append(batch, port.CandleRecord{
						Symbol:    symbol,
						Timeframe: iv.tf,
						OpenedAt:  k.OpenTime,
						Open:      k.Open, High: k.High, Low: k.Low, Close: k.Close,
						Volume: k.Volume,
					})
				}
			}
		}
		totalGMO += loaded
		logger.Info("kline_backfill_loaded",
			"interval", iv.api, "gmo_count", loaded, "db_already", dbLoaded[iv.dur])
		if candlesRepo != nil && len(batch) > 0 {
			if err := candlesRepo.UpsertBatch(ctx, batch); err != nil {
				logger.Warn("candle_db_persist_failed", "interval", iv.tf, "err", err)
			}
		}
	}

	// Step 2.5: Materialise the aggregator exactly once per unique candle in
	// chronological order. Repositories return newest-first, and GMO/DB ranges
	// overlap; appending those directly would make SinceWindow see the whole
	// buffer as "recent" and inflate 1h/6h/24h summaries.
	for _, iv := range intervals {
		bucket := collected[iv.dur]
		if len(bucket) == 0 {
			continue
		}
		klines := make([]market.Kline, 0, len(bucket))
		for _, k := range bucket {
			klines = append(klines, k)
		}
		sort.Slice(klines, func(i, j int) bool {
			return klines[i].OpenTime.Before(klines[j].OpenTime)
		})
		for _, k := range klines {
			agg.OnKline(k)
		}
		logger.Info("candle_aggregator_loaded", "interval", iv.tf, "unique_count", len(klines))
	}

	// Step 3: DB-empty emergency fallback. No bars from any source → trip emergency_stop
	// so the entry gate refuses to trade on stale or absent data.
	if totalDB == 0 && totalGMO == 0 {
		logger.Warn("candle_bootstrap_complete_failure",
			"symbol", symbol, "db_total", totalDB, "gmo_total", totalGMO,
			"action", "tripping emergency_stop")
		if err := safety.TripFor(emergencyFlagPath, safety.ReasonCandleRestoreFailed); err != nil {
			logger.Error("emergency_stop_write_failed", "err", err)
		}
	}
}
