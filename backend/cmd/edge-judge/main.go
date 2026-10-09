// cmd/edge-judge applies the pre-registered 3-way edge judgment
// (bootstrap 95%CI + PF gate) to a NET PnL series, instead of a naive
// "N=100 pass/fail" screening.
//
// Usage:
//
//	# live trades from DB (read-only; DATABASE_URL_RO preferred):
//	go run ./cmd/edge-judge -config-id frozen-mapb-usdjpy-v3-1
//
//	# backtest/sweep pipeline (JSON array of net PnL JPY per trade):
//	go run ./cmd/edge-judge -json /tmp/pnl.json
//
//	# optional: -resamples 10000 (default) -seed 42 (default)
//
// confidence は 0.95 固定でフラグ化しない(事前登録 — 後から動かせると意味がない)。
// The CLI is read-only: SELECT only, never writes to the DB.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/analysis"
)

type cliFlags struct {
	configID  string
	jsonPath  string
	resamples int
	seed      int64
	dsrTrials int
	dsrSRVar  float64
}

func parseFlags() cliFlags {
	var f cliFlags
	flag.StringVar(&f.configID, "config-id", "", "strategy_config_id: judge live trades from the DB (read-only)")
	flag.StringVar(&f.jsonPath, "json", "", "path to a JSON array of net PnL JPY per trade (backtest/sweep pipeline)")
	flag.IntVar(&f.resamples, "resamples", 10000, "bootstrap resamples")
	flag.Int64Var(&f.seed, "seed", 42, "RNG seed (same seed reproduces the same CI)")
	flag.IntVar(&f.dsrTrials, "dsr-trials", 0, "if >0, also print Sharpe / PSR / Deflated Sharpe. Honest count of EVERY variant tried (strategies × params × thresholds)")
	flag.Float64Var(&f.dsrSRVar, "dsr-sr-variance", 0, "variance of the per-trade Sharpe across the trials (needed for the multiple-testing deflation; 0 = report PSR(0) only)")
	flag.Parse()
	return f
}

func main() {
	f := parseFlags()
	if err := run(f); err != nil {
		fmt.Fprintln(os.Stderr, "edge-judge:", err)
		os.Exit(1)
	}
}

func run(f cliFlags) error {
	// 入力モードは排他: -config-id (DB) xor -json (file)。
	if (f.configID == "") == (f.jsonPath == "") {
		return fmt.Errorf("exactly one of -config-id or -json is required")
	}

	var (
		values []float64
		source string
		err    error
	)
	if f.jsonPath != "" {
		values, err = loadValuesFromJSON(f.jsonPath)
		source = fmt.Sprintf("json file %s", f.jsonPath)
	} else {
		values, err = loadValuesFromDB(context.Background(), f.configID)
		source = fmt.Sprintf("DB trades (config_id=%s, net = pnl - fee + swap)", f.configID)
	}
	if err != nil {
		return err
	}

	res, err := analysis.Judge(values, f.resamples, f.seed)
	if err != nil {
		return err
	}

	printHuman(res, source, f)

	if f.dsrTrials > 0 {
		printDSR(values, f)
	}

	line, err := analysis.RenderJSONLine(res)
	if err != nil {
		return err
	}
	fmt.Println(line)
	return nil
}

// loadValuesFromJSON は JSON 配列(数値 = 取引ごとの net PnL JPY)を読む。
// backtest/sweep パイプラインが DB を介さず結果を流し込むための入口。
func loadValuesFromJSON(path string) ([]float64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read -json file: %w", err)
	}
	var values []float64
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("parse -json file (want a JSON array of numbers): %w", err)
	}
	return values, nil
}

// loadValuesFromDB は trades からコスト控除後 net PnL を closed_at 順で読む。
// READ-ONLY: DATABASE_URL_RO(SELECT-only ロール)があれば優先する。SELECT のみ。
func loadValuesFromDB(ctx context.Context, configID string) ([]float64, error) {
	dsn := os.Getenv("DATABASE_URL_RO")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		return nil, fmt.Errorf("DATABASE_URL_RO or DATABASE_URL env var is required for -config-id mode")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()

	rows, err := pool.Query(ctx,
		`SELECT (profit_loss_jpy - fee_jpy + swap_jpy)
		   FROM trades
		  WHERE strategy_config_id = $1
		  ORDER BY closed_at`, configID)
	if err != nil {
		return nil, fmt.Errorf("query trades: %w", err)
	}
	defer rows.Close()

	var values []float64
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan trade pnl: %w", err)
		}
		values = append(values, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trades: %w", err)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("no closed trades for config_id=%q", configID)
	}
	return values, nil
}

// printHuman は人間向けブロックを出力する。最後の 1 行 JSON は run() が別途出す。
func printHuman(res analysis.JudgeResult, source string, f cliFlags) {
	fmt.Println("=== edge-judge: 事前登録 3-way 判定 (bootstrap 95%CI + PF) ===")
	fmt.Printf("source    : %s\n", source)
	fmt.Printf("resamples : %d  seed: %d  confidence: %.2f (固定)\n",
		f.resamples, f.seed, analysis.JudgeConfidence)
	fmt.Printf("N         : %d trades\n", res.N)
	fmt.Printf("mean      : %+.2f JPY/trade\n", res.Mean)
	fmt.Printf("95%% CI    : [%+.2f, %+.2f] JPY\n", res.CILo, res.CIHi)
	fmt.Printf("PF        : %.2f\n", res.PF)
	fmt.Printf("verdict   : %s\n", res.Verdict)
	for _, r := range res.Reasons {
		fmt.Printf("  - %s\n", r)
	}
	fmt.Println()
}

// printDSR は Sharpe / PSR(0) / Deflated Sharpe を出す(多重検定で割り引いた
// 「真の Sharpe が正である確率」)。dsr-sr-variance 未指定(0)のときは deflation
// を適用できないので PSR(0) のみ。
func printDSR(values []float64, f cliFlags) {
	fmt.Println("=== Deflated Sharpe (多重検定の割引: Bailey & López de Prado) ===")
	sr, ok := analysis.SharpeRatio(values)
	if !ok {
		fmt.Println("Sharpe    : undefined (n<2 or zero variance)")
		fmt.Println()
		return
	}
	fmt.Printf("Sharpe    : %.3f / trade  (trials counted: %d)\n", sr, f.dsrTrials)
	if psr0, ok := analysis.ProbabilisticSharpeRatio(values, 0); ok {
		fmt.Printf("PSR(0)    : %.3f  (P[true SR>0], skew/kurt-adjusted)\n", psr0)
	}
	if f.dsrSRVar > 0 {
		if dsr, ok := analysis.DeflatedSharpeRatio(values, f.dsrTrials, f.dsrSRVar); ok {
			fmt.Printf("DSR       : %.3f  (P[true SR > luckiest of %d trials], srVar=%.3f)\n", dsr, f.dsrTrials, f.dsrSRVar)
			if dsr < 0.95 {
				fmt.Println("  ⚠ DSR<0.95: 多重検定を割り引くと正エッジは確証できない")
			}
		}
	} else {
		fmt.Println("DSR       : (skipped — pass -dsr-sr-variance = Var(Sharpe across trials) to deflate)")
	}
	fmt.Println()
}
