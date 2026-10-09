package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"

	"fx-bot/backend/internal/adapter/repository"
	"fx-bot/backend/internal/analysis"
	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
)

// SweepFile は -config で渡すスイープ定義 YAML。
type SweepFile struct {
	// StrategyConfig は全 symbol 共通のベース frozen config パス。symbol は
	// 実行時に上書きされる (CloneForOffline)。symbol 毎に別 config を使うなら
	// StrategyConfigs (map) が優先。
	StrategyConfig  string               `yaml:"strategy_config"`
	StrategyConfigs map[string]string    `yaml:"strategy_configs"`
	Symbols         []string             `yaml:"symbols"`
	Grid            map[string][]float64 `yaml:"grid"`
	Splits          struct {
		TrainFrom   string `yaml:"train_from"`
		TrainTo     string `yaml:"train_to"`
		OOSFrom     string `yaml:"oos_from"`
		OOSTo       string `yaml:"oos_to"`
		HoldoutFrom string `yaml:"holdout_from"`
		HoldoutTo   string `yaml:"holdout_to"`
	} `yaml:"splits"`
	Costs struct {
		SlippagePips     float64 `yaml:"slippage_pips"`
		FeeRatePct       float64 `yaml:"fee_rate_pct"`
		SpreadMedian     float64 `yaml:"spread_median"`
		SpreadTokyoSpike float64 `yaml:"spread_tokyo_spike"`
		SpreadFile       string  `yaml:"spread_file"`
		SwapTable        string  `yaml:"swap_table"`
		// AssumedUSDJPYRate は USD-quote pair (EUR_USD 等) の PnL を円換算する
		// 代表レート (未設定だと USD 建て PnL が JPY 建てと同じ系列に
		// プールされ ~155 倍過小重みで Judge が歪む)。
		// USD-quote symbol を含む sweep では必須 — run() が fail-fast で検証する。
		AssumedUSDJPYRate float64 `yaml:"assumed_usdjpy_rate"`
	} `yaml:"costs"`
	TopOOS int `yaml:"top_oos"`
}

// comboResult は 1 combo の 1 区間 (train or OOS) 集計。
type comboResult struct {
	Combo  Combo
	N      int
	Mean   float64 // net 期待値 (JPY/trade)
	PF     float64
	Culled bool // train PF<1 粗選抜で棄却
}

type cliFlags struct {
	configPath    string
	top           int
	seed          int64
	resamples     int
	unlockHoldout bool
	format        string
}

func parseFlags() cliFlags {
	var f cliFlags
	flag.StringVar(&f.configPath, "config", "", "sweep 定義 YAML (required)")
	flag.IntVar(&f.top, "top", 3, "OOS へ進める上位 combo 数 (sweep YAML の top_oos が優先)")
	flag.Int64Var(&f.seed, "seed", 42, "bootstrap seed (deterministic)")
	flag.IntVar(&f.resamples, "resamples", 10000, "bootstrap resamples")
	flag.BoolVar(&f.unlockHoldout, "unlock-holdout", false, "holdout (最終試験) を開封する。⚠️ 全部終わった後に 1 回だけ")
	flag.StringVar(&f.format, "format", "pretty", "pretty | json")
	flag.Parse()
	return f
}

func main() {
	f := parseFlags()
	if err := run(f); err != nil {
		fmt.Fprintln(os.Stderr, "sweep:", err)
		os.Exit(1)
	}
}

func parseSplits(sf SweepFile, now time.Time) (Splits, error) {
	s := DefaultSplits(now)
	set := func(dst *time.Time, v string) error {
		if v == "" {
			return nil
		}
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			return err
		}
		*dst = t
		return nil
	}
	for _, p := range []struct {
		dst *time.Time
		v   string
	}{
		{&s.TrainFrom, sf.Splits.TrainFrom}, {&s.TrainTo, sf.Splits.TrainTo},
		{&s.OOSFrom, sf.Splits.OOSFrom}, {&s.OOSTo, sf.Splits.OOSTo},
		{&s.HoldoutFrom, sf.Splits.HoldoutFrom}, {&s.HoldoutTo, sf.Splits.HoldoutTo},
	} {
		if err := set(p.dst, p.v); err != nil {
			return Splits{}, err
		}
	}
	return s, s.Validate()
}

// gridFromMap は YAML map を決定的順序 (キー昇順) の GridDim 列にする。
func gridFromMap(m map[string][]float64) []GridDim {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]GridDim, 0, len(keys))
	for _, k := range keys {
		out = append(out, GridDim{Key: k, Values: m[k]})
	}
	return out
}

func run(f cliFlags) error {
	if f.configPath == "" {
		return fmt.Errorf("-config is required")
	}
	raw, err := os.ReadFile(f.configPath)
	if err != nil {
		return fmt.Errorf("read sweep config: %w", err)
	}
	var sf SweepFile
	if err := yaml.Unmarshal(raw, &sf); err != nil {
		return fmt.Errorf("parse sweep config: %w", err)
	}
	if len(sf.Symbols) == 0 {
		return fmt.Errorf("symbols is required")
	}
	if sf.TopOOS > 0 {
		f.top = sf.TopOOS
	}
	splits, err := parseSplits(sf, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("splits: %w", err)
	}
	// USD-quote symbol は assumed_usdjpy_rate が無いと PnL が USD のまま混在プール
	// される (silent ~155x 過小重み)。replay 前に fail-fast。
	for _, sym := range sf.Symbols {
		if _, qerr := market.QuoteJPYRate(sym, sf.Costs.AssumedUSDJPYRate); qerr != nil {
			return fmt.Errorf("%s: %w (sweep YAML の costs.assumed_usdjpy_rate を設定すること)", sym, qerr)
		}
	}
	combos, err := ExpandGrid(gridFromMap(sf.Grid))
	if err != nil {
		return fmt.Errorf("grid: %w", err)
	}

	// ベース strategy config (per-symbol 上書き付き) をロード。
	baseFor := func(sym string) (string, error) {
		if p, ok := sf.StrategyConfigs[sym]; ok {
			return p, nil
		}
		if sf.StrategyConfig != "" {
			return sf.StrategyConfig, nil
		}
		return "", fmt.Errorf("no strategy_config for %s", sym)
	}
	baseCfgs := map[string]*config.StrategyConfig{}
	for _, sym := range sf.Symbols {
		path, err := baseFor(sym)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read strategy config %s: %w", path, err)
		}
		sc, err := config.ParseStrategyConfig(b)
		if err != nil {
			return fmt.Errorf("parse strategy config %s: %w", path, err)
		}
		baseCfgs[sym] = CloneForOffline(sc, sym)
	}

	// コストモデル部品 (cmd/backtest と同じローダ)。
	var swapTable backtest.SwapTable
	if sf.Costs.SwapTable != "" {
		if swapTable, err = backtest.LoadSwapTable(sf.Costs.SwapTable); err != nil {
			return fmt.Errorf("swap_table: %w", err)
		}
	}
	var fileSpreads map[string]backtest.HourlySpread
	if sf.Costs.SpreadFile != "" {
		if fileSpreads, err = backtest.LoadSpreadFile(sf.Costs.SpreadFile); err != nil {
			return fmt.Errorf("spread_file: %w", err)
		}
	}
	var flagSpread backtest.SpreadModel
	if sf.Costs.SpreadMedian > 0 {
		flagSpread = backtest.TimeOfDaySpread{
			MedianPips:         sf.Costs.SpreadMedian,
			TokyoOpenSpikePips: sf.Costs.SpreadTokyoSpike,
			SpikeStartJST:      5, SpikeEndJST: 8,
		}
	}
	spreadFor := func(sym string) backtest.SpreadModel {
		if hs, ok := fileSpreads[sym]; ok {
			return hs
		}
		return flagSpread
	}

	// 1m candles を全期間 (train_from〜holdout_to) で 1 回だけロードし、区間で切る。
	dsn := os.Getenv("DATABASE_URL_RO")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL_RO / DATABASE_URL env var is required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()
	repos := repository.NewRepositories(pool)

	barsBySym := map[string][]market.Candle{}
	for _, sym := range sf.Symbols {
		bars, err := loadBars(ctx, repos, sym, splits.TrainFrom, splits.HoldoutTo)
		if err != nil {
			return fmt.Errorf("%s: %w", sym, err)
		}
		barsBySym[sym] = bars
	}
	sliceBars := func(bars []market.Candle, from, to time.Time) []market.Candle {
		lo := sort.Search(len(bars), func(i int) bool { return !bars[i].OpenTime.Before(from) })
		hi := sort.Search(len(bars), func(i int) bool { return !bars[i].OpenTime.Before(to) })
		return bars[lo:hi]
	}

	// 1 combo を 1 区間で全 symbol replay して net PnL 系列を集める。
	runSpan := func(c Combo, from, to time.Time) ([]float64, error) {
		var pnls []float64
		for _, sym := range sf.Symbols {
			eng := backtest.NewEngine(backtest.EngineConfig{
				Symbol:         sym,
				StrategyConfig: baseCfgs[sym],
				Costs: backtest.CostModel{
					SlippagePips: sf.Costs.SlippagePips,
					FeeRatePct:   sf.Costs.FeeRatePct,
					SpreadModel:  spreadFor(sym),
					SwapTable:    swapTable,
				},
				// USD-quote pair の PnL を円換算して全 symbol を同一単位 (JPY) で
				// プールする (Judge の前提)。JPY-quote では無視される。
				AssumedUSDJPYRate: sf.Costs.AssumedUSDJPYRate,
			})
			// パラメータ注入: registry の ma_pullback をこの combo の値で差し替える。
			eng.Strategies.Register(strategy.MAPullback{P: c.Params})
			res, err := eng.Replay(ctx, sliceBars(barsBySym[sym], from, to))
			if err != nil {
				return nil, fmt.Errorf("%s replay: %w", sym, err)
			}
			for _, tr := range res.Trades {
				pnls = append(pnls, tr.ProfitLossJPY) // backtest PnL は摩擦込み net
			}
		}
		return pnls, nil
	}

	// --- Phase 1: train (粗選抜 PF<1 即棄却 → 期待値降順ランキング) ---
	nTrials := len(combos)
	fmt.Fprintf(os.Stderr, "sweep: N_trials=%d combos × %d symbols, train [%s, %s)\n",
		nTrials, len(sf.Symbols), splits.TrainFrom.Format("2006-01-02"), splits.TrainTo.Format("2006-01-02"))

	trainResults := make([]comboResult, 0, len(combos))
	for i, c := range combos {
		pnls, err := runSpan(c, splits.TrainFrom, splits.TrainTo)
		if err != nil {
			return err
		}
		r := comboResult{Combo: c, N: len(pnls), PF: analysis.ProfitFactor(pnls)}
		for _, v := range pnls {
			r.Mean += v
		}
		if r.N > 0 {
			r.Mean /= float64(r.N)
		}
		r.Culled = r.N == 0 || r.PF < 1.0 // 粗選抜 (train でコスト後 PF<1 は即棄却)
		trainResults = append(trainResults, r)
		fmt.Fprintf(os.Stderr, "  [%d/%d] %s → N=%d mean=%+.2f PF=%.2f%s\n",
			i+1, len(combos), c.Label, r.N, r.Mean, r.PF, map[bool]string{true: " (culled)", false: ""}[r.Culled])
	}

	survivors := make([]comboResult, 0, len(trainResults))
	for _, r := range trainResults {
		if !r.Culled {
			survivors = append(survivors, r)
		}
	}
	sortByExpectancyDesc(survivors)

	// --- プラトー確認: 最良 combo の隣接の生存率 ---
	plateau := map[string]any{}
	if len(survivors) > 0 {
		best := survivors[0]
		nb := NeighborsOf(best.Combo, combos)
		nbAlive := 0
		for _, n := range nb {
			for _, r := range trainResults {
				if r.Combo.Label == n.Label && !r.Culled {
					nbAlive++
					break
				}
			}
		}
		frac := 0.0
		if len(nb) > 0 {
			frac = float64(nbAlive) / float64(len(nb))
		}
		plateau = map[string]any{
			"best": best.Combo.Label, "neighbors": len(nb),
			"neighbors_pf_ge_1": nbAlive, "fraction": frac,
		}
	}

	// --- Phase 2: OOS (上位 top のみ・1 回) ---
	type oosOut struct {
		Label   string             `json:"label"`
		Train   map[string]float64 `json:"train"`
		OOS     map[string]any     `json:"oos"`
		Verdict string             `json:"verdict"`
	}
	oosResults := make([]oosOut, 0, f.top)
	for i, r := range survivors {
		if i >= f.top {
			break
		}
		pnls, err := runSpan(r.Combo, splits.OOSFrom, splits.OOSTo)
		if err != nil {
			return err
		}
		o := oosOut{Label: r.Combo.Label, Train: map[string]float64{"n": float64(r.N), "mean": r.Mean, "pf": r.PF}}
		jr, jerr := analysis.Judge(pnls, f.resamples, f.seed)
		if jerr != nil {
			o.OOS = map[string]any{"n": len(pnls), "error": jerr.Error()}
			o.Verdict = "continue"
		} else {
			o.OOS = map[string]any{"n": jr.N, "mean": jr.Mean, "ci_lo": jr.CILo, "ci_hi": jr.CIHi, "pf": jr.PF}
			o.Verdict = string(jr.Verdict)
		}
		oosResults = append(oosResults, o)
	}

	// --- Phase 3 (optional): holdout — 1 回だけの最終試験 ---
	var holdout []oosOut
	if f.unlockHoldout {
		fmt.Fprintln(os.Stderr, "⚠️⚠️ holdout を開封します。これは 1 回だけの最終試験 — 結果を見てから再調整したら実験無効 ⚠️⚠️")
		for i, r := range survivors {
			if i >= f.top {
				break
			}
			pnls, err := runSpan(r.Combo, splits.HoldoutFrom, splits.HoldoutTo)
			if err != nil {
				return err
			}
			o := oosOut{Label: r.Combo.Label}
			jr, jerr := analysis.Judge(pnls, f.resamples, f.seed)
			if jerr != nil {
				o.OOS = map[string]any{"n": len(pnls), "error": jerr.Error()}
				o.Verdict = "continue"
			} else {
				o.OOS = map[string]any{"n": jr.N, "mean": jr.Mean, "ci_lo": jr.CILo, "ci_hi": jr.CIHi, "pf": jr.PF}
				o.Verdict = string(jr.Verdict)
			}
			holdout = append(holdout, o)
		}
	}

	// --- 出力 ---
	report := map[string]any{
		"n_trials": nTrials,
		"symbols":  sf.Symbols,
		"splits": map[string]string{
			"train":   fmt.Sprintf("[%s, %s)", splits.TrainFrom.Format("2006-01-02"), splits.TrainTo.Format("2006-01-02")),
			"oos":     fmt.Sprintf("[%s, %s)", splits.OOSFrom.Format("2006-01-02"), splits.OOSTo.Format("2006-01-02")),
			"holdout": fmt.Sprintf("[%s, %s)", splits.HoldoutFrom.Format("2006-01-02"), splits.HoldoutTo.Format("2006-01-02")),
		},
		"culled":  len(trainResults) - len(survivors),
		"plateau": plateau,
		"oos_top": oosResults,
		"holdout": holdout,
		"seed":    f.seed,
		"caveat":  fmt.Sprintf("最良成績は N_trials=%d 込みで割り引いて解釈すること (多重比較)。合格目安 = OOS で ci_lo>0 かつ PF≥1.1 かつプラトーあり。", nTrials),
	}
	switch f.format {
	case "json":
		body, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(body))
	default:
		fmt.Printf("\n=== walk-forward sweep (N_trials=%d, 粗選抜で %d 棄却) ===\n", nTrials, len(trainResults)-len(survivors))
		fmt.Printf("train survivors (期待値降順, 上位 10):\n")
		for i, r := range survivors {
			if i >= 10 {
				break
			}
			fmt.Printf("  %2d. %-60s N=%4d mean=%+8.2f PF=%.2f\n", i+1, r.Combo.Label, r.N, r.Mean, r.PF)
		}
		if len(plateau) > 0 {
			fmt.Printf("\nプラトー確認 (best=%v): 隣接 %v 中 PF≥1 が %v (fraction=%.2f)\n",
				plateau["best"], plateau["neighbors"], plateau["neighbors_pf_ge_1"], plateau["fraction"])
		}
		fmt.Printf("\nOOS (上位 %d のみ・事前登録判定):\n", f.top)
		for _, o := range oosResults {
			fmt.Printf("  %-60s OOS=%v → verdict=%s\n", o.Label, o.OOS, o.Verdict)
		}
		if f.unlockHoldout {
			fmt.Println("\n⚠️ HOLDOUT (1 回だけの最終試験):")
			for _, o := range holdout {
				fmt.Printf("  %-60s holdout=%v → verdict=%s\n", o.Label, o.OOS, o.Verdict)
			}
		}
		fmt.Printf("\n%s\n", report["caveat"])
		fmt.Println("勝者の live 投入は必ず新 config_id + N リセット + 人間判断 (このツールは何も書かない)。")
	}
	return nil
}

// loadBars は cmd/backtest と同じ取得規約 (1m ASC, [from,to))。
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
		return nil, fmt.Errorf("no 1m candles for %s in range", symbol)
	}
	return bars, nil
}
