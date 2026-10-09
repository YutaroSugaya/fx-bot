// cmd/fee-backfill は過去 trade の手数料を 0.002%/leg で推定 backfill する
// CLI。migration 0007 以前に close された trades 行は
// fee_jpy=0 / fee_estimated=false のまま — GMO 約定金額×0.002%×往復で推定し、
// fee_estimated=true を立てて broker 実報告値 (close-saga 配線済み) と区別する。
//
// Usage:
//
//	go run ./cmd/fee-backfill -from 2026-01-01T00:00:00Z                # DRY-RUN (書込なし)
//	go run ./cmd/fee-backfill -from ... -to ... -fallback-usdjpy 155.0  # 期間・換算レート指定
//	FXBOT_HUMAN_APPROVED_DB_WRITE=1 go run ./cmd/fee-backfill -from ... -apply  # 人間のみ
//
// ⚠️ -from (課金開始日) は推測しない: API キー作成から 30 日は無料期間なので、
// 人間が GMO 口座で課金開始時期を確認してから決める。
// -apply は FXBOT_HUMAN_APPROVED_DB_WRITE=1 が無ければ拒否する (live DB は read-only 原則)。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/domain/market"
)

const (
	// GMO 手数料率: 約定金額 × 0.002% を entry / exit の各 leg に課金。
	feePerLegRate = 0.00002
	// DeriveQuoteJPYRate の逆算 sanity 範囲 (USD/JPY 実勢として妥当な帯)。
	// 範囲外 = pnl 列の破損か qty 不整合とみなし fallback に落とす。
	saneRateMin = 50.0
	saneRateMax = 500.0
)

// EstimateRoundtripFeeJPY は往復 (entry leg + exit leg) の手数料を円で推定する。
//
//	fee = (entryPrice + exitPrice) × qty × 0.00002 × quoteJPYRate
//
// quote 通貨が JPY なら quoteJPYRate=1.0、USD なら USD/JPY レートを渡す。
func EstimateRoundtripFeeJPY(entryPrice, exitPrice float64, qty int, quoteJPYRate float64) float64 {
	return (entryPrice + exitPrice) * float64(qty) * feePerLegRate * quoteJPYRate
}

// DeriveQuoteJPYRate は約定金額の円換算レートを trade 行から導出する。
//
//	JPY-quote (pip 0.01): 損益がそのまま円 → 1.0 固定。
//	USD-quote (pip 0.0001): 記録済み PnL から当時のレートを逆算
//	    rate = pnlJPY / (pnlPips × 0.0001 × qty)
//	  pnlPips=0 (逆算不能) や結果が sane 範囲 [50,500] 外 (qty=0 の ±Inf/NaN、
//	  符号不一致の負値を含む) は fallbackRate に落とす。
func DeriveQuoteJPYRate(symbol string, pnlJPY, pnlPips float64, qty int, fallbackRate float64) float64 {
	if market.PipSize(symbol) == 0.01 {
		return 1.0
	}
	if pnlPips != 0 {
		rate := pnlJPY / (pnlPips * 0.0001 * float64(qty))
		if rate >= saneRateMin && rate <= saneRateMax {
			return rate
		}
	}
	return fallbackRate
}

// tradeRow は backfill 対象の trades 1 行 (SELECT 列と 1:1)。
type tradeRow struct {
	ID             int64
	Symbol         string
	Quantity       int
	EntryPrice     float64
	ExitPrice      float64
	ProfitLossPips float64
	ProfitLossJPY  float64
	// dry-run レビューで人間が対象行の素性を確認できる
	// よう closed_at と config_id を表示に含める。
	ConfigID string
	ClosedAt time.Time
}

// feeEstimate は 1 行分の推定結果 (適用前のレビュー用に rate も保持)。
type feeEstimate struct {
	Row    tradeRow
	Rate   float64
	FeeJPY float64
}

// buildEstimates は全行の fee 推定と合計を計算する (pure — DB を触らない)。
func buildEstimates(rows []tradeRow, fallbackUSDJPY float64) ([]feeEstimate, float64) {
	ests := make([]feeEstimate, 0, len(rows))
	total := 0.0
	for _, r := range rows {
		rate := DeriveQuoteJPYRate(r.Symbol, r.ProfitLossJPY, r.ProfitLossPips, r.Quantity, fallbackUSDJPY)
		fee := EstimateRoundtripFeeJPY(r.EntryPrice, r.ExitPrice, r.Quantity, rate)
		ests = append(ests, feeEstimate{Row: r, Rate: rate, FeeJPY: fee})
		total += fee
	}
	return ests, total
}

// resolveWindow は -from / -to を検証して [from, to) の期間を返す。
// -from は必須 (課金開始日は人間が決める — デフォルトで推測しない)。
// -to 省略時は now。to は from より後でなければならない。
func resolveWindow(fromStr, toStr string, now time.Time) (time.Time, time.Time, error) {
	if fromStr == "" {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"-from は必須 (RFC3339)。課金開始日は人間が GMO 口座で確認してから決めること (無料期間 = API キー作成から 30 日)")
	}
	from, err := time.Parse(time.RFC3339, fromStr)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("-from: RFC3339 で指定すること (例 2026-01-01T00:00:00Z): %w", err)
	}
	to := now
	if toStr != "" {
		to, err = time.Parse(time.RFC3339, toStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("-to: RFC3339 で指定すること: %w", err)
		}
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("-to (%s) は -from (%s) より後でなければならない", to.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	return from, to, nil
}

// checkApplyApproval は --apply の人間承認ゲート。
// CLAUDE.md: live DB は read-only。人間が個別に指定した UPDATE のみ
// FXBOT_HUMAN_APPROVED_DB_WRITE=1 を前置して実行できる (deny hook と二段構え)。
func checkApplyApproval(apply bool, humanApproved string) error {
	if !apply {
		return nil
	}
	if humanApproved != "1" {
		return fmt.Errorf(
			"-apply 拒否: FXBOT_HUMAN_APPROVED_DB_WRITE=1 が設定されていない。\n" +
				"CLAUDE.md ルール: live DB は read-only。SELECT のみ許可。人間が個別に指定した UPDATE だけ\n" +
				"FXBOT_HUMAN_APPROVED_DB_WRITE=1 をコマンドに前置して実行できる。\n" +
				"例: FXBOT_HUMAN_APPROVED_DB_WRITE=1 go run ./cmd/fee-backfill -from ... -apply")
	}
	return nil
}

// selectDSN は接続先 DSN を選ぶ。DRY-RUN は SELECT-only ロール fxbot_ro
// (DATABASE_URL_RO) を優先 (CLAUDE.md: 調査 SQL は RO ロールで)。
// --apply は書込が必要なので DATABASE_URL (RW) を使う。
func selectDSN(apply bool, ro, rw string) (string, error) {
	if apply {
		if rw == "" {
			return "", fmt.Errorf("-apply には DATABASE_URL (書込ロール) が必要")
		}
		return rw, nil
	}
	if ro != "" {
		return ro, nil
	}
	if rw != "" {
		return rw, nil
	}
	return "", fmt.Errorf("DATABASE_URL_RO か DATABASE_URL のどちらかが必要")
}

type cliFlags struct {
	from           string
	to             string
	fallbackUSDJPY float64
	apply          bool
}

func parseFlags() cliFlags {
	var f cliFlags
	flag.StringVar(&f.from, "from", "", "課金開始日 RFC3339 (必須。人間が GMO 口座で確認して決める)")
	flag.StringVar(&f.to, "to", "", "終端 RFC3339 exclusive (省略時 now)")
	flag.Float64Var(&f.fallbackUSDJPY, "fallback-usdjpy", 155.0, "USD-quote の円換算レート逆算が不能な行に使う USD/JPY fallback")
	flag.BoolVar(&f.apply, "apply", false, "true で UPDATE 実行 (FXBOT_HUMAN_APPROVED_DB_WRITE=1 必須)。default は DRY-RUN")
	flag.Parse()
	return f
}

func main() {
	f := parseFlags()
	if err := run(f); err != nil {
		fmt.Fprintln(os.Stderr, "fee-backfill:", err)
		os.Exit(1)
	}
}

func run(f cliFlags) error {
	from, to, err := resolveWindow(f.from, f.to, time.Now().UTC())
	if err != nil {
		return err
	}
	// 人間承認ゲートは DB 接続より前に評価する (未承認なら一切触らない)。
	if err := checkApplyApproval(f.apply, os.Getenv("FXBOT_HUMAN_APPROVED_DB_WRITE")); err != nil {
		return err
	}
	dsn, err := selectDSN(f.apply, os.Getenv("DATABASE_URL_RO"), os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()

	rows, err := loadTargets(ctx, pool, from, to)
	if err != nil {
		return err
	}
	ests, total := buildEstimates(rows, f.fallbackUSDJPY)
	printReport(os.Stdout, ests, total, from, to, f.apply)

	if !f.apply {
		return nil
	}
	count, err := applyEstimates(ctx, pool, ests)
	if err != nil {
		return err
	}
	fmt.Printf("APPLIED: %d 行を UPDATE (fee_jpy 推定値, fee_estimated=true) — 単一トランザクションで commit 済み\n", count)
	return nil
}

// loadTargets は backfill 対象行 (fee 未記録 かつ 期間内 close) を読む。読取専用。
//
// `fee_jpy=0 AND fee_estimated=false` だけでは 3 つの
// 母集団が混ざるため、既存の判別子で誤対象を除外する:
//   - paper close (close_saga は paper で常に zero costs を書く) →
//     strategy_configs.mode='live_config' で除外 (mode は insert-only で安定)。
//   - 0008 配線後に broker が実際に 0 を報告した live 行 (保持すべき正しいゼロ) →
//     positions.entry_fee_jpy IS NULL (= 配線前の建玉) で除外。
func loadTargets(ctx context.Context, pool *pgxpool.Pool, from, to time.Time) ([]tradeRow, error) {
	const q = backfillTargetSQL
	rs, err := pool.Query(ctx, q, from, to)
	if err != nil {
		return nil, fmt.Errorf("select trades: %w", err)
	}
	defer rs.Close()
	var out []tradeRow
	for rs.Next() {
		var r tradeRow
		if err := rs.Scan(&r.ID, &r.Symbol, &r.Quantity, &r.EntryPrice, &r.ExitPrice, &r.ProfitLossPips, &r.ProfitLossJPY, &r.ConfigID, &r.ClosedAt); err != nil {
			return nil, fmt.Errorf("scan trades: %w", err)
		}
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		return nil, fmt.Errorf("iterate trades: %w", err)
	}
	return out, nil
}

// backfillTargetSQL は対象選択の SSOT (テストが述語を pin する)。
const backfillTargetSQL = `
	SELECT t.id, t.symbol, t.quantity, t.entry_price, t.exit_price,
	       t.profit_loss_pips, t.profit_loss_jpy, t.strategy_config_id, t.closed_at
	FROM trades t
	JOIN strategy_configs sc ON sc.config_id = t.strategy_config_id
	JOIN positions p ON p.id = t.position_id
	WHERE t.fee_jpy = 0 AND t.fee_estimated = false
	  AND sc.mode = 'live_config'
	  AND p.entry_fee_jpy IS NULL
	  AND t.closed_at >= $1 AND t.closed_at < $2
	ORDER BY t.id`

// printReport は per-row 推定と合計を出力する。DRY-RUN ではヘッダで
// 「書込なし」と課金開始日の人間確認を明示する。
func printReport(w io.Writer, ests []feeEstimate, total float64, from, to time.Time, apply bool) {
	if apply {
		fmt.Fprintln(w, "APPLY MODE (FXBOT_HUMAN_APPROVED_DB_WRITE=1 承認済み) — 以下の推定値を UPDATE する")
	} else {
		fmt.Fprintln(w, "DRY-RUN (no writes). 課金開始日は人間が GMO 口座で確認してから -from を決めること (無料期間 = API キー作成から 30 日)")
	}
	fmt.Fprintf(w, "window: [%s, %s)  対象 = fee_jpy=0 AND fee_estimated=false AND mode=live_config AND entry_fee_jpy IS NULL\n\n",
		from.Format(time.RFC3339), to.Format(time.RFC3339))
	fmt.Fprintf(w, "%8s  %-8s  %6s  %10s  %10s  %9s  %12s  %-20s  %s\n",
		"id", "symbol", "qty", "entry", "exit", "rate", "est_fee_jpy", "closed_at", "config_id")
	for _, e := range ests {
		fmt.Fprintf(w, "%8d  %-8s  %6d  %10.4f  %10.4f  %9.4f  %12.4f  %-20s  %s\n",
			e.Row.ID, e.Row.Symbol, e.Row.Quantity, e.Row.EntryPrice, e.Row.ExitPrice, e.Rate, e.FeeJPY,
			e.Row.ClosedAt.UTC().Format("2006-01-02T15:04:05Z"), e.Row.ConfigID)
	}
	fmt.Fprintf(w, "\nTOTAL: %d 行 / 推定手数料合計 %.4f 円\n", len(ests), total)
}

// applyEstimates は人間承認済みの場合のみ呼ばれる書込ブランチ。
// 単一トランザクションで全行 UPDATE し、affected 件数を返す。
func applyEstimates(ctx context.Context, pool *pgxpool.Pool, ests []feeEstimate) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // commit 後の rollback は no-op
	count := 0
	for _, e := range ests {
		// 述語を再アサート: 同ツールの二重実行や別経路の書込が先行していた場合に
		// 正しい値を上書きしない (idempotent ガード)。
		tag, err := tx.Exec(ctx,
			`UPDATE trades SET fee_jpy = $1, fee_estimated = true
			 WHERE id = $2 AND fee_jpy = 0 AND fee_estimated = false`,
			e.FeeJPY, e.Row.ID)
		if err != nil {
			return 0, fmt.Errorf("update trade id=%d: %w", e.Row.ID, err)
		}
		count += int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return count, nil
}
