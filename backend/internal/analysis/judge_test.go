package analysis

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// repeat は (win 値 × wins 回, loss 値 × losses 回) を reps 回繰り返した
// PnL 系列を作るテストヘルパ。
func repeat(win float64, wins int, loss float64, losses int, reps int) []float64 {
	out := make([]float64, 0, (wins+losses)*reps)
	for r := 0; r < reps; r++ {
		for i := 0; i < wins; i++ {
			out = append(out, win)
		}
		for i := 0; i < losses; i++ {
			out = append(out, loss)
		}
	}
	return out
}

// TestJudge_Reject_CIHiBelowZero は CI 上限が負(= エッジ無しを高信頼で棄却)
// なら verdict=reject になることを検証する。
func TestJudge_Reject_CIHiBelowZero(t *testing.T) {
	// 定数 -2.0 → CI は [-2, -2] に潰れ ci_hi < 0 が確定する。
	values := repeat(0, 0, -2.0, 1, 20)
	res, err := Judge(values, 1000, 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Verdict != VerdictReject {
		t.Errorf("verdict: got %q, want %q (CIHi=%v)", res.Verdict, VerdictReject, res.CIHi)
	}
	if res.CIHi >= 0 {
		t.Errorf("test premise broken: want CIHi < 0, got %v", res.CIHi)
	}
	if res.N != 20 {
		t.Errorf("N: got %d, want 20", res.N)
	}
	if math.Abs(res.Mean-(-2.0)) > 1e-9 {
		t.Errorf("Mean: got %v, want -2.0", res.Mean)
	}
	if len(res.Reasons) == 0 || !strings.Contains(res.Reasons[0], "ci_hi=") {
		t.Errorf("Reasons should cite ci_hi, got %v", res.Reasons)
	}
}

// TestJudge_PromoteCandidate_CILoPositiveAndPFGate は CI 下限 > 0 かつ PF >= 1.1
// で verdict=promote_candidate になることを検証する(合格目安。プラトー確認は
// sweep 側の責務なので "candidate" 止まり)。
func TestJudge_PromoteCandidate_CILoPositiveAndPFGate(t *testing.T) {
	// +10 × 30, -5 × 5 → mean ≈ +7.86, PF = 300/25 = 12。CI 下限は確実に正。
	values := repeat(10, 30, -5, 5, 1)
	res, err := Judge(values, 2000, 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Verdict != VerdictPromoteCandidate {
		t.Errorf("verdict: got %q, want %q (CILo=%v PF=%v)",
			res.Verdict, VerdictPromoteCandidate, res.CILo, res.PF)
	}
	if res.CILo <= 0 {
		t.Errorf("test premise broken: want CILo > 0, got %v", res.CILo)
	}
	if math.Abs(res.PF-12.0) > 1e-9 {
		t.Errorf("PF: got %v, want 12.0", res.PF)
	}
	joined := strings.Join(res.Reasons, " / ")
	if !strings.Contains(joined, "ci_lo=") || !strings.Contains(joined, "pf=") {
		t.Errorf("Reasons should cite ci_lo and pf, got %v", res.Reasons)
	}
}

// TestJudge_Continue_CIStraddlesZero は CI が 0 を跨ぐ(判定不能)なら
// verdict=continue になることを検証する。微プラスは合格ではない。
func TestJudge_Continue_CIStraddlesZero(t *testing.T) {
	// +10 / -10 交互 → mean = 0、CI は 0 を跨ぐ。PF = 1.0 < 1.1。
	values := repeat(10, 1, -10, 1, 20)
	res, err := Judge(values, 2000, 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Verdict != VerdictContinue {
		t.Errorf("verdict: got %q, want %q (CI=[%v, %v])",
			res.Verdict, VerdictContinue, res.CILo, res.CIHi)
	}
	if !(res.CILo <= 0 && res.CIHi >= 0) {
		t.Errorf("test premise broken: want CI straddling 0, got [%v, %v]", res.CILo, res.CIHi)
	}
	if len(res.Reasons) == 0 {
		t.Errorf("Reasons must not be empty for continue")
	}
}

// TestJudge_Continue_PositiveCIButLowPF は CI 下限 > 0 でも PF < 1.1 なら
// promote しない(両ゲート AND)ことを検証する。
func TestJudge_Continue_PositiveCIButLowPF(t *testing.T) {
	// +1 × 109, -1 × 100 を 30 回繰り返し (n=6270)。
	// mean ≈ +0.043, SE ≈ 0.0126 → CI 下限 ≈ +0.018 > 0。PF = 109/100 = 1.09 < 1.1。
	values := repeat(1, 109, -1, 100, 30)
	res, err := Judge(values, 2000, 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.CILo <= 0 {
		t.Fatalf("test premise broken: want CILo > 0, got %v (re-pick sample)", res.CILo)
	}
	if res.PF >= 1.1 {
		t.Fatalf("test premise broken: want PF < 1.1, got %v", res.PF)
	}
	if res.Verdict != VerdictContinue {
		t.Errorf("verdict: got %q, want %q (CILo=%v PF=%v)",
			res.Verdict, VerdictContinue, res.CILo, res.PF)
	}
	if !strings.Contains(strings.Join(res.Reasons, " / "), "pf=") {
		t.Errorf("Reasons should cite the failed pf gate, got %v", res.Reasons)
	}
}

// TestJudge_EmptyInput_ReturnsError は取引ゼロでは判定不能 → 明示 error を検証する。
func TestJudge_EmptyInput_ReturnsError(t *testing.T) {
	if _, err := Judge(nil, 1000, 42); err == nil {
		t.Fatal("want error on empty input, got nil")
	}
	if _, err := Judge([]float64{}, 1000, 42); err == nil {
		t.Fatal("want error on empty slice, got nil")
	}
}

// TestJudge_Deterministic_SameSeed は同一 seed なら JudgeResult が完全に再現する
// ことを検証する(事前登録判定の再現性)。
func TestJudge_Deterministic_SameSeed(t *testing.T) {
	values := []float64{5, -3, 12, 7, -8, 2, 15, -1, 4, 9}
	r1, err1 := Judge(values, 1000, 42)
	r2, err2 := Judge(values, 1000, 42)
	if err1 != nil || err2 != nil {
		t.Fatalf("unexpected error: err1=%v err2=%v", err1, err2)
	}
	if r1.CILo != r2.CILo || r1.CIHi != r2.CIHi || r1.Verdict != r2.Verdict {
		t.Errorf("same seed must reproduce identical result: %+v vs %+v", r1, r2)
	}
}

// TestVerdictStringValues は machine-readable 出力で使う文字列値を固定する
// (事前登録した語彙を変えない)。
func TestVerdictStringValues(t *testing.T) {
	tests := []struct {
		v    Verdict
		want string
	}{
		{VerdictReject, "reject"},
		{VerdictPromoteCandidate, "promote_candidate"},
		{VerdictContinue, "continue"},
	}
	for _, tt := range tests {
		if string(tt.v) != tt.want {
			t.Errorf("Verdict %v: got %q, want %q", tt.v, string(tt.v), tt.want)
		}
	}
}

// TestRenderJSONLine は machine-readable 1 行 JSON のキーと値を検証する。
// cmd/edge-judge がこのまま出力する(pure 部分を package 側でテストする)。
func TestRenderJSONLine(t *testing.T) {
	res := JudgeResult{
		N: 35, Mean: 7.857142, CILo: 5.0, CIHi: 10.0, PF: 12.0,
		Verdict: VerdictPromoteCandidate,
		Reasons: []string{"ci_lo=+5.00 > 0", "pf=12.00 >= 1.1"},
	}
	line, err := RenderJSONLine(res)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, line)
	}
	if got := m["n"].(float64); got != 35 {
		t.Errorf("n: got %v, want 35", got)
	}
	if got := m["verdict"].(string); got != "promote_candidate" {
		t.Errorf("verdict: got %q, want promote_candidate", got)
	}
	for _, key := range []string{"mean", "ci_lo", "ci_hi", "pf"} {
		if _, ok := m[key].(float64); !ok {
			t.Errorf("key %q missing or not a number: %v", key, m[key])
		}
	}
	if math.Abs(m["pf"].(float64)-12.0) > 1e-9 {
		t.Errorf("pf: got %v, want 12.0", m["pf"])
	}
}

// TestRenderJSONLine_InfinitePF_EncodesNull は PF=+Inf(全勝)でも JSON が壊れず
// pf を null にすることを検証する(encoding/json は ±Inf を marshal できない)。
func TestRenderJSONLine_InfinitePF_EncodesNull(t *testing.T) {
	res := JudgeResult{
		N: 2, Mean: 75, CILo: 50, CIHi: 100, PF: math.Inf(1),
		Verdict: VerdictContinue, Reasons: []string{"n too small"},
	}
	line, err := RenderJSONLine(res)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, line)
	}
	if m["pf"] != nil {
		t.Errorf("pf with +Inf should encode as null, got %v", m["pf"])
	}
}
