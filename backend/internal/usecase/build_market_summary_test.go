package usecase

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fx-bot/backend/internal/adapter/artifact"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
)

func mkCandle(o, h, l, c float64, t time.Time) market.Candle {
	return market.Candle{
		Symbol:   "USD_JPY",
		Interval: time.Minute,
		OpenTime: t,
		Open:     o,
		High:     h,
		Low:      l,
		Close:    c,
	}
}

func testHardLimits() *config.HardLimits {
	return &config.HardLimits{
		AllowedSymbols:         []string{"USD_JPY"},
		Quantity:               config.IntRange{Min: 100, Max: 100},
		TakeProfitPips:         config.FloatRange{Min: 1.0, Max: 8.0},
		StopLossPips:           config.FloatRange{Min: 1.0, Max: 5.0},
		MaxHoldMinutes:         config.IntRange{Min: 1, Max: 30},
		MaxTradesInThisWindow:  config.IntRange{Min: 0, Max: 5},
		MaxLossInThisWindowJPY: config.IntRange{Min: 0, Max: 500},
		MaxSpreadPips:          config.FloatRange{Min: 0.1, Max: 0.5},
		ConfigTTLMinutes:       config.IntRange{Min: 30, Max: 90},
	}
}

func TestBuildMarketSummary_PopulatesAllSections(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	candles := []market.Candle{
		mkCandle(150.10, 150.30, 150.05, 150.20, now.Add(-5*time.Minute)),
		mkCandle(150.20, 150.35, 150.15, 150.30, now.Add(-4*time.Minute)),
		mkCandle(150.30, 150.40, 150.25, 150.35, now.Add(-3*time.Minute)),
	}
	in := BuildMarketSummaryInput{
		Symbol:     "USD_JPY",
		Mode:       config.ModePaperConfig,
		Now:        now,
		HardLimits: testHardLimits(),
		Candles1m:  candles,
		Ticker: &market.Ticker{
			Symbol: "USD_JPY", Bid: 150.40, Ask: 150.403, Timestamp: now,
		},
		BotState: market.BotState{
			DailyPnLJPY:           120,
			ConsecutiveLosses:     1,
			TradesToday:           4,
			TradesInCurrentWindow: 1,
		},
		AllowedStrategies: []string{"momentum_pullback", "no_trade"},
	}
	s := BuildMarketSummary(in)

	if s.Symbol != "USD_JPY" {
		t.Errorf("symbol: %s", s.Symbol)
	}
	if s.BotState.Mode != string(config.ModePaperConfig) {
		t.Errorf("mode: %s", s.BotState.Mode)
	}
	if s.CurrentRate.SpreadPips < 0.29 || s.CurrentRate.SpreadPips > 0.31 {
		t.Errorf("spread pips: %v", s.CurrentRate.SpreadPips)
	}
	if s.Summary1h.NumCandles != 3 {
		t.Errorf("Summary1h NumCandles: %d", s.Summary1h.NumCandles)
	}
	if s.HardLimits == nil || s.HardLimits.Quantity.Max != 100 {
		t.Errorf("hard limits: %+v", s.HardLimits)
	}
	if len(s.AllowedStrategies) != 2 {
		t.Errorf("allowed strategies: %v", s.AllowedStrategies)
	}
}

// TestBuildMarketSummary_AllFourWindowsPopulated は 15m / 1h / 6h / 24h の
// 4 つの WindowSummary がすべて埋まることを担保する。BuildMarketSummary を
// loop ベースにリファクタする際の regression guard。
func TestBuildMarketSummary_AllFourWindowsPopulated(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	// 24h ぶん候補を作って 4 ウィンドウ全てに候補が入るようにする
	var candles []market.Candle
	for i := 0; i < 24*60; i++ {
		ts := now.Add(-time.Duration(24*60-i) * time.Minute)
		c := 150.0 + float64(i%30)*0.01
		candles = append(candles, mkCandle(c, c+0.05, c-0.05, c+0.02, ts))
	}
	// spread sample も 24h 分入れる
	var samples []market.SpreadSample
	for i := 0; i < 24*60; i++ {
		samples = append(samples, market.SpreadSample{
			Time: now.Add(-time.Duration(24*60-i) * time.Minute),
			Pips: 0.3,
		})
	}
	s := BuildMarketSummary(BuildMarketSummaryInput{
		Symbol: "USD_JPY", Mode: config.ModePaperConfig, Now: now,
		HardLimits:    testHardLimits(),
		Candles1m:     candles,
		SpreadSamples: samples,
		Ticker:        &market.Ticker{Symbol: "USD_JPY", Bid: 150.40, Ask: 150.42, Timestamp: now},
	})
	windows := map[string]market.WindowSummary{
		"15m": s.Summary15m,
		"1h":  s.Summary1h,
		"6h":  s.Summary6h,
		"24h": s.Summary24h,
	}
	for name, w := range windows {
		if w.NumCandles == 0 {
			t.Errorf("%s: NumCandles is 0", name)
		}
		if w.High == 0 || w.Low == 0 {
			t.Errorf("%s: High/Low not populated: %+v", name, w)
		}
		if w.AvgSpreadPips == 0 {
			t.Errorf("%s: AvgSpreadPips not populated", name)
		}
	}
	// ウィンドウサイズが大きいほど NumCandles が大きい (= 単調増加) はず
	if !(s.Summary15m.NumCandles < s.Summary1h.NumCandles &&
		s.Summary1h.NumCandles < s.Summary6h.NumCandles &&
		s.Summary6h.NumCandles < s.Summary24h.NumCandles) {
		t.Errorf("expected NumCandles strictly increasing 15m<1h<6h<24h, got %d/%d/%d/%d",
			s.Summary15m.NumCandles, s.Summary1h.NumCandles,
			s.Summary6h.NumCandles, s.Summary24h.NumCandles)
	}
}

// TestBuildMarketSummary_RangePosition — 現在値 mid が window の
// [low,high] のどこにいるかを range_position_pct に入れる。
func TestBuildMarketSummary_RangePosition(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	// window low=150.00, high=150.40
	candles := []market.Candle{
		mkCandle(150.20, 150.20, 150.00, 150.10, now.Add(-3*time.Minute)),
		mkCandle(150.10, 150.40, 150.10, 150.30, now.Add(-2*time.Minute)),
		mkCandle(150.30, 150.30, 150.20, 150.25, now.Add(-1*time.Minute)),
	}
	s := BuildMarketSummary(BuildMarketSummaryInput{
		Symbol: "USD_JPY", Mode: config.ModePaperConfig, Now: now,
		HardLimits: testHardLimits(), Candles1m: candles,
		// mid = 150.10 → (150.10-150.00)/(150.40-150.00) = 0.25 (安値寄り)
		Ticker: &market.Ticker{Symbol: "USD_JPY", Bid: 150.10, Ask: 150.10, Timestamp: now},
	})
	if got := s.Summary1h.RangePositionPct; got < 0.24 || got > 0.26 {
		t.Errorf("Summary1h.RangePositionPct expected ~0.25, got %v", got)
	}
	if s.Summary1h.ATRPips <= 0 {
		t.Errorf("Summary1h.ATRPips expected >0, got %v", s.Summary1h.ATRPips)
	}
}

// TestBuildMarketSummary_FiveMinuteWindow — The playbook
// references "ATR(5m)" but the summary only ever exposed 1m-derived ATR (mean TR of 1m candles
// over a window), an order of magnitude smaller → the ATR≥12-15pips gates never fired (permanent
// no_trade). Candles5m was passed into BuildMarketSummary but silently dropped. summary_5m must now
// be built from the 5m stream so "ATR(5m)" has a real, correctly-scaled value.
func TestBuildMarketSummary_FiveMinuteWindow(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	// 5 five-minute candles, each with True Range = 0.05 (= 5 pips on USDJPY, pip=0.01).
	var c5 []market.Candle
	for i := 0; i < 5; i++ {
		ts := now.Add(-time.Duration((5-i)*5) * time.Minute)
		base := 150.00
		c5 = append(c5, market.Candle{
			Symbol: "USD_JPY", Interval: 5 * time.Minute, OpenTime: ts,
			Open: base, High: base + 0.05, Low: base, Close: base + 0.03,
		})
	}
	s := BuildMarketSummary(BuildMarketSummaryInput{
		Symbol: "USD_JPY", Mode: config.ModePaperConfig, Now: now,
		HardLimits: testHardLimits(),
		Candles5m:  c5,
		Ticker:     &market.Ticker{Symbol: "USD_JPY", Bid: 150.02, Ask: 150.03, Timestamp: now},
	})
	if s.Summary5m.NumCandles != 5 {
		t.Errorf("Summary5m.NumCandles = %d, want 5", s.Summary5m.NumCandles)
	}
	// Each 5m candle TR = high-low = 0.05 → ATR = 0.05/0.01 = 5 pips.
	if got := s.Summary5m.ATRPips; got < 4.9 || got > 5.1 {
		t.Errorf("Summary5m.ATRPips = %v, want ~5 (true 5m-candle ATR in pips)", got)
	}
	if s.Summary5m.High != 150.05 || s.Summary5m.Low != 150.00 {
		t.Errorf("Summary5m High/Low = %v/%v, want 150.05/150.00", s.Summary5m.High, s.Summary5m.Low)
	}
}

// Candles5m absent → summary_5m stays a safe zero value (no panic, NumCandles 0).
func TestBuildMarketSummary_FiveMinuteWindow_AbsentIsSafe(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	s := BuildMarketSummary(BuildMarketSummaryInput{
		Symbol: "USD_JPY", Mode: config.ModePaperConfig, Now: now,
		HardLimits: testHardLimits(),
		Candles1m:  []market.Candle{mkCandle(150.10, 150.30, 150.05, 150.20, now.Add(-time.Minute))},
		Ticker:     &market.Ticker{Symbol: "USD_JPY", Bid: 150.20, Ask: 150.21, Timestamp: now},
	})
	if s.Summary5m.NumCandles != 0 {
		t.Errorf("Summary5m.NumCandles = %d, want 0 when no 5m candles", s.Summary5m.NumCandles)
	}
}

func TestBuildMarketSummary_NoCandles_StillReturnsSafe(t *testing.T) {
	now := time.Now()
	s := BuildMarketSummary(BuildMarketSummaryInput{
		Symbol: "USD_JPY", Mode: config.ModePaperConfig, Now: now,
		HardLimits: testHardLimits(),
	})
	if s.Symbol != "USD_JPY" || s.BotState.Mode != string(config.ModePaperConfig) {
		t.Errorf("base fields: %+v", s)
	}
}

func TestPublishMarketSummary_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ai_input", "latest_summary.json")
	store := artifact.NewFileMarketSummaryStore(path)
	s := BuildMarketSummary(BuildMarketSummaryInput{
		Symbol: "USD_JPY", Mode: config.ModePaperConfig, Now: time.Now(),
		HardLimits: testHardLimits(),
	})
	if err := PublishMarketSummary(context.Background(), store, s); err != nil {
		t.Fatalf("PublishMarketSummary: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["symbol"] != "USD_JPY" {
		t.Errorf("got: %v", got["symbol"])
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "summary-") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp: %s", e.Name())
		}
	}
}

func TestBuildMarketSummary_NoSecretLeakage(t *testing.T) {
	t.Setenv("GMO_API_KEY", "should-not-appear")
	t.Setenv("GMO_API_SECRET", "should-not-appear-either")

	now := time.Now()
	s := BuildMarketSummary(BuildMarketSummaryInput{
		Symbol: "USD_JPY", Mode: config.ModePaperConfig, Now: now,
		HardLimits: testHardLimits(),
		Ticker:     &market.Ticker{Symbol: "USD_JPY", Bid: 150.10, Ask: 150.13, Timestamp: now},
	})
	body, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, needle := range []string{"GMO_API_KEY", "GMO_API_SECRET", "should-not-appear"} {
		if strings.Contains(string(body), needle) {
			t.Errorf("summary JSON leaked %q: %s", needle, body)
		}
	}
}

// The advertised hard_limits.max_spread_pips must match the gate that actually
// governs the path the summary feeds: the LLM decision loop defers at
// llm_decision.max_spread_pips (e.g. 3.0), but the summary advertised the strategy
// hard-limit ceiling (1.5) — an LLM shown both numbers tends to treat 1.5 as a
// "hard cap", inviting spurious no_trades in the 1.5-3.0 band.
// A positive override replaces the advertised value; 0 keeps the hard-limits value
// (back-compat for paths without their own spread gate).
func TestBuildMarketSummary_MaxSpreadPipsOverride(t *testing.T) {
	in := BuildMarketSummaryInput{
		Symbol:     "USD_JPY",
		Mode:       config.ModeLiveConfig,
		Now:        time.Now(),
		HardLimits: testHardLimits(), // MaxSpreadPips.Max = 0.5
	}

	in.MaxSpreadPipsOverride = 3.0
	if got := BuildMarketSummary(in).HardLimits.MaxSpreadPips; got != 3.0 {
		t.Errorf("override must win: max_spread_pips = %v, want 3.0", got)
	}

	in.MaxSpreadPipsOverride = 0
	if got := BuildMarketSummary(in).HardLimits.MaxSpreadPips; got != 0.5 {
		t.Errorf("no override must keep hard-limits value: max_spread_pips = %v, want 0.5", got)
	}
}
