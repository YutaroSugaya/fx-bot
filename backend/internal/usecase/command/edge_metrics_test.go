package command

import (
	"math"
	"testing"

	"fx-bot/backend/internal/port"
)

// tr builds a closed TradeRecord with just the pips/jpy fields the edge
// metrics care about.
func tr(pips, jpy float64) port.TradeRecord {
	return port.TradeRecord{ProfitLossPips: pips, ProfitLossJPY: jpy}
}

func approxEq(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestDeriveEdgeMetrics(t *testing.T) {
	cases := []struct {
		name   string
		trades []port.TradeRecord
		want   EdgeMetrics
	}{
		{
			name:   "empty: all zero",
			trades: nil,
			want:   EdgeMetrics{},
		},
		{
			// 代表的な 8 trade の系列: 勝率は高いが
			// 勝ち平均 ~4.9pips / 負け 15pips = 実効RR≈0.32 の逆RR。
			name: "8 trades: high win rate but inverted RR",
			trades: []port.TradeRecord{
				tr(2.4, 24), tr(5.1, 51), tr(3.3, 33), tr(4.1, 41),
				tr(-15, -150), tr(3.0, 30), tr(3.4, 34), tr(12.8, 128),
			},
			want: EdgeMetrics{
				TradeCount:           8,
				WinCount:             7,
				LossCount:            1,
				WinRatePct:           87.5,
				GrossProfitJPY:       341,
				GrossLossJPY:         150,
				NetPnLJPY:            191,
				ProfitFactor:         2.2733,
				AvgWinPips:           4.8714,
				AvgLossPips:          15,
				RewardRisk:           0.3248,
				ExpectancyJPY:        23.875,
				MaxConsecutiveLosses: 1,
			},
		},
		{
			name:   "all losses: PF/RR are N/A (0), streak counts",
			trades: []port.TradeRecord{tr(-10, -100), tr(-12, -120), tr(-8, -80)},
			want: EdgeMetrics{
				TradeCount: 3, WinCount: 0, LossCount: 3, WinRatePct: 0,
				GrossProfitJPY: 0, GrossLossJPY: 300, NetPnLJPY: -300,
				ProfitFactor: 0, AvgWinPips: 0, AvgLossPips: 10,
				RewardRisk: 0, ExpectancyJPY: -100, MaxConsecutiveLosses: 3,
			},
		},
		{
			name:   "no losses: PF/RR N/A (0), no loss streak",
			trades: []port.TradeRecord{tr(6, 60), tr(8, 80)},
			want: EdgeMetrics{
				TradeCount: 2, WinCount: 2, LossCount: 0, WinRatePct: 100,
				GrossProfitJPY: 140, GrossLossJPY: 0, NetPnLJPY: 140,
				ProfitFactor: 0, AvgWinPips: 7, AvgLossPips: 0,
				RewardRisk: 0, ExpectancyJPY: 70, MaxConsecutiveLosses: 0,
			},
		},
		{
			name:   "interleaved W L L W L: max streak 2",
			trades: []port.TradeRecord{tr(5, 50), tr(-5, -50), tr(-5, -50), tr(5, 50), tr(-5, -50)},
			want: EdgeMetrics{
				TradeCount: 5, WinCount: 2, LossCount: 3, WinRatePct: 40,
				GrossProfitJPY: 100, GrossLossJPY: 150, NetPnLJPY: -50,
				ProfitFactor: 0.6667, AvgWinPips: 5, AvgLossPips: 5,
				RewardRisk: 1.0, ExpectancyJPY: -10, MaxConsecutiveLosses: 2,
			},
		},
		{
			name:   "breakeven (0 pnl) resets loss streak, not counted as loss",
			trades: []port.TradeRecord{tr(-5, -50), tr(0, 0), tr(-5, -50)},
			want: EdgeMetrics{
				TradeCount: 3, WinCount: 0, LossCount: 2, WinRatePct: 0,
				GrossProfitJPY: 0, GrossLossJPY: 100, NetPnLJPY: -100,
				ProfitFactor: 0, AvgWinPips: 0, AvgLossPips: 5,
				RewardRisk: 0, ExpectancyJPY: -33.33, MaxConsecutiveLosses: 1,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveEdgeMetrics(tc.trades)
			if got.TradeCount != tc.want.TradeCount ||
				got.WinCount != tc.want.WinCount ||
				got.LossCount != tc.want.LossCount ||
				got.MaxConsecutiveLosses != tc.want.MaxConsecutiveLosses {
				t.Errorf("int fields mismatch:\n got=%+v\nwant=%+v", got, tc.want)
			}
			floats := []struct {
				name     string
				got, exp float64
			}{
				{"WinRatePct", got.WinRatePct, tc.want.WinRatePct},
				{"GrossProfitJPY", got.GrossProfitJPY, tc.want.GrossProfitJPY},
				{"GrossLossJPY", got.GrossLossJPY, tc.want.GrossLossJPY},
				{"NetPnLJPY", got.NetPnLJPY, tc.want.NetPnLJPY},
				{"ProfitFactor", got.ProfitFactor, tc.want.ProfitFactor},
				{"AvgWinPips", got.AvgWinPips, tc.want.AvgWinPips},
				{"AvgLossPips", got.AvgLossPips, tc.want.AvgLossPips},
				{"RewardRisk", got.RewardRisk, tc.want.RewardRisk},
				{"ExpectancyJPY", got.ExpectancyJPY, tc.want.ExpectancyJPY},
			}
			for _, f := range floats {
				if !approxEq(f.got, f.exp) {
					t.Errorf("%s = %.4f, want %.4f", f.name, f.got, f.exp)
				}
			}
		})
	}
}
