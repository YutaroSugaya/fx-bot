package analysis

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// 事前登録済みの判定パラメータ。
// confidence はフラグ化しない — 後から動かせると事前登録の意味がなくなる。
const (
	// JudgeConfidence は bootstrap CI の信頼水準(95% 固定)。
	JudgeConfidence = 0.95
	// JudgePFGate は promote_candidate に要求する PF 下限(>= 1.1)。
	JudgePFGate = 1.1
)

// Verdict は 3-way 判定の語彙。"N=100 合否" の置き換え:
// 微プラスは判定不能(continue)であって合格ではない — 微プラスはノイズと区別できない。
type Verdict string

const (
	// VerdictReject = CI 上限 < 0。コスト控除後 expectancy が負であることを
	// 高信頼で支持 = エッジ無しとして棄却。
	VerdictReject Verdict = "reject"
	// VerdictPromoteCandidate = CI 下限 > 0 かつ PF >= 1.1。合格"目安"。
	// プラトー確認(隣接パラメータでも勝つか)は sweep 側の責務なので
	// "candidate" 止まり — 名前で過信を防ぐ。
	VerdictPromoteCandidate Verdict = "promote_candidate"
	// VerdictContinue = 上記どちらでもない。判定不能 → 計測継続。
	VerdictContinue Verdict = "continue"
)

// JudgeResult は 1 回の判定の入力サマリと結論。Reasons は人間向けの判定根拠
// (例: "ci_lo=+0.42 > 0", "pf=1.18 >= 1.1")。
type JudgeResult struct {
	N       int
	Mean    float64
	CILo    float64
	CIHi    float64
	PF      float64
	Verdict Verdict
	Reasons []string
}

// Judge はコスト控除後 net PnL(JPY/trade)系列に事前登録 3-way 判定を適用する。
//
//   - reject:            ci_hi < 0(エッジ無しを高信頼で棄却)
//   - promote_candidate: ci_lo > 0 AND pf >= 1.1(合格目安)
//   - continue:          それ以外(判定不能 — 合格ではない)
//
// CI は BootstrapMeanCI(JudgeConfidence=0.95 固定)。取引ゼロは error。
func Judge(netPnLJPY []float64, resamples int, seed int64) (JudgeResult, error) {
	if len(netPnLJPY) == 0 {
		return JudgeResult{}, errors.New("analysis: Judge requires at least 1 trade")
	}
	lo, hi, err := BootstrapMeanCI(netPnLJPY, resamples, JudgeConfidence, seed)
	if err != nil {
		return JudgeResult{}, err
	}

	var sum float64
	for _, v := range netPnLJPY {
		sum += v
	}
	res := JudgeResult{
		N:    len(netPnLJPY),
		Mean: sum / float64(len(netPnLJPY)),
		CILo: lo,
		CIHi: hi,
		PF:   ProfitFactor(netPnLJPY),
	}

	switch {
	case hi < 0:
		res.Verdict = VerdictReject
		res.Reasons = []string{
			fmt.Sprintf("ci_hi=%+.2f < 0 (コスト控除後 expectancy の 95%%CI 上限が負 = エッジ無しを高信頼で棄却)", hi),
		}
	case lo > 0 && res.PF >= JudgePFGate:
		res.Verdict = VerdictPromoteCandidate
		res.Reasons = []string{
			fmt.Sprintf("ci_lo=%+.2f > 0", lo),
			fmt.Sprintf("pf=%.2f >= %.1f", res.PF, JudgePFGate),
		}
	default:
		res.Verdict = VerdictContinue
		reasons := make([]string, 0, 2)
		if lo <= 0 {
			reasons = append(reasons,
				fmt.Sprintf("ci_lo=%+.2f <= 0 (判定不能 — 微プラスは合格ではない)", lo))
		} else {
			reasons = append(reasons, fmt.Sprintf("ci_lo=%+.2f > 0", lo))
		}
		if res.PF < JudgePFGate {
			reasons = append(reasons, fmt.Sprintf("pf=%.2f < %.1f", res.PF, JudgePFGate))
		}
		res.Reasons = reasons
	}
	return res, nil
}

// jsonLine は machine-readable 出力のスキーマ。pf はポインタ:
// encoding/json は ±Inf/NaN を marshal できないため null に落とす(全勝 PF=+Inf 対策)。
type jsonLine struct {
	N       int      `json:"n"`
	Mean    float64  `json:"mean"`
	CILo    float64  `json:"ci_lo"`
	CIHi    float64  `json:"ci_hi"`
	PF      *float64 `json:"pf"`
	Verdict Verdict  `json:"verdict"`
	Reasons []string `json:"reasons"`
}

// RenderJSONLine は JudgeResult を machine-readable な 1 行 JSON に変換する。
// cmd/edge-judge / sweep パイプラインがそのまま出力・パースする。
func RenderJSONLine(r JudgeResult) (string, error) {
	l := jsonLine{
		N:       r.N,
		Mean:    r.Mean,
		CILo:    r.CILo,
		CIHi:    r.CIHi,
		Verdict: r.Verdict,
		Reasons: r.Reasons,
	}
	if !math.IsInf(r.PF, 0) && !math.IsNaN(r.PF) {
		pf := r.PF
		l.PF = &pf
	}
	b, err := json.Marshal(l)
	if err != nil {
		return "", fmt.Errorf("analysis: marshal json line: %w", err)
	}
	return string(b), nil
}
