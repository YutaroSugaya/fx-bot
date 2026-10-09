package analysis

import (
	"math"
	"testing"
)

// TestBootstrapMeanCI_Deterministic_SameSeed は同一 seed なら CI が完全一致する
// (事前登録の再現性要件)ことを検証する。
func TestBootstrapMeanCI_Deterministic_SameSeed(t *testing.T) {
	values := []float64{5, -3, 12, 7, -8, 2, 15, -1, 4, 9}

	lo1, hi1, err1 := BootstrapMeanCI(values, 1000, 0.95, 42)
	lo2, hi2, err2 := BootstrapMeanCI(values, 1000, 0.95, 42)
	if err1 != nil || err2 != nil {
		t.Fatalf("unexpected error: err1=%v err2=%v", err1, err2)
	}
	// 同一 seed → bit-exact で一致(percentile bootstrap は乱数列だけが源)。
	if lo1 != lo2 || hi1 != hi2 {
		t.Errorf("same seed must reproduce identical CI: got [%v, %v] vs [%v, %v]",
			lo1, hi1, lo2, hi2)
	}

	// 異なる seed は(この固定入力では)異なる CI を返す。seed 固定なので flake しない。
	lo3, hi3, err3 := BootstrapMeanCI(values, 1000, 0.95, 43)
	if err3 != nil {
		t.Fatalf("unexpected error: %v", err3)
	}
	if lo1 == lo3 && hi1 == hi3 {
		t.Errorf("different seed should yield a different CI: both [%v, %v]", lo1, hi1)
	}
}

// TestBootstrapMeanCI_ConstantValues_CollapsesToValue は定数サンプルなら
// resample 平均が全て同値になり lo == hi == value に潰れることを検証する。
func TestBootstrapMeanCI_ConstantValues_CollapsesToValue(t *testing.T) {
	values := []float64{7.5, 7.5, 7.5, 7.5, 7.5, 7.5, 7.5, 7.5}
	lo, hi, err := BootstrapMeanCI(values, 500, 0.95, 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(lo-7.5) > 1e-12 || math.Abs(hi-7.5) > 1e-12 {
		t.Errorf("constant sample: want lo==hi==7.5, got [%v, %v]", lo, hi)
	}
}

// TestBootstrapMeanCI_ClearlyPositiveSample_LoAboveZero は明確にプラスの分布
// (≈10 ± 0.7)で CI 下限 > 0、かつ lo <= 標本平均 <= hi を検証する。
func TestBootstrapMeanCI_ClearlyPositiveSample_LoAboveZero(t *testing.T) {
	values := []float64{9.5, 10.2, 10.8, 9.9, 10.1, 10.4, 9.7, 10.0, 10.6, 9.8}
	lo, hi, err := BootstrapMeanCI(values, 2000, 0.95, 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lo <= 0 {
		t.Errorf("clearly positive sample: want lo > 0, got lo=%v", lo)
	}
	if lo > hi {
		t.Errorf("want lo <= hi, got [%v, %v]", lo, hi)
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(len(values))
	if mean < lo || mean > hi {
		t.Errorf("sample mean %v should lie inside CI [%v, %v]", mean, lo, hi)
	}
}

// TestBootstrapMeanCI_InvalidInput_ReturnsError は n==0 / resamples / confidence
// の不正値で明示的に error を返す(0,0 を黙って返さない)ことを検証する。
func TestBootstrapMeanCI_InvalidInput_ReturnsError(t *testing.T) {
	valid := []float64{1, 2, 3}
	tests := []struct {
		name       string
		values     []float64
		resamples  int
		confidence float64
	}{
		{"empty values", []float64{}, 1000, 0.95},
		{"nil values", nil, 1000, 0.95},
		{"resamples zero", valid, 0, 0.95},
		{"resamples negative", valid, -1, 0.95},
		{"confidence zero", valid, 1000, 0},
		{"confidence one", valid, 1000, 1},
		{"confidence above one", valid, 1000, 1.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lo, hi, err := BootstrapMeanCI(tt.values, tt.resamples, tt.confidence, 42)
			if err == nil {
				t.Fatalf("want error, got nil (lo=%v hi=%v)", lo, hi)
			}
			if lo != 0 || hi != 0 {
				t.Errorf("on error want (0, 0), got (%v, %v)", lo, hi)
			}
		})
	}
}

// TestProfitFactor は PF = sum(wins)/|sum(losses)| の定義と端ケース
// (全勝 +Inf / 取引なし 0 / 全敗 0 / PnL==0 はどちらにも数えない)を検証する。
// backtest.ComputeMetrics と同じ定義(internal/backtest/metrics.go)。
func TestProfitFactor(t *testing.T) {
	tests := []struct {
		name    string
		values  []float64
		want    float64
		wantInf bool
	}{
		{"wins and losses", []float64{100, -50, 200, -100, 50}, 350.0 / 150.0, false},
		{"all wins -> +Inf", []float64{50, 100}, 0, true},
		{"all losses -> 0", []float64{-50, -30}, 0, false},
		{"no trades -> 0", nil, 0, false},
		{"zero-only trades -> 0", []float64{0, 0}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ProfitFactor(tt.values)
			if tt.wantInf {
				if !math.IsInf(got, 1) {
					t.Errorf("want +Inf, got %v", got)
				}
				return
			}
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
