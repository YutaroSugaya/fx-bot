package command

import (
	"testing"

	"fx-bot/backend/internal/port"
)

// edge metrics の gross / fee / net 分離。
//   - GrossPnLJPY = Σ profit_loss_jpy (従来の "NetPnLJPY" は実は gross だった)
//   - FeeTotalJPY / SwapTotalJPY = Σ fee_jpy / Σ swap_jpy
//   - NetPnLJPY = gross − fee + swap (真の net)
//   - ExpectancyJPY は net ベース — エッジ判定は必ず net を使う。
//   - 勝敗分類 / PF / RR は gross のまま (入口の質の指標として連続性を維持)。
func TestDeriveEdgeMetrics_SeparatesGrossFeeNet(t *testing.T) {
	trades := []port.TradeRecord{
		{ProfitLossPips: 10, ProfitLossJPY: 100, FeeJPY: 7.4, SwapJPY: 0},
		{ProfitLossPips: -5, ProfitLossJPY: -50, FeeJPY: 7.4, SwapJPY: -12, FeeEstimated: true},
	}
	m := DeriveEdgeMetrics(trades)

	if !approxEq(m.GrossPnLJPY, 50) {
		t.Errorf("GrossPnLJPY: got %v want 50", m.GrossPnLJPY)
	}
	if !approxEq(m.FeeTotalJPY, 14.8) {
		t.Errorf("FeeTotalJPY: got %v want 14.8", m.FeeTotalJPY)
	}
	if !approxEq(m.SwapTotalJPY, -12) {
		t.Errorf("SwapTotalJPY: got %v want -12", m.SwapTotalJPY)
	}
	// net = 50 − 14.8 + (−12) = 23.2
	if !approxEq(m.NetPnLJPY, 23.2) {
		t.Errorf("NetPnLJPY: got %v want 23.2 (gross−fee+swap)", m.NetPnLJPY)
	}
	// expectancy は net ベース = 23.2 / 2 = 11.6
	if !approxEq(m.ExpectancyJPY, 11.6) {
		t.Errorf("ExpectancyJPY: got %v want 11.6 (net/count)", m.ExpectancyJPY)
	}
	if m.FeeEstimatedCount != 1 {
		t.Errorf("FeeEstimatedCount: got %d want 1", m.FeeEstimatedCount)
	}
	// 勝敗分類は gross のまま (1勝1敗)
	if m.WinCount != 1 || m.LossCount != 1 {
		t.Errorf("win/loss must stay gross-based: got %d/%d want 1/1", m.WinCount, m.LossCount)
	}
}

// fee=swap=0 の旧データでは net == gross — 後方互換 (既存表示の数値は不変)。
func TestDeriveEdgeMetrics_ZeroCostsBackCompat(t *testing.T) {
	trades := []port.TradeRecord{tr(10, 100), tr(-5, -50)}
	m := DeriveEdgeMetrics(trades)
	if !approxEq(m.NetPnLJPY, m.GrossPnLJPY) || !approxEq(m.NetPnLJPY, 50) {
		t.Errorf("zero-cost rows: net (%v) must equal gross (%v) = 50", m.NetPnLJPY, m.GrossPnLJPY)
	}
	if !approxEq(m.ExpectancyJPY, 25) {
		t.Errorf("ExpectancyJPY: got %v want 25", m.ExpectancyJPY)
	}
}
