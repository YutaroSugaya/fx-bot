package main

import (
	"strings"
	"testing"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/strategy"
)

// walk-forward スイープ基盤の純粋部分のテスト。
// グリッド展開 / パラメータ適用 / 隣接プラトー / オフライン config 複製 / 3 分割検証。

func TestExpandGrid_CartesianProductAndTrialCount(t *testing.T) {
	grid := []GridDim{
		{Key: "ratchet_arm_pips", Values: []float64{12, 16}},
		{Key: "ratchet_giveback_pips", Values: []float64{6, 8, 10}},
	}
	combos, err := ExpandGrid(grid)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if len(combos) != 6 {
		t.Fatalf("N_trials: got %d want 6 (2×3)", len(combos))
	}
	// 先頭 combo は両次元とも index 0 で、defaults からの差分が反映される。
	c0 := combos[0]
	if c0.Params.RatchetArmPips != 12 || c0.Params.RatchetGivebackPips != 6 {
		t.Errorf("combo0 params: %+v", c0.Params)
	}
	// グリッドに無いパラメータは ma_pullback デフォルトのまま。
	def := strategy.DefaultMAPullbackParams()
	if c0.Params.ZoneATR != def.ZoneATR || c0.Params.MaxHoldMinutes != def.MaxHoldMinutes {
		t.Errorf("unswept params must stay default: %+v", c0.Params)
	}
}

func TestExpandGrid_UnknownKeyRejected(t *testing.T) {
	_, err := ExpandGrid([]GridDim{{Key: "nope", Values: []float64{1}}})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("want unknown-key error naming the key; got %v", err)
	}
}

func TestApplyParam_IntFieldsTruncate(t *testing.T) {
	p := strategy.DefaultMAPullbackParams()
	if err := applyParam(&p, "max_hold_minutes", 240); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if p.MaxHoldMinutes != 240 {
		t.Errorf("MaxHoldMinutes: got %d want 240", p.MaxHoldMinutes)
	}
	if err := applyParam(&p, "trend_min_slope_pips", 7.5); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if p.TrendMinSlopePips != 7.5 {
		t.Errorf("TrendMinSlopePips: got %v want 7.5", p.TrendMinSlopePips)
	}
}

// 隣接 = ちょうど 1 次元だけ index が ±1 違う combo (プラトー確認の定義)。
func TestNeighborsOf(t *testing.T) {
	grid := []GridDim{
		{Key: "ratchet_arm_pips", Values: []float64{12, 16, 20}},
		{Key: "zone_atr", Values: []float64{0.6, 0.8}},
	}
	combos, err := ExpandGrid(grid)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	// 中央 (arm=16, zone=0.6) の隣接 = arm12/zone0.6, arm20/zone0.6, arm16/zone0.8 の 3 つ。
	var center Combo
	found := false
	for _, c := range combos {
		if c.Params.RatchetArmPips == 16 && c.Params.ZoneATR == 0.6 {
			center, found = c, true
		}
	}
	if !found {
		t.Fatal("center combo not found")
	}
	nb := NeighborsOf(center, combos)
	if len(nb) != 3 {
		t.Fatalf("neighbors: got %d want 3", len(nb))
	}
}

// オフライン複製: frozen config の valid_from 罠 (IsActive が黙って 0 trades) を
// 必ず無効化し、symbol を上書きする。
func TestCloneForOffline_DefusesValidFromTrap(t *testing.T) {
	base := &config.StrategyConfig{
		ConfigID:   "frozen-mapb-usdjpy-v3-1",
		ValidFrom:  time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC),
		ValidUntil: time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC),
		Symbol:     "USD_JPY",
		Enabled:    true,
	}
	cl := CloneForOffline(base, "EUR_JPY")
	past := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	if !cl.IsActive(past) {
		// Strategy.Name が空だと IsActive の別条件で落ちることはない (no_trade 以外)。
		t.Fatalf("clone must be active across the whole backfill window; IsActive(2024-01-01)=false (from=%s until=%s)", cl.ValidFrom, cl.ValidUntil)
	}
	if cl.Symbol != "EUR_JPY" {
		t.Errorf("symbol override: got %s", cl.Symbol)
	}
	// 元 config は不変 (live の凍結値を絶対に触らない)。
	if base.ValidFrom.Year() != 2026 || base.Symbol != "USD_JPY" {
		t.Errorf("base config mutated: %+v", base)
	}
}

func TestSplits_Validate(t *testing.T) {
	good := Splits{
		TrainFrom: d("2023-11-01"), TrainTo: d("2025-04-01"),
		OOSFrom: d("2025-04-01"), OOSTo: d("2026-01-01"),
		HoldoutFrom: d("2026-01-01"), HoldoutTo: d("2026-06-11"),
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("good splits rejected: %v", err)
	}
	bad := good
	bad.OOSFrom = d("2025-03-01") // train と重複
	if err := bad.Validate(); err == nil {
		t.Fatal("overlapping train/oos must be rejected (検証を見ながらの調整は実験無効)")
	}
}

func d(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t
}

// trend_slope_lookback_1h はエンジンの履歴キャップ
// (maxHistoryBars=20000 1m ≈ 333 本の 1h) を超えると全 bar で
// insufficient_1h_candles → N=0 cull になり「エッジ無し」と区別不能。
// 負値は ma_pullback の slice 演算で panic する。範囲外は展開時に即エラー。
func TestApplyParam_TrendSlopeLookbackRangeValidated(t *testing.T) {
	p := strategy.DefaultMAPullbackParams()
	if err := applyParam(&p, "trend_slope_lookback_1h", 200); err == nil {
		t.Error("lookback 200 (> 133) must be rejected — silently culls every bar")
	}
	if err := applyParam(&p, "trend_slope_lookback_1h", -5); err == nil {
		t.Error("negative lookback must be rejected — panics in ma_pullback slice")
	}
	if err := applyParam(&p, "trend_slope_lookback_1h", 40); err != nil {
		t.Errorf("lookback 40 must be accepted: %v", err)
	}
	if err := applyParam(&p, "swing_lookback", 0); err == nil {
		t.Error("swing_lookback 0 must be rejected")
	}
	if err := applyParam(&p, "max_hold_minutes", -1); err == nil {
		t.Error("negative max_hold_minutes must be rejected")
	}
}

// グリッド全値が検証されること (先頭値だけ valid で途中に panic 値が混ざるケース)。
func TestExpandGrid_ValidatesEveryValue(t *testing.T) {
	_, err := ExpandGrid([]GridDim{{Key: "trend_slope_lookback_1h", Values: []float64{20, -5}}})
	if err == nil {
		t.Fatal("mid-grid invalid value must fail at expansion, not hours into the replay")
	}
}
