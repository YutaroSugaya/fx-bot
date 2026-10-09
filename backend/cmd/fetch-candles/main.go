// cmd/fetch-candles fetches historical OHLC candles from GMO Forex and upserts
// them into the candles table (backtest replay, or warming a higher-timeframe
// strategy's history). The interval is configurable (default 1min); for the
// ma_pullback MTF trend it can backfill 1h bars (-interval 1hour).
//
// Usage:
//
//	go run ./cmd/fetch-candles \
//	    -symbol USD_JPY -from 2026-04-01 -to 2026-05-18
//	go run ./cmd/fetch-candles \
//	    -symbol GBP_JPY -interval 1hour -from 2026-05-20 -to 2026-06-11
//
// Required env: DATABASE_URL (postgres DSN).
// GMO public API is used (no API key needed).
// Rate: 1 request/second to stay inside GMO public limits.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/adapter/broker"
	"fx-bot/backend/internal/adapter/repository"
	"fx-bot/backend/internal/port"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fetch-candles:", err)
		os.Exit(1)
	}
}

func run() error {
	symbol := flag.String("symbol", "USD_JPY", "symbol")
	fromStr := flag.String("from", "", "start date YYYY-MM-DD UTC (inclusive, required)")
	toStr := flag.String("to", "", "end date YYYY-MM-DD UTC (exclusive, required)")
	interval := flag.String("interval", "1min", "GMO kline interval: 1min/5min/15min/1hour")
	flag.Parse()

	if *fromStr == "" || *toStr == "" {
		return fmt.Errorf("-from and -to are required (YYYY-MM-DD)")
	}
	// GMO interval string → DB timeframe column value (candles.timeframe).
	dbTF := map[string]string{"1min": "1m", "5min": "5m", "15min": "15m", "1hour": "1h"}[*interval]
	if dbTF == "" {
		return fmt.Errorf("unsupported -interval %q (use 1min/5min/15min/1hour)", *interval)
	}
	from, err := time.Parse("2006-01-02", *fromStr)
	if err != nil {
		return fmt.Errorf("-from: %w", err)
	}
	to, err := time.Parse("2006-01-02", *toStr)
	if err != nil {
		return fmt.Errorf("-to: %w", err)
	}
	if !to.After(from) {
		return fmt.Errorf("-to must be after -from")
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL env var is required")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()

	repos := repository.NewRepositories(pool)
	gmo := broker.NewGmoBroker(broker.GmoBrokerConfig{
		PublicBaseURL: "https://forex-api.coin.z.com/public",
		Logger:        slog.Default(),
	})

	logger := slog.Default()
	totalDays := int(to.Sub(from).Hours() / 24)
	totalInserted := 0

	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		dateStr := d.Format("20060102")
		day := int(d.Sub(from).Hours()/24) + 1
		logger.Info("fetching", "date", dateStr, "day", fmt.Sprintf("%d/%d", day, totalDays))

		klines, err := gmo.GetKlines(ctx, *symbol, *interval, dateStr)
		if err != nil {
			logger.Warn("fetch failed, skipping day", "date", dateStr, "err", err)
			time.Sleep(time.Second)
			continue
		}

		recs := make([]port.CandleRecord, 0, len(klines))
		for _, k := range klines {
			recs = append(recs, port.CandleRecord{
				Symbol:    k.Symbol,
				Timeframe: dbTF,
				OpenedAt:  k.OpenTime,
				Open:      k.Open,
				High:      k.High,
				Low:       k.Low,
				Close:     k.Close,
				Volume:    k.Volume,
			})
		}

		if len(recs) > 0 {
			if err := repos.Candles.UpsertBatch(ctx, recs); err != nil {
				return fmt.Errorf("upsert %s: %w", dateStr, err)
			}
			totalInserted += len(recs)
			logger.Info("inserted", "date", dateStr, "bars", len(recs), "total", totalInserted)
		} else {
			logger.Info("no data (weekend/holiday?)", "date", dateStr)
		}

		// Stay within GMO public rate limit (6 req/sec public; 1s conservative).
		time.Sleep(time.Second)
	}

	logger.Info("done", "symbol", *symbol, "from", *fromStr, "to", *toStr, "total_bars", totalInserted)
	return nil
}
