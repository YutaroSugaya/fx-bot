// cmd/histdata-ingest loads HistData.com free M1 (1-minute) CSV exports into an ISOLATED
// fxbot_backtest database, giving the backtest harness pre-2023 history (a yen-STRENGTH /
// multi-volatility regime) that the GMO 外為 API cannot serve. Without a second regime,
// JPY-pair backtest "edges" cannot be told apart from harvesting the 2023-26 yen-weakness
// trend.
//
// SAFETY: this NEVER writes to the live money DB. -apply requires a -dsn (or
// BACKTEST_DATABASE_URL) whose db name ends in "_backtest" and differs from DATABASE_URL
// (repository.SafeBacktestDSN). Default is a dry-run that opens NO database connection at all.
//
// Usage:
//
//	# 1. dry-run: parse + sanity-check every CSV under ./data/histdata, write nothing
//	go run ./cmd/histdata-ingest -dir ./data/histdata
//
//	# 2. load into the isolated backtest DB (after provisioning it — see scripts/provision_backtest_db.sh)
//	go run ./cmd/histdata-ingest -dir ./data/histdata \
//	    -dsn "postgres://fxbot:...@localhost:5432/fxbot_backtest?sslmode=disable" -apply
//
// Files are matched as DAT_ASCII_<PAIR>_M1_<period>.csv; the pair token selects the DB symbol
// (-symbol overrides for a single-pair dir). Bars are EST(no-DST) -> UTC normalised by the
// histdata parser so they align with the existing UTC candles.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/adapter/histdata"
	"fx-bot/backend/internal/adapter/repository"
	"fx-bot/backend/internal/port"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "histdata-ingest:", err)
		os.Exit(1)
	}
}

// fileSummary aggregates one symbol's parsed bars for the dry-run sanity report.
type symStats struct {
	bars               int
	minTime, maxTime   time.Time
	minClose, maxClose float64
}

func run() error {
	dir := flag.String("dir", "", "directory containing HistData M1 CSVs (recursive; required)")
	symbolOverride := flag.String("symbol", "", "DB symbol for all files (else inferred from filename)")
	dsn := flag.String("dsn", os.Getenv("BACKTEST_DATABASE_URL"), "target _backtest DSN (or BACKTEST_DATABASE_URL); required with -apply")
	apply := flag.Bool("apply", false, "write to the backtest DB (default false = dry-run, no DB connection)")
	batch := flag.Int("batch", 5000, "upsert batch size")
	useCopy := flag.Bool("copy", false, "use Postgres COPY (bulk) instead of row-by-row upsert — ~50x faster, but INSERT-only (no conflict handling): use ONLY for an initial load into an EMPTY candles table")
	flag.Parse()

	if *dir == "" {
		return fmt.Errorf("-dir is required")
	}

	var files []string
	err := filepath.WalkDir(*dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".csv") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", *dir, err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no .csv files found under %s", *dir)
	}
	sort.Strings(files)

	logger := slog.Default()

	// In -apply mode, validate the target DSN BEFORE parsing anything, so we fail fast and
	// can NEVER touch the live DB.
	var pool *pgxpool.Pool
	var repos *port.Repositories
	if *apply {
		if err := repository.SafeBacktestDSN(*dsn, os.Getenv("DATABASE_URL")); err != nil {
			return fmt.Errorf("refusing to apply: %w", err)
		}
		ctx := context.Background()
		pool, err = pgxpool.New(ctx, *dsn)
		if err != nil {
			return fmt.Errorf("connect backtest db: %w", err)
		}
		defer pool.Close()
		repos = repository.NewRepositories(pool)
		logger.Info("apply mode: writing to isolated backtest DB", "db", dbName(*dsn))
	} else {
		logger.Info("DRY-RUN: parsing + sanity-checking only, no DB connection (pass -apply to load)")
	}

	stats := map[string]*symStats{}
	grandBars := 0

	for _, f := range files {
		sym := *symbolOverride
		if sym == "" {
			s, ok := histdata.SymbolFromFilename(f)
			if !ok {
				logger.Warn("skipping: cannot infer symbol from filename", "file", f)
				continue
			}
			sym = s
		}

		fh, err := os.Open(f)
		if err != nil {
			return fmt.Errorf("open %s: %w", f, err)
		}
		recs, perr := histdata.ParseReader(sym, fh)
		fh.Close()
		if perr != nil {
			return fmt.Errorf("parse %s: %w", f, perr)
		}

		accumulate(stats, sym, recs)
		grandBars += len(recs)
		logger.Info("parsed", "file", filepath.Base(f), "symbol", sym, "bars", len(recs))

		if *apply {
			if *useCopy {
				if _, err := copyInsert(context.Background(), pool, recs); err != nil {
					return fmt.Errorf("copy %s: %w", f, err)
				}
			} else if err := upsertChunked(context.Background(), repos.Candles, recs, *batch); err != nil {
				return fmt.Errorf("upsert %s: %w", f, err)
			}
		}
	}

	printReport(stats, grandBars, *apply)
	return nil
}

func accumulate(stats map[string]*symStats, sym string, recs []port.CandleRecord) {
	s := stats[sym]
	if s == nil {
		s = &symStats{}
		stats[sym] = s
	}
	for _, r := range recs {
		if s.bars == 0 {
			s.minTime, s.maxTime = r.OpenedAt, r.OpenedAt
			s.minClose, s.maxClose = r.Close, r.Close
		}
		if r.OpenedAt.Before(s.minTime) {
			s.minTime = r.OpenedAt
		}
		if r.OpenedAt.After(s.maxTime) {
			s.maxTime = r.OpenedAt
		}
		if r.Close < s.minClose {
			s.minClose = r.Close
		}
		if r.Close > s.maxClose {
			s.maxClose = r.Close
		}
		s.bars++
	}
}

// copyInsert bulk-loads via the Postgres COPY protocol (pgx CopyFrom). Far fewer fsyncs than
// row-by-row upsert, so it's ~50x faster — critical on slow storage (macOS Docker bind mounts).
// INSERT-only: it has NO ON CONFLICT handling, so it must target an EMPTY candles table (initial
// load). For incremental top-ups over existing data, use the upsert path (omit -copy).
func copyInsert(ctx context.Context, pool *pgxpool.Pool, recs []port.CandleRecord) (int64, error) {
	// HistData files contain occasional duplicate timestamps (~1500 across 16.5M). The upsert
	// path absorbs them via ON CONFLICT; COPY is plain INSERT, so we dedupe by OpenedAt here
	// (one file = one symbol+timeframe, so OpenedAt is the unique key within recs).
	seen := make(map[time.Time]struct{}, len(recs))
	rows := make([][]any, 0, len(recs))
	for _, r := range recs {
		if _, dup := seen[r.OpenedAt]; dup {
			continue
		}
		seen[r.OpenedAt] = struct{}{}
		rows = append(rows, []any{r.Symbol, r.Timeframe, r.OpenedAt, r.Open, r.High, r.Low, r.Close, r.Volume})
	}
	return pool.CopyFrom(ctx,
		pgx.Identifier{"candles"},
		[]string{"symbol", "timeframe", "opened_at", "open", "high", "low", "close", "volume"},
		pgx.CopyFromRows(rows))
}

func upsertChunked(ctx context.Context, repo port.CandleRepository, recs []port.CandleRecord, batch int) error {
	if batch <= 0 {
		batch = 5000
	}
	for i := 0; i < len(recs); i += batch {
		end := i + batch
		if end > len(recs) {
			end = len(recs)
		}
		if err := repo.UpsertBatch(ctx, recs[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func printReport(stats map[string]*symStats, grandBars int, applied bool) {
	syms := make([]string, 0, len(stats))
	for s := range stats {
		syms = append(syms, s)
	}
	sort.Strings(syms)

	mode := "DRY-RUN (nothing written)"
	if applied {
		mode = "APPLIED to fxbot_backtest"
	}
	fmt.Printf("\n=== histdata-ingest summary — %s ===\n", mode)
	fmt.Printf("%-10s %12s  %-20s %-20s  %s\n", "SYMBOL", "BARS", "MIN (UTC)", "MAX (UTC)", "CLOSE RANGE")
	for _, s := range syms {
		st := stats[s]
		fmt.Printf("%-10s %12d  %-20s %-20s  %.5f .. %.5f\n",
			s, st.bars,
			st.minTime.Format("2006-01-02 15:04"),
			st.maxTime.Format("2006-01-02 15:04"),
			st.minClose, st.maxClose)
	}
	fmt.Printf("%-10s %12d\n", "TOTAL", grandBars)
}

func dbName(dsn string) string {
	s := dsn
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
		if q := strings.IndexByte(s, '?'); q >= 0 {
			s = s[:q]
		}
		return s
	}
	return ""
}
