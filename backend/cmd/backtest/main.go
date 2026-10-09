// cmd/backtest runs the Mode A (fixed-config) backtest engine over historical
// candles stored in the `candles` table and prints a Result summary.
//
// Usage:
//
//	go run ./cmd/backtest \
//	    -config configs/strategy_config.active.yaml \
//	    -from 2026-04-01 -to 2026-05-01 \
//	    -slippage 0.5 -fee 0 \
//	    -format pretty
//
// Required env: DATABASE_URL (postgres DSN).
//
// The CLI is read-only: it never writes to the DB and never invokes Claude.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/adapter/repository"
	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
)

type cliFlags struct {
	configPath       string
	symbol           string // legacy single-symbol flag
	symbols          string // CSV: "USD_JPY,EUR_JPY"; takes precedence when set
	from             string
	to               string
	slippage         float64
	fee              float64
	feeRate          float64
	spreadMedian     float64
	spreadTokyoSpike float64
	spreadFile       string // 較正済み時間帯別 spread YAML。symbol 毎に flag モデルを上書き
	swapTable        string // per-symbol/side 円/晩スワップテーブル YAML
	format           string
	pnlOut           string // optional: write per-trade net PnL JPY as a JSON array (feeds edge-judge)
	slice            string // "" | "hour" | "weekday" | "both"
	batch            string // path to BatchConfig YAML; populates symbols/period/config/slippage/fee
	replayTF         string // "1m" (default) | "1d": resample to daily for low-freq strategies
}

// spreadModelFromFlags builds the time-of-day spread model from -spread-median
// / -spread-tokyo-spike. nil when -spread-median is 0 (= use the fixed
// -slippage only). Tokyo-open window is 05-08 JST (GMO USD/JPY spreads
// blow out to ~10pips there).
func spreadModelFromFlags(f cliFlags) backtest.SpreadModel {
	if f.spreadMedian <= 0 {
		return nil
	}
	return backtest.TimeOfDaySpread{
		MedianPips:         f.spreadMedian,
		TokyoOpenSpikePips: f.spreadTokyoSpike,
		SpikeStartJST:      5,
		SpikeEndJST:        8,
	}
}

func parseFlags() cliFlags {
	var f cliFlags
	flag.StringVar(&f.configPath, "config", "", "path to strategy_config YAML (required unless -batch sets it)")
	flag.StringVar(&f.symbol, "symbol", "USD_JPY", "single symbol (legacy; use -symbols or -batch for multi)")
	flag.StringVar(&f.symbols, "symbols", "", "CSV of symbols (e.g. USD_JPY,EUR_JPY). Overrides -symbol when set.")
	flag.StringVar(&f.from, "from", "", "start date YYYY-MM-DD UTC (required unless -batch sets it)")
	flag.StringVar(&f.to, "to", "", "end date YYYY-MM-DD UTC, exclusive (required unless -batch sets it)")
	flag.Float64Var(&f.slippage, "slippage", 0, "simulated slippage pips on entry+exit (default 0)")
	flag.Float64Var(&f.fee, "fee", 0, "API fee JPY per trade, subtracted from PnL (default 0)")
	flag.Float64Var(&f.feeRate, "fee-rate", 0, "GMO 約定金額×rate%% commission, roundtrip (e.g. 0.002 = 0.002%%). 0=off")
	flag.Float64Var(&f.spreadMedian, "spread-median", 0, "time-of-day spread model: median spread pips. 0=off (use -slippage only)")
	flag.Float64Var(&f.spreadTokyoSpike, "spread-tokyo-spike", 0, "extra spread pips during the Tokyo-open window (05-08 JST); requires -spread-median > 0")
	flag.StringVar(&f.spreadFile, "spread-file", "", "path to calibrated per-hour spread YAML. Symbols present in the file override -spread-median/-spread-tokyo-spike; missing symbols fall back to the flag-built model.")
	flag.StringVar(&f.swapTable, "swap-table", "", "path to swap table YAML: swap_table: {USD_JPY: {BUY: 15.0, SELL: -18.0}} — JPY/night per 1,000 units, 21:00 UTC crossings, Wednesday ×3. Empty=off")
	flag.StringVar(&f.format, "format", "pretty", "output format: pretty | json")
	flag.StringVar(&f.pnlOut, "pnl-out", "", "also write per-trade net PnL JPY as a JSON array to this file (feeds cmd/edge-judge; always finite, unlike the +Inf-prone PF in -format json)")
	flag.StringVar(&f.slice, "slice", "", "additional breakdown: hour | weekday | both (pretty only)")
	flag.StringVar(&f.batch, "batch", "", "path to a BatchConfig YAML (symbols + period + config + costs). Explicit CLI flags override individual batch values.")
	flag.StringVar(&f.replayTF, "replay-tf", "1m", "replay timeframe: 1m (default) | 1d (resample 1m->daily for low-freq strategies like daily_trend)")
	flag.Parse()
	return f
}

// resolveSymbols returns the symbol list to backtest. -symbols (CSV) wins;
// otherwise -symbol single value (back-compat). Returns the trimmed slice.
func (f cliFlags) resolveSymbols() []string {
	if f.symbols != "" {
		parts := strings.Split(f.symbols, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if v := strings.TrimSpace(p); v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	return []string{f.symbol}
}

func main() {
	f := parseFlags()
	if err := run(f); err != nil {
		fmt.Fprintln(os.Stderr, "backtest:", err)
		os.Exit(1)
	}
}

func run(f cliFlags) error {
	if f.batch != "" {
		b, err := LoadBatchConfig(f.batch)
		if err != nil {
			return err
		}
		merged, err := applyBatch(f, b)
		if err != nil {
			return err
		}
		f = merged
	}
	if f.configPath == "" {
		return fmt.Errorf("-config is required")
	}
	if f.from == "" || f.to == "" {
		return fmt.Errorf("-from and -to are required (YYYY-MM-DD)")
	}
	from, err := time.Parse("2006-01-02", f.from)
	if err != nil {
		return fmt.Errorf("-from: %w", err)
	}
	to, err := time.Parse("2006-01-02", f.to)
	if err != nil {
		return fmt.Errorf("-to: %w", err)
	}
	if !to.After(from) {
		return fmt.Errorf("-to (%s) must be after -from (%s)", f.to, f.from)
	}

	// Load strategy config YAML.
	raw, err := os.ReadFile(f.configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	strat, err := config.ParseStrategyConfig(raw)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	// スワップテーブル (円/晩 per-symbol/side)。未指定 = nil = 無効。
	var swapTable backtest.SwapTable
	if f.swapTable != "" {
		swapTable, err = backtest.LoadSwapTable(f.swapTable)
		if err != nil {
			return fmt.Errorf("-swap-table: %w", err)
		}
	}
	// 較正済み時間帯別 spread。ファイルに entry のある symbol は
	// flag 構築の TimeOfDaySpread を上書きし、無い symbol は flag 側へフォールバック。
	var fileSpreads map[string]backtest.HourlySpread
	if f.spreadFile != "" {
		fileSpreads, err = backtest.LoadSpreadFile(f.spreadFile)
		if err != nil {
			return fmt.Errorf("-spread-file: %w", err)
		}
	}
	flagSpread := spreadModelFromFlags(f)

	// Load candles from DB.
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

	symbols := f.resolveSymbols()
	perSymbol := make(map[string]backtest.Result, len(symbols))
	barCounts := make(map[string]int, len(symbols))
	for _, sym := range symbols {
		bars, err := loadBars(ctx, repos, sym, from, to)
		if err != nil {
			return fmt.Errorf("%s: %w", sym, err)
		}
		var maxHist int
		if f.replayTF == "1d" {
			bars = market.Resample(bars, 24*time.Hour)
			maxHist = 300 // daily window: covers 200d SMA + Donchian-55
		}
		// -spread-file に sym の entry があればそれが優先。
		spread := flagSpread
		if hs, ok := fileSpreads[sym]; ok {
			spread = hs
		}
		engine := backtest.NewEngine(backtest.EngineConfig{
			Symbol:         sym,
			StrategyConfig: strat,
			MaxHistoryBars: maxHist,
			Costs: backtest.CostModel{
				SlippagePips:   f.slippage,
				FeeJPYPerTrade: f.fee,
				FeeRatePct:     f.feeRate,
				SpreadModel:    spread,
				SwapTable:      swapTable,
			},
		})
		res, err := engine.Replay(ctx, bars)
		if err != nil {
			return fmt.Errorf("%s replay: %w", sym, err)
		}
		perSymbol[sym] = res
		barCounts[sym] = len(bars)
	}

	// Single-symbol mode: emit the legacy single-Result shape so existing
	// dashboards / scripts continue to parse it. Multi-symbol mode emits
	// the AccountResult shape (per-symbol blocks + combined account view).
	if len(symbols) == 1 {
		sym := symbols[0]
		return emitSingle(f, perSymbol[sym], sym, barCounts[sym])
	}
	return emitMulti(f, backtest.AggregateAccountResult(perSymbol), symbols, barCounts)
}

// loadBars fetches 1m candles for symbol in [from, to) and returns them in
// ascending chronological order (ListSince returns DESC).
func loadBars(ctx context.Context, repos *port.Repositories, symbol string, from, to time.Time) ([]market.Candle, error) {
	// CandleRepo.ListSince は limit<=0 を 1,000,000 行キャップに変換し、
	// ORDER BY opened_at DESC なので超過時は**最古 (= train 側) の bar が黙って
	// 消える**。数年分の 1m backfill (~100 万行/symbol) はこのキャップに届くため、
	// 明示 limit + tripwire で silent truncation をハードエラー化。
	const candleFetchLimit = 5_000_000 // ≈9.5 年分の FX 1m bar。int32 に収まる
	dbRecs, err := repos.Candles.ListSince(ctx, symbol, "1m", from, candleFetchLimit)
	if err != nil {
		return nil, fmt.Errorf("list candles: %w", err)
	}
	if len(dbRecs) >= candleFetchLimit {
		return nil, fmt.Errorf("%s: candle fetch hit %d-row limit — 最古 (train) bar が黙って落ちるため中断。期間を狭めるかページングを実装すること", symbol, candleFetchLimit)
	}
	bars := make([]market.Candle, 0, len(dbRecs))
	for i := len(dbRecs) - 1; i >= 0; i-- {
		r := dbRecs[i]
		if r.OpenedAt.Before(from) || !r.OpenedAt.Before(to) {
			continue
		}
		bars = append(bars, market.Candle{
			Symbol:   r.Symbol,
			Interval: time.Minute,
			OpenTime: r.OpenedAt,
			Open:     r.Open, High: r.High, Low: r.Low, Close: r.Close,
			Volume: r.Volume,
		})
	}
	if len(bars) == 0 {
		return nil, fmt.Errorf("no candles for %s 1m — run the bot to populate the DB first", symbol)
	}
	return bars, nil
}

func emitSingle(f cliFlags, result backtest.Result, symbol string, bars int) error {
	if f.pnlOut != "" {
		pnl := make([]float64, len(result.Trades))
		for i, t := range result.Trades {
			pnl[i] = t.ProfitLossJPY
		}
		body, err := json.Marshal(pnl)
		if err != nil {
			return fmt.Errorf("marshal pnl-out: %w", err)
		}
		if err := os.WriteFile(f.pnlOut, body, 0o644); err != nil {
			return fmt.Errorf("write pnl-out: %w", err)
		}
	}
	switch f.format {
	case "json":
		body, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %w", err)
		}
		fmt.Println(string(body))
	case "pretty", "":
		fmt.Print(backtest.PrettyResult(result))
		if f.slice == "hour" || f.slice == "both" {
			fmt.Print(backtest.PrettySlice("Hour-of-Day JST",
				backtest.SliceBy(result.Trades, backtest.HourKeyJST)))
		}
		if f.slice == "weekday" || f.slice == "both" {
			fmt.Print(backtest.PrettySlice("Weekday JST",
				backtest.SliceBy(result.Trades, backtest.WeekdayKeyJST)))
		}
		fmt.Printf("\nReplayed %d bars (%s 1m) from %s to %s\n",
			bars, symbol, f.from, f.to)
		fmt.Printf("Costs: slippage=%.2f pips/leg fee=%.2f JPY/trade fee-rate=%.4f%% (約定金額×往復)\n",
			f.slippage, f.fee, f.feeRate)
	default:
		return fmt.Errorf("unknown -format %q (use pretty or json)", f.format)
	}
	return nil
}

func emitMulti(f cliFlags, account backtest.AccountResult, symbols []string, barCounts map[string]int) error {
	switch f.format {
	case "json":
		body, err := json.MarshalIndent(account, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %w", err)
		}
		fmt.Println(string(body))
	case "pretty", "":
		for _, sym := range symbols {
			fmt.Printf("\n### Symbol: %s ###\n", sym)
			fmt.Print(backtest.PrettyResult(account.Symbols[sym]))
		}
		fmt.Print(backtest.PrettyAccount(account))
		fmt.Printf("\nReplayed across symbols=%v from %s to %s (bars: ", symbols, f.from, f.to)
		for i, sym := range symbols {
			if i > 0 {
				fmt.Print(", ")
			}
			fmt.Printf("%s=%d", sym, barCounts[sym])
		}
		fmt.Println(")")
		fmt.Printf("Costs: slippage=%.2f pips/leg fee=%.2f JPY/trade fee-rate=%.4f%% (約定金額×往復)\n",
			f.slippage, f.fee, f.feeRate)
	default:
		return fmt.Errorf("unknown -format %q (use pretty or json)", f.format)
	}
	return nil
}
