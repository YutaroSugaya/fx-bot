package backtest

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fx-bot/backend/internal/domain/market"
)

// スワップモデル。
// per-symbol/side 円/晩テーブル + 21:00 UTC 跨ぎカウント + 水曜 3 倍。

// 2026-06-08 = 月曜 / 2026-06-10 = 水曜 (UTC)。
var (
	swapMonday    = time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)
	swapWednesday = time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
)

func TestSwapNightsWeighted(t *testing.T) {
	at := func(base time.Time, hour, min int) time.Time {
		return base.Add(time.Duration(hour)*time.Hour + time.Duration(min)*time.Minute)
	}
	cases := []struct {
		name     string
		openedAt time.Time
		closedAt time.Time
		want     int
	}{
		{
			name:     "同日 21:00 UTC 未到達 → 0 泊",
			openedAt: at(swapMonday, 10, 0),
			closedAt: at(swapMonday, 15, 0),
			want:     0,
		},
		{
			name:     "21:00 UTC を 1 回跨ぐ → 1 泊",
			openedAt: at(swapMonday, 20, 0),
			closedAt: at(swapMonday, 22, 0),
			want:     1,
		},
		{
			name:     "ちょうど 21:00 UTC に open → その瞬間は跨ぎに数えない (strict)",
			openedAt: at(swapMonday, 21, 0),
			closedAt: at(swapMonday.AddDate(0, 0, 1), 10, 0),
			want:     0,
		},
		{
			name:     "ちょうど 21:00 UTC に close → 跨ぎに数えない (strict)",
			openedAt: at(swapMonday, 20, 0),
			closedAt: at(swapMonday, 21, 0),
			want:     0,
		},
		{
			name:     "水曜 21:00 UTC 跨ぎ → 3 倍 (週末ロール)",
			openedAt: at(swapWednesday, 20, 0),
			closedAt: at(swapWednesday, 22, 0),
			want:     3,
		},
		{
			name:     "複数日: 月 10:00 → 金 10:00 = 月+火+水×3+木 = 6 泊",
			openedAt: at(swapMonday, 10, 0),
			closedAt: at(swapMonday.AddDate(0, 0, 4), 10, 0),
			want:     6,
		},
		{
			name:     "closedAt <= openedAt → 0 泊",
			openedAt: at(swapMonday, 22, 0),
			closedAt: at(swapMonday, 22, 0),
			want:     0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SwapNightsWeighted(tc.openedAt, tc.closedAt); got != tc.want {
				t.Errorf("SwapNightsWeighted(%v, %v) = %d, want %d",
					tc.openedAt, tc.closedAt, got, tc.want)
			}
		})
	}
}

func TestSwapTable_SwapJPY(t *testing.T) {
	table := SwapTable{
		"USD_JPY": {"BUY": 15.0, "SELL": -18.0},
	}
	// 月 20:00 → 月 22:00 = 1 泊 (月曜跨ぎ)。
	opened := swapMonday.Add(20 * time.Hour)
	closed := swapMonday.Add(22 * time.Hour)

	cases := []struct {
		name   string
		table  SwapTable
		symbol string
		side   string
		qty    int
		want   float64
	}{
		{"BUY 1,000 通貨 = テーブル値そのまま", table, "USD_JPY", "BUY", 1000, 15.0},
		{"SELL は符号付き (負 = 支払い)", table, "USD_JPY", "SELL", 1000, -18.0},
		{"qty スケール: 2,000 通貨 = 2 倍", table, "USD_JPY", "BUY", 2000, 30.0},
		{"テーブルに無い symbol → 0", table, "EUR_JPY", "BUY", 1000, 0},
		{"テーブルに無い side → 0", SwapTable{"USD_JPY": {"BUY": 15.0}}, "USD_JPY", "SELL", 1000, 0},
		{"nil テーブル → 0 (無効化 = back-compat)", nil, "USD_JPY", "BUY", 1000, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.table.SwapJPY(tc.symbol, tc.side, opened, closed, tc.qty)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("SwapJPY = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("水曜跨ぎ × 2,000 通貨 = 3 × 2 倍", func(t *testing.T) {
		got := table.SwapJPY("USD_JPY", "BUY",
			swapWednesday.Add(20*time.Hour), swapWednesday.Add(22*time.Hour), 2000)
		want := 15.0 * 3 * 2
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("SwapJPY = %v, want %v", got, want)
		}
	})
}

// エンジン統合: makeTrade が swap を ProfitLossJPY に加算し、SwapJPY に透明記録する。
// 月 20:30 UTC entry → MaxHold 60 分で 21:31 UTC close = 月曜 21:00 UTC を 1 回跨ぐ。
// flat candle なので gross 0 円、swap -15 円/晩 × qty 100/1000 = -1.5 円が net。
func TestEngine_Replay_SwapApplied(t *testing.T) {
	ctx := context.Background()
	start := swapMonday.Add(20*time.Hour + 30*time.Minute) // 月 20:30 UTC
	cfg := buyConfig(start, 30 /*TP*/, 20 /*SL*/, 60 /*MaxHold*/)
	candles := flatCandles(70, 150.00, start)

	e := NewEngine(EngineConfig{
		Symbol: "USD_JPY", StrategyConfig: cfg,
		Costs: CostModel{SwapTable: SwapTable{"USD_JPY": {"BUY": -15.0}}},
	})
	e.Strategies.Register(alwaysBuyStrategy{})
	res, err := e.Replay(ctx, candles)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(res.Trades) != 1 {
		t.Fatalf("expected 1 trade, got %d: %+v", len(res.Trades), res.Trades)
	}
	tr := res.Trades[0]
	wantSwap := -15.0 * 100.0 / 1000.0 // 1 泊 × -15 円 × qty 100/1,000
	if math.Abs(tr.SwapJPY-wantSwap) > 1e-9 {
		t.Errorf("SwapJPY: got %v want %v", tr.SwapJPY, wantSwap)
	}
	if math.Abs(tr.ProfitLossJPY-wantSwap) > 1e-9 {
		t.Errorf("ProfitLossJPY (gross 0 + swap): got %v want %v", tr.ProfitLossJPY, wantSwap)
	}
}

// nil テーブル = 挙動完全不変 (back-compat)。既存スタイルの TP hit トレードを固定し、
// SwapTable 無し / nil 明示で PnL が同一であることをピン留めする。
func TestEngine_Replay_NilSwapTable_NoBehaviorChange(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	cfg := buyConfig(start, 30 /*TP*/, 20 /*SL*/, 60 /*MaxHold*/)
	candles := []market.Candle{
		{Symbol: "USD_JPY", Interval: time.Minute, OpenTime: start.Add(0 * time.Minute), Open: 150.00, High: 150.05, Low: 149.98, Close: 150.02},
		{Symbol: "USD_JPY", Interval: time.Minute, OpenTime: start.Add(1 * time.Minute), Open: 150.02, High: 150.40, Low: 150.00, Close: 150.30},
	}

	replay := func(costs CostModel) Trade {
		e := NewEngine(EngineConfig{Symbol: "USD_JPY", StrategyConfig: cfg, Costs: costs})
		e.Strategies.Register(alwaysBuyStrategy{})
		res, err := e.Replay(ctx, candles)
		if err != nil {
			t.Fatalf("Replay: %v", err)
		}
		if len(res.Trades) != 1 {
			t.Fatalf("expected 1 trade, got %d", len(res.Trades))
		}
		return res.Trades[0]
	}

	base := replay(CostModel{FeeJPYPerTrade: 10.0})
	withNil := replay(CostModel{FeeJPYPerTrade: 10.0, SwapTable: nil})
	if math.Abs(base.ProfitLossJPY-withNil.ProfitLossJPY) > 1e-9 {
		t.Errorf("nil SwapTable changed PnL: base %v vs nil-table %v",
			base.ProfitLossJPY, withNil.ProfitLossJPY)
	}
	if withNil.SwapJPY != 0 {
		t.Errorf("SwapJPY with nil table: got %v want 0", withNil.SwapJPY)
	}
	// 既存スタイルのピン: TP +30 pips × 100 通貨 = 30 円 gross − fee 10 円 = 20 円。
	if math.Abs(base.ProfitLossJPY-20.0) > 0.01 {
		t.Errorf("pinned PnL drifted: got %v want 20.0", base.ProfitLossJPY)
	}
}

func TestLoadSwapTable(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "swap.yaml")
		body := "swap_table:\n  USD_JPY: { BUY: 15.0, SELL: -18.0 }\n  EUR_JPY: { BUY: 10.0, SELL: -12.5 }\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		table, err := LoadSwapTable(path)
		if err != nil {
			t.Fatalf("LoadSwapTable: %v", err)
		}
		if got := table["USD_JPY"]["SELL"]; got != -18.0 {
			t.Errorf("USD_JPY SELL = %v, want -18.0", got)
		}
		if got := table["EUR_JPY"]["BUY"]; got != 10.0 {
			t.Errorf("EUR_JPY BUY = %v, want 10.0", got)
		}
	})
	t.Run("malformed yaml", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "swap.yaml")
		if err := os.WriteFile(path, []byte("swap_table: [not, a, map"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSwapTable(path); err == nil {
			t.Error("expected parse error for malformed yaml, got nil")
		}
	})
	t.Run("empty table rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "swap.yaml")
		if err := os.WriteFile(path, []byte("other_key: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSwapTable(path); err == nil {
			t.Error("expected error for missing swap_table key, got nil")
		}
	})
	t.Run("missing file", func(t *testing.T) {
		if _, err := LoadSwapTable(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
			t.Error("expected error for missing file, got nil")
		}
	})
}

// 週末 (土日 UTC) の 21:00 跨ぎはロールオーバーが発生しない
// (週末分は水曜 3 倍が既にカバー)。数えると Fri→Mon が 3 泊 (正 1 泊)、
// 1 週間で 9 泊 (正 7 泊) と ~29% 過大計上になる。
func TestSwapNightsWeighted_WeekendCrossingsNotCounted(t *testing.T) {
	// 2026-06-12 = Friday。Fri 20:00 UTC → Mon 12:00 UTC は Fri 21:00 の 1 跨ぎのみ。
	opened := time.Date(2026, 6, 12, 20, 0, 0, 0, time.UTC)
	closed := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	if got := SwapNightsWeighted(opened, closed); got != 1 {
		t.Errorf("Fri→Mon nights: got %d want 1 (Sat/Sun 跨ぎはロール無し)", got)
	}
	// Mon 12:00 → 翌 Mon 12:00 の 1 週間 = Mon,Tue,Wed×3,Thu,Fri = 7 泊。
	opened = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	closed = time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	if got := SwapNightsWeighted(opened, closed); got != 7 {
		t.Errorf("full week nights: got %d want 7 (broker 規約と一致)", got)
	}
}
