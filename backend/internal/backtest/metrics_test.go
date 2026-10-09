package backtest

import (
	"math"
	"testing"
	"time"
)

func tradeAt(pnl float64, openedAt time.Time) Trade {
	return Trade{
		Side:           "BUY",
		ProfitLossJPY:  pnl,
		ProfitLossPips: pnl, // simplified for test
		OpenedAt:       openedAt,
		ClosedAt:       openedAt.Add(time.Minute),
	}
}

// TestComputeMetrics_BasicProfitFactor は PF, Expectancy, WinRate, SampleSize
// の基本計算を fixed series で検証する。
func TestComputeMetrics_BasicProfitFactor(t *testing.T) {
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	trades := []Trade{
		tradeAt(100, t0),
		tradeAt(-50, t0.Add(time.Minute)),
		tradeAt(200, t0.Add(2*time.Minute)),
		tradeAt(-100, t0.Add(3*time.Minute)),
		tradeAt(50, t0.Add(4*time.Minute)),
	}
	m := ComputeMetrics(trades)

	// sum wins = 350, sum losses = 150, PF = 350/150 = 2.333...
	wantPF := 350.0 / 150.0
	if math.Abs(m.ProfitFactor-wantPF) > 1e-9 {
		t.Errorf("ProfitFactor: got %v, want %v", m.ProfitFactor, wantPF)
	}
	// expectancy = (350-150)/5 = 40
	if math.Abs(m.Expectancy-40.0) > 1e-9 {
		t.Errorf("Expectancy: got %v, want %v", m.Expectancy, 40.0)
	}
	// win rate = 3/5 = 0.6
	if math.Abs(m.WinRate-0.6) > 1e-9 {
		t.Errorf("WinRate: got %v, want %v", m.WinRate, 0.6)
	}
	if m.SampleSize != 5 {
		t.Errorf("SampleSize: got %d, want 5", m.SampleSize)
	}
}

func TestComputeMetrics_Empty_ReturnsZeros(t *testing.T) {
	m := ComputeMetrics(nil)
	if m.SampleSize != 0 || m.ProfitFactor != 0 || m.Expectancy != 0 || m.MaxDrawdown != 0 || m.WinRate != 0 {
		t.Errorf("empty trades should produce zero Metrics, got %+v", m)
	}
}

func TestComputeMetrics_AllWins_PFIsInfinite(t *testing.T) {
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	trades := []Trade{tradeAt(50, t0), tradeAt(100, t0.Add(time.Minute))}
	m := ComputeMetrics(trades)
	if !math.IsInf(m.ProfitFactor, 1) {
		t.Errorf("PF with no losses should be +Inf, got %v", m.ProfitFactor)
	}
	if m.WinRate != 1.0 {
		t.Errorf("WinRate: got %v want 1.0", m.WinRate)
	}
	if m.MaxDrawdown != 0 {
		t.Errorf("MaxDrawdown on all-wins should be 0, got %v", m.MaxDrawdown)
	}
}

func TestComputeMetrics_AllLosses_PFIsZero(t *testing.T) {
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	trades := []Trade{tradeAt(-50, t0), tradeAt(-30, t0.Add(time.Minute))}
	m := ComputeMetrics(trades)
	if m.ProfitFactor != 0 {
		t.Errorf("PF with no wins should be 0, got %v", m.ProfitFactor)
	}
	if m.WinRate != 0 {
		t.Errorf("WinRate: got %v want 0", m.WinRate)
	}
	// MDD: equity -50 → -80, peak=0 → max drop = 80
	if math.Abs(m.MaxDrawdown-80.0) > 1e-9 {
		t.Errorf("MaxDrawdown: got %v want 80", m.MaxDrawdown)
	}
}

// TestComputeMetrics_MaxDrawdown_TroughAfterPeak は equity curve が
// up → peak → trough → 回復 という典型パターンで peak-to-trough drop を
// 正しく拾うかを検証する。
func TestComputeMetrics_MaxDrawdown_TroughAfterPeak(t *testing.T) {
	t0 := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	// equity curve: 0 → +100 → +300 (peak) → +200 → +100 (trough, drop 200) → +250
	trades := []Trade{
		tradeAt(100, t0),
		tradeAt(200, t0.Add(time.Minute)), // peak 300
		tradeAt(-100, t0.Add(2*time.Minute)),
		tradeAt(-100, t0.Add(3*time.Minute)), // trough 100, dd=200
		tradeAt(150, t0.Add(4*time.Minute)),
	}
	m := ComputeMetrics(trades)
	if math.Abs(m.MaxDrawdown-200.0) > 1e-9 {
		t.Errorf("MaxDrawdown: got %v want 200", m.MaxDrawdown)
	}
}
