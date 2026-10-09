package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

// 過去 trade の fee backfill 推定 (0.002%/leg 往復)。
// pure 関数のみをテストする — DB 接続を要するテストはこの CLI では書かない
// (live DB は read-only、integration テスト禁止)。

func TestEstimateRoundtripFeeJPY(t *testing.T) {
	tests := []struct {
		name         string
		entryPrice   float64
		exitPrice    float64
		qty          int
		quoteJPYRate float64
		want         float64
	}{
		{
			// (155.00+155.50) × 1000 × 0.00002 × 1.0 = 6.21 円
			name:       "USD_JPY 1000通貨 (rate=1.0)",
			entryPrice: 155.00, exitPrice: 155.50, qty: 1000, quoteJPYRate: 1.0,
			want: 6.21,
		},
		{
			// (1.0850+1.0860) × 1000 × 0.00002 × 155 = 6.7301 円
			name:       "EUR_USD 1000通貨 (rate=155 で円換算)",
			entryPrice: 1.0850, exitPrice: 1.0860, qty: 1000, quoteJPYRate: 155.0,
			want: 6.7301,
		},
		{
			// qty 10 倍で fee も線形に 10 倍。
			name:       "qty 10000 は 10 倍",
			entryPrice: 155.00, exitPrice: 155.50, qty: 10000, quoteJPYRate: 1.0,
			want: 62.1,
		},
		{
			name:       "qty 0 は 0 円",
			entryPrice: 155.00, exitPrice: 155.50, qty: 0, quoteJPYRate: 1.0,
			want: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EstimateRoundtripFeeJPY(tc.entryPrice, tc.exitPrice, tc.qty, tc.quoteJPYRate)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("EstimateRoundtripFeeJPY(%v,%v,%d,%v) = %v, want %v",
					tc.entryPrice, tc.exitPrice, tc.qty, tc.quoteJPYRate, got, tc.want)
			}
		})
	}
}

func TestDeriveQuoteJPYRate(t *testing.T) {
	tests := []struct {
		name         string
		symbol       string
		pnlJPY       float64
		pnlPips      float64
		qty          int
		fallbackRate float64
		want         float64
	}{
		{
			// JPY-quote (pip 0.01) は損益がそのまま円 → 倍率 1.0 固定。
			name:   "JPY-quote は常に 1.0",
			symbol: "USD_JPY", pnlJPY: 500, pnlPips: 50, qty: 1000, fallbackRate: 155,
			want: 1.0,
		},
		{
			name:   "JPY cross も 1.0",
			symbol: "GBP_JPY", pnlJPY: -120, pnlPips: -12, qty: 1000, fallbackRate: 155,
			want: 1.0,
		},
		{
			// rate = 155.0 / (10 × 0.0001 × 1000) = 155.0 (当時の実レートを PnL から逆算)。
			name:   "USD-quote は PnL から逆算",
			symbol: "EUR_USD", pnlJPY: 155.0, pnlPips: 10.0, qty: 1000, fallbackRate: 99,
			want: 155.0,
		},
		{
			// 負け trade でも符号が一致していれば正のレートに逆算できる。
			name:   "負け trade も符号一致なら逆算",
			symbol: "EUR_USD", pnlJPY: -77.5, pnlPips: -5.0, qty: 1000, fallbackRate: 99,
			want: 155.0,
		},
		{
			// pnlPips=0 はゼロ除算になるので fallback。
			name:   "pnlPips=0 は fallback",
			symbol: "EUR_USD", pnlJPY: 3.0, pnlPips: 0, qty: 1000, fallbackRate: 155,
			want: 155,
		},
		{
			// 10000 / (1 × 0.0001 × 1000) = 100000 → sane 範囲 [50,500] 外 → fallback。
			name:   "逆算が sane 範囲外 (高すぎ) は fallback",
			symbol: "EUR_USD", pnlJPY: 10000, pnlPips: 1.0, qty: 1000, fallbackRate: 155,
			want: 155,
		},
		{
			// 符号不一致 (データ破損) は負レートになる → fallback。
			name:   "符号不一致 (負レート) は fallback",
			symbol: "EUR_USD", pnlJPY: -155, pnlPips: 10, qty: 1000, fallbackRate: 155,
			want: 155,
		},
		{
			// qty=0 はゼロ除算 (±Inf) → sane 判定で弾かれ fallback。
			name:   "qty=0 は fallback",
			symbol: "EUR_USD", pnlJPY: 155, pnlPips: 10, qty: 0, fallbackRate: 155,
			want: 155,
		},
		{
			// 境界値: 50 ちょうどは sane (inclusive)。50 / (10 × 0.0001 × 1000) = 50。
			name:   "境界 50 は sane",
			symbol: "EUR_USD", pnlJPY: 50, pnlPips: 10, qty: 1000, fallbackRate: 99,
			want: 50,
		},
		{
			// 境界値: 500 ちょうども sane (inclusive)。
			name:   "境界 500 は sane",
			symbol: "EUR_USD", pnlJPY: 500, pnlPips: 10, qty: 1000, fallbackRate: 99,
			want: 500,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveQuoteJPYRate(tc.symbol, tc.pnlJPY, tc.pnlPips, tc.qty, tc.fallbackRate)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("DeriveQuoteJPYRate(%q,%v,%v,%d,%v) = %v, want %v",
					tc.symbol, tc.pnlJPY, tc.pnlPips, tc.qty, tc.fallbackRate, got, tc.want)
			}
		})
	}
}

func TestResolveWindow(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		fromStr  string
		toStr    string
		wantFrom time.Time
		wantTo   time.Time
		wantErr  bool
	}{
		{
			// 課金開始日は人間が決める — デフォルトで推測しない (必須 flag)。
			name:    "-from 未指定は error",
			fromStr: "", toStr: "",
			wantErr: true,
		},
		{
			name:    "-from が RFC3339 でないのは error",
			fromStr: "2026-05-01", toStr: "",
			wantErr: true,
		},
		{
			name:    "-to 省略は now がデフォルト",
			fromStr: "2026-05-01T00:00:00Z", toStr: "",
			wantFrom: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			wantTo:   now,
		},
		{
			name:    "-to 指定は RFC3339 でパース",
			fromStr: "2026-05-01T00:00:00Z", toStr: "2026-06-01T00:00:00Z",
			wantFrom: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			wantTo:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:    "-to が RFC3339 でないのは error",
			fromStr: "2026-05-01T00:00:00Z", toStr: "2026-06-01",
			wantErr: true,
		},
		{
			name:    "-to <= -from は error",
			fromStr: "2026-06-01T00:00:00Z", toStr: "2026-05-01T00:00:00Z",
			wantErr: true,
		},
		{
			name:    "-to == -from も error (区間が空)",
			fromStr: "2026-06-01T00:00:00Z", toStr: "2026-06-01T00:00:00Z",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			from, to, err := resolveWindow(tc.fromStr, tc.toStr, now)
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolveWindow(%q,%q) err=%v wantErr=%v", tc.fromStr, tc.toStr, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if !from.Equal(tc.wantFrom) || !to.Equal(tc.wantTo) {
				t.Errorf("resolveWindow(%q,%q) = (%v,%v), want (%v,%v)",
					tc.fromStr, tc.toStr, from, to, tc.wantFrom, tc.wantTo)
			}
		})
	}
}

func TestCheckApplyApproval(t *testing.T) {
	tests := []struct {
		name          string
		apply         bool
		humanApproved string
		wantErr       bool
	}{
		{name: "DRY-RUN は env 不要", apply: false, humanApproved: "", wantErr: false},
		{name: "DRY-RUN は env があっても素通り", apply: false, humanApproved: "1", wantErr: false},
		{name: "--apply で env 未設定は拒否", apply: true, humanApproved: "", wantErr: true},
		{name: "--apply で env=0 は拒否", apply: true, humanApproved: "0", wantErr: true},
		{name: "--apply で env=true も拒否 (厳密に 1 のみ)", apply: true, humanApproved: "true", wantErr: true},
		{name: "--apply + env=1 で許可", apply: true, humanApproved: "1", wantErr: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkApplyApproval(tc.apply, tc.humanApproved)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkApplyApproval(%v,%q) err=%v wantErr=%v", tc.apply, tc.humanApproved, err, tc.wantErr)
			}
			// 拒否メッセージは CLAUDE.md ルール (env 名) を必ず含む。
			if tc.wantErr && !strings.Contains(err.Error(), "FXBOT_HUMAN_APPROVED_DB_WRITE") {
				t.Errorf("error %q should mention FXBOT_HUMAN_APPROVED_DB_WRITE", err.Error())
			}
		})
	}
}

func TestSelectDSN(t *testing.T) {
	tests := []struct {
		name    string
		apply   bool
		ro      string
		rw      string
		want    string
		wantErr bool
	}{
		{
			// 調査 SQL は SELECT-only ロール fxbot_ro を優先 (CLAUDE.md)。
			name:  "DRY-RUN は DATABASE_URL_RO 優先",
			apply: false, ro: "postgres://ro", rw: "postgres://rw",
			want: "postgres://ro",
		},
		{
			name:  "DRY-RUN で RO 未設定は DATABASE_URL に fallback",
			apply: false, ro: "", rw: "postgres://rw",
			want: "postgres://rw",
		},
		{
			name:  "DRY-RUN で両方未設定は error",
			apply: false, ro: "", rw: "",
			wantErr: true,
		},
		{
			// 書込は SELECT-only ロールでは不可能なので RW を使う。
			name:  "--apply は DATABASE_URL (RW) を使う",
			apply: true, ro: "postgres://ro", rw: "postgres://rw",
			want: "postgres://rw",
		},
		{
			name:  "--apply で DATABASE_URL 未設定は error",
			apply: true, ro: "postgres://ro", rw: "",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectDSN(tc.apply, tc.ro, tc.rw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("selectDSN(%v,%q,%q) err=%v wantErr=%v", tc.apply, tc.ro, tc.rw, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got != tc.want {
				t.Errorf("selectDSN(%v,%q,%q) = %q, want %q", tc.apply, tc.ro, tc.rw, got, tc.want)
			}
		})
	}
}

func TestBuildEstimates(t *testing.T) {
	rows := []tradeRow{
		{
			// JPY-quote: rate=1.0、fee = (155.00+155.50)×1000×0.00002 = 6.21 円。
			ID: 1, Symbol: "USD_JPY", Quantity: 1000,
			EntryPrice: 155.00, ExitPrice: 155.50,
			ProfitLossPips: 50, ProfitLossJPY: 500,
		},
		{
			// USD-quote: rate を PnL から逆算 (155.0/(10×0.0001×1000)=155)、
			// fee = (1.0850+1.0860)×1000×0.00002×155 = 6.7301 円。
			ID: 2, Symbol: "EUR_USD", Quantity: 1000,
			EntryPrice: 1.0850, ExitPrice: 1.0860,
			ProfitLossPips: 10, ProfitLossJPY: 155.0,
		},
	}
	ests, total := buildEstimates(rows, 99.0)
	if len(ests) != 2 {
		t.Fatalf("len(ests) = %d, want 2", len(ests))
	}
	if math.Abs(ests[0].FeeJPY-6.21) > 1e-9 {
		t.Errorf("ests[0].FeeJPY = %v, want 6.21", ests[0].FeeJPY)
	}
	if math.Abs(ests[0].Rate-1.0) > 1e-9 {
		t.Errorf("ests[0].Rate = %v, want 1.0", ests[0].Rate)
	}
	if math.Abs(ests[1].FeeJPY-6.7301) > 1e-9 {
		t.Errorf("ests[1].FeeJPY = %v, want 6.7301", ests[1].FeeJPY)
	}
	if math.Abs(ests[1].Rate-155.0) > 1e-9 {
		t.Errorf("ests[1].Rate = %v, want 155.0", ests[1].Rate)
	}
	if math.Abs(total-(6.21+6.7301)) > 1e-9 {
		t.Errorf("total = %v, want %v", total, 6.21+6.7301)
	}
}

// 対象選択述語が paper close と「broker 実報告 0」
// 行を機械的に除外すること。これが欠けると -apply が正しいゼロを推定値で上書きする。
func TestBackfillTargetSQL_ExcludesPaperAndBrokerReportedZero(t *testing.T) {
	for _, want := range []string{
		"fee_jpy = 0",
		"fee_estimated = false",
		"sc.mode = 'live_config'", // paper close (zero costs) を除外
		"p.entry_fee_jpy IS NULL", // 0008 配線後の broker 実報告 0 を除外
		"JOIN strategy_configs sc",
		"JOIN positions p",
	} {
		if !strings.Contains(backfillTargetSQL, want) {
			t.Errorf("backfillTargetSQL must contain %q", want)
		}
	}
}
