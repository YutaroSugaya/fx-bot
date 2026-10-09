// Package analysis は事前登録済みの統計判定(bootstrap CI / PF / 3-way verdict)を
// 提供する純粋パッケージ。
//
// 設計方針:
//   - domain-free / I/O なし / stdlib のみ(math/rand を含む)。
//   - 同一 seed で bit-exact に再現する(事前登録判定の再現性要件)。
//   - 呼び出し側は backtest/sweep パイプラインと cmd/edge-judge。
package analysis

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
)

// BootstrapMeanCI は平均値の percentile bootstrap 信頼区間 [lo, hi] を返す。
// values から n 個を復元抽出 → 平均、を resamples 回繰り返し、その分布の
// (1-confidence)/2 / 1-(1-confidence)/2 分位点(線形補間)を区間とする。
//
// 同一 (values, resamples, confidence, seed) なら結果は完全に再現する
// (rand.New(rand.NewSource(seed)) のみが乱数源)。
// n==0 / resamples<1 / confidence∉(0,1) は (0, 0, error) を明示的に返す。
func BootstrapMeanCI(values []float64, resamples int, confidence float64, seed int64) (lo, hi float64, err error) {
	n := len(values)
	if n == 0 {
		return 0, 0, errors.New("analysis: BootstrapMeanCI requires at least 1 value")
	}
	if resamples < 1 {
		return 0, 0, fmt.Errorf("analysis: resamples must be >= 1, got %d", resamples)
	}
	if confidence <= 0 || confidence >= 1 {
		return 0, 0, fmt.Errorf("analysis: confidence must be in (0, 1), got %g", confidence)
	}

	rng := rand.New(rand.NewSource(seed))
	means := make([]float64, resamples)
	for i := range means {
		var sum float64
		for j := 0; j < n; j++ {
			sum += values[rng.Intn(n)]
		}
		means[i] = sum / float64(n)
	}
	sort.Float64s(means)

	alpha := (1 - confidence) / 2
	return quantileSorted(means, alpha), quantileSorted(means, 1-alpha), nil
}

// quantileSorted は昇順ソート済み slice の q∈[0,1] 分位点を線形補間で返す。
func quantileSorted(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 1 {
		return sorted[0]
	}
	pos := q * float64(n-1)
	i := int(math.Floor(pos))
	if i >= n-1 {
		return sorted[n-1]
	}
	frac := pos - float64(i)
	return sorted[i] + frac*(sorted[i+1]-sorted[i])
}

// ProfitFactor は PF = sum(wins) / |sum(losses)| を返す。
// 定義は internal/backtest/metrics.go の ComputeMetrics と同一:
// 負け無しで勝ちあり → +Inf、取引なし・勝ちなし → 0。PnL==0 はどちらにも数えない。
func ProfitFactor(values []float64) float64 {
	var sumWins, sumLossesAbs float64
	for _, v := range values {
		switch {
		case v > 0:
			sumWins += v
		case v < 0:
			sumLossesAbs += -v
		}
	}
	switch {
	case sumLossesAbs == 0 && sumWins > 0:
		return math.Inf(1)
	case sumLossesAbs > 0:
		return sumWins / sumLossesAbs
	default:
		return 0
	}
}
