package main

import (
	"math"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// jstT は JST 固定タイムゾーンで time.Time を作るテストヘルパー。
// 2026-06-05=金, 06=土, 07=日, 08=月, 10=水 (実カレンダー確認済み)。
func jstT(year int, month time.Month, day, hour, min int) time.Time {
	return time.Date(year, month, day, hour, min, 0, 0, jstZone)
}

func TestIsFXMarketClosedJST(t *testing.T) {
	cases := []struct {
		name   string
		t      time.Time
		closed bool
	}{
		{"金曜 23:00 JST は開場", jstT(2026, time.June, 5, 23, 0), false},
		{"土曜 05:59 JST はまだ開場 (夏時間 NY close 前)", jstT(2026, time.June, 6, 5, 59), false},
		{"土曜 06:00 JST から閉場 (夏時間 NY close = 21:00 UTC)", jstT(2026, time.June, 6, 6, 0), true},
		{"土曜 07:00 JST も閉場", jstT(2026, time.June, 6, 7, 0), true},
		{"土曜 23:00 JST は閉場", jstT(2026, time.June, 6, 23, 0), true},
		{"日曜 00:00 JST は閉場", jstT(2026, time.June, 7, 0, 0), true},
		{"日曜 23:59 JST は閉場", jstT(2026, time.June, 7, 23, 59), true},
		{"月曜 04:59 JST はまだ閉場 (オープン前)", jstT(2026, time.June, 8, 4, 59), true},
		{"月曜 05:00 JST ちょうどから開場", jstT(2026, time.June, 8, 5, 0), false},
		{"水曜 12:00 JST は開場", jstT(2026, time.June, 10, 12, 0), false},
		// DB の created_at は UTC 保存。
		// UTC で渡しても JST に変換して判定されること:
		// 金曜 22:30 UTC = 土曜 07:30 JST → 閉場。
		{"UTC 入力も JST 変換で判定 (金曜 22:30 UTC = 土曜 07:30 JST)",
			time.Date(2026, time.June, 5, 22, 30, 0, 0, time.UTC), true},
		// 日曜 21:00 UTC = 月曜 06:00 JST → 開場。
		{"UTC 入力 (日曜 21:00 UTC = 月曜 06:00 JST) は開場",
			time.Date(2026, time.June, 7, 21, 0, 0, 0, time.UTC), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsFXMarketClosedJST(tc.t); got != tc.closed {
				t.Errorf("IsFXMarketClosedJST(%v) = %v, want %v", tc.t, got, tc.closed)
			}
		})
	}
}

func TestMedianByHourJST(t *testing.T) {
	const tol = 1e-9
	cases := []struct {
		name       string
		samples    []Sample
		wantHour   int
		wantMedian float64
		wantCount  int
	}{
		{
			name: "奇数個は中央の値",
			samples: []Sample{
				{At: jstT(2026, time.June, 10, 9, 0), SpreadPips: 0.5},
				{At: jstT(2026, time.June, 10, 9, 10), SpreadPips: 0.9},
				{At: jstT(2026, time.June, 10, 9, 20), SpreadPips: 0.6},
			},
			wantHour: 9, wantMedian: 0.6, wantCount: 3,
		},
		{
			name: "偶数個は中央 2 値の平均",
			samples: []Sample{
				{At: jstT(2026, time.June, 10, 5, 0), SpreadPips: 9.0},
				{At: jstT(2026, time.June, 10, 5, 30), SpreadPips: 11.0},
			},
			wantHour: 5, wantMedian: 10.0, wantCount: 2,
		},
		{
			name: "閉場サンプル (土曜午後 JST) は除外される",
			samples: []Sample{
				{At: jstT(2026, time.June, 10, 14, 0), SpreadPips: 0.5},
				// 土曜 14:00 JST: 閉場汚染 (週末 10-11pips の実例) — 無視されること
				{At: jstT(2026, time.June, 6, 14, 0), SpreadPips: 10.5},
				{At: jstT(2026, time.June, 7, 14, 0), SpreadPips: 11.0},
			},
			wantHour: 14, wantMedian: 0.5, wantCount: 1,
		},
		{
			name: "spread<=0 は除外される",
			samples: []Sample{
				{At: jstT(2026, time.June, 10, 9, 0), SpreadPips: 0.0},
				{At: jstT(2026, time.June, 10, 9, 5), SpreadPips: -1.0},
				{At: jstT(2026, time.June, 10, 9, 10), SpreadPips: 0.7},
			},
			wantHour: 9, wantMedian: 0.7, wantCount: 1,
		},
		{
			name: "UTC 保存の created_at は JST の時間帯に振り分けられる",
			samples: []Sample{
				// 09:30 UTC = 18:30 JST → hour 18 に入ること
				{At: time.Date(2026, time.June, 10, 9, 30, 0, 0, time.UTC), SpreadPips: 0.4},
			},
			wantHour: 18, wantMedian: 0.4, wantCount: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			byHour, counts := MedianByHourJST(tc.samples)
			if counts[tc.wantHour] != tc.wantCount {
				t.Errorf("counts[%d] = %d, want %d", tc.wantHour, counts[tc.wantHour], tc.wantCount)
			}
			if math.Abs(byHour[tc.wantHour]-tc.wantMedian) > tol {
				t.Errorf("byHour[%d] = %v, want %v", tc.wantHour, byHour[tc.wantHour], tc.wantMedian)
			}
			// 指定時間以外のバケツは空のままであること
			for h := 0; h < 24; h++ {
				if h == tc.wantHour {
					continue
				}
				if counts[h] != 0 {
					t.Errorf("counts[%d] = %d, want 0", h, counts[h])
				}
			}
		})
	}
}

func TestOverallMedian(t *testing.T) {
	const tol = 1e-9
	samples := []Sample{
		{At: jstT(2026, time.June, 10, 9, 0), SpreadPips: 0.4},
		{At: jstT(2026, time.June, 10, 15, 0), SpreadPips: 0.6},
		{At: jstT(2026, time.June, 10, 22, 0), SpreadPips: 0.8},
		// 閉場 (日曜) と非正値はフォールバック中央値からも除外されること
		{At: jstT(2026, time.June, 7, 12, 0), SpreadPips: 11.0},
		{At: jstT(2026, time.June, 10, 9, 5), SpreadPips: 0.0},
	}
	got, n := OverallMedian(samples)
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
	if math.Abs(got-0.6) > tol {
		t.Errorf("median = %v, want 0.6", got)
	}

	t.Run("空入力は 0 件", func(t *testing.T) {
		got, n := OverallMedian(nil)
		if n != 0 || got != 0 {
			t.Errorf("OverallMedian(nil) = (%v, %d), want (0, 0)", got, n)
		}
	})
}

func TestRenderSpreadYAML(t *testing.T) {
	t.Run("基本形: header + fallback + by_hour_jst (min-count 未満の hour は省略)", func(t *testing.T) {
		var hours [24]float64
		var counts [24]int
		hours[0], counts[0] = 0.6, 12
		hours[5], counts[5] = 9.8, 7
		hours[9], counts[9] = 0.5, 3 // minCount=5 未満 → 省略されること

		got := RenderSpreadYAML(
			map[string][24]float64{"USD_JPY": hours},
			map[string]float64{"USD_JPY": 0.5},
			5,
			map[string][24]int{"USD_JPY": counts},
		)
		want := "# generated by spread-calibrate; weekend/closed-market samples excluded\n" +
			"spreads:\n" +
			"  USD_JPY:\n" +
			"    fallback_pips: 0.5\n" +
			"    by_hour_jst:\n" +
			"      0: 0.6\n" +
			"      5: 9.8\n"
		if got != want {
			t.Errorf("RenderSpreadYAML mismatch:\ngot:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("複数 symbol はアルファベット順で安定出力", func(t *testing.T) {
		var h1, h2 [24]float64
		var c1, c2 [24]int
		h1[10], c1[10] = 0.7, 6
		h2[10], c2[10] = 1.2, 6

		got := RenderSpreadYAML(
			map[string][24]float64{"USD_JPY": h1, "EUR_JPY": h2},
			map[string]float64{"USD_JPY": 0.5, "EUR_JPY": 0.9},
			5,
			map[string][24]int{"USD_JPY": c1, "EUR_JPY": c2},
		)
		iEUR := strings.Index(got, "EUR_JPY:")
		iUSD := strings.Index(got, "USD_JPY:")
		if iEUR < 0 || iUSD < 0 || iEUR > iUSD {
			t.Errorf("symbols not sorted alphabetically:\n%s", got)
		}
	})

	t.Run("全 hour が min-count 未満なら by_hour_jst キーごと省略", func(t *testing.T) {
		var hours [24]float64
		var counts [24]int
		hours[3], counts[3] = 0.6, 2

		got := RenderSpreadYAML(
			map[string][24]float64{"EUR_USD": hours},
			map[string]float64{"EUR_USD": 0.4},
			5,
			map[string][24]int{"EUR_USD": counts},
		)
		if strings.Contains(got, "by_hour_jst") {
			t.Errorf("by_hour_jst should be omitted when no hour qualifies:\n%s", got)
		}
		if !strings.Contains(got, "fallback_pips: 0.4\n") {
			t.Errorf("fallback_pips missing:\n%s", got)
		}
	})

	t.Run("float は 0.001 pips に丸めて余計な桁を出さない", func(t *testing.T) {
		var hours [24]float64
		var counts [24]int
		hours[8], counts[8] = 0.6000000000000001, 9 // 浮動小数点誤差 → "0.6"

		got := RenderSpreadYAML(
			map[string][24]float64{"GBP_JPY": hours},
			map[string]float64{"GBP_JPY": 1.0},
			5,
			map[string][24]int{"GBP_JPY": counts},
		)
		if !strings.Contains(got, "      8: 0.6\n") {
			t.Errorf("expected rounded '8: 0.6' in:\n%s", got)
		}
		if !strings.Contains(got, "fallback_pips: 1\n") {
			t.Errorf("expected 'fallback_pips: 1' in:\n%s", got)
		}
	})
}

// TestRenderSpreadYAML_RoundTrip: 出力が valid YAML であること
// (sibling の backtest loader は yaml パーサで読む — header コメントは無害)。
func TestRenderSpreadYAML_RoundTrip(t *testing.T) {
	const tol = 1e-9
	var hours [24]float64
	var counts [24]int
	hours[0], counts[0] = 0.6, 12
	hours[5], counts[5] = 9.8, 7

	out := RenderSpreadYAML(
		map[string][24]float64{"USD_JPY": hours},
		map[string]float64{"USD_JPY": 0.5},
		5,
		map[string][24]int{"USD_JPY": counts},
	)

	var doc struct {
		Spreads map[string]struct {
			FallbackPips float64         `yaml:"fallback_pips"`
			ByHourJST    map[int]float64 `yaml:"by_hour_jst"`
		} `yaml:"spreads"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output is not valid YAML: %v\n%s", err, out)
	}
	usd, ok := doc.Spreads["USD_JPY"]
	if !ok {
		t.Fatalf("USD_JPY missing in parsed YAML:\n%s", out)
	}
	if math.Abs(usd.FallbackPips-0.5) > tol {
		t.Errorf("fallback_pips = %v, want 0.5", usd.FallbackPips)
	}
	if math.Abs(usd.ByHourJST[5]-9.8) > tol {
		t.Errorf("by_hour_jst[5] = %v, want 9.8", usd.ByHourJST[5])
	}
	if len(usd.ByHourJST) != 2 {
		t.Errorf("by_hour_jst size = %d, want 2 (hours 0 and 5)", len(usd.ByHourJST))
	}
}

// TestExtractSpreadPips: market_summaries.raw_json (market.MarketSummary の
// serialize 結果) から current_rate.spread_pips だけを最小 struct で取り出す。
func TestExtractSpreadPips(t *testing.T) {
	const tol = 1e-9
	t.Run("実 raw_json の形 (抜粋) から取り出せる", func(t *testing.T) {
		raw := []byte(`{
			"symbol": "USD_JPY",
			"time": "2026-06-10T09:00:00Z",
			"current_rate": {"bid": 144.123, "ask": 144.128, "spread_pips": 0.5},
			"summary_15m": {"high": 144.2, "low": 144.0, "range_pips": 20, "realized_volatility": 0.1, "trend_direction": "flat", "range_position_pct": 0.5}
		}`)
		got, err := extractSpreadPips(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if math.Abs(got-0.5) > tol {
			t.Errorf("spread = %v, want 0.5", got)
		}
	})
	t.Run("壊れた JSON はエラー", func(t *testing.T) {
		if _, err := extractSpreadPips([]byte(`{not json`)); err == nil {
			t.Error("expected error for malformed JSON")
		}
	})
}

// run() の入力バリデーション (DB に触る前に落ちる経路のみテスト)。
func TestRun_RejectsBadSince(t *testing.T) {
	err := run(cliFlags{since: "not-a-time", symbols: "USD_JPY", minCount: 5})
	if err == nil || !strings.Contains(err.Error(), "-since") {
		t.Fatalf("expected -since parse error, got %v", err)
	}
}

func TestRun_RejectsEmptySymbols(t *testing.T) {
	err := run(cliFlags{since: "2026-05-01T00:00:00Z", symbols: " , ,", minCount: 5})
	if err == nil || !strings.Contains(err.Error(), "-symbols") {
		t.Fatalf("expected -symbols error, got %v", err)
	}
}

func TestRun_RejectsBadMinCount(t *testing.T) {
	err := run(cliFlags{since: "2026-05-01T00:00:00Z", symbols: "USD_JPY", minCount: 0})
	if err == nil || !strings.Contains(err.Error(), "-min-count") {
		t.Fatalf("expected -min-count error, got %v", err)
	}
}

func TestResolveSymbolsCSV(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // join した期待値 (空 = nil)
	}{
		{"カンマ区切り", "USD_JPY,EUR_JPY", "USD_JPY|EUR_JPY"},
		{"空白 trim", " USD_JPY , EUR_JPY ", "USD_JPY|EUR_JPY"},
		{"空要素 skip", "USD_JPY,,EUR_JPY,", "USD_JPY|EUR_JPY"},
		{"全部空なら nil", " , ,", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(splitSymbolsCSV(tc.in), "|")
			if got != tc.want {
				t.Errorf("splitSymbolsCSV(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
