package market

import "testing"

// IsSpreadSpike は generate_strategy_config.md の「spread > avg * 2 で no_trade」
// 判定を Go 側にも持たせる純粋関数。
// 通り抜け疑いを audit log で可視化するための観測 hook。
func TestIsSpreadSpike(t *testing.T) {
	cases := []struct {
		name        string
		currentPips float64
		avg1hPips   float64
		wantSpike   bool
	}{
		{"normal (current == avg)", 0.5, 0.5, false},
		{"slight elevation (1.5x)", 0.75, 0.5, false},
		{"exactly 2x: NOT a spike", 1.0, 0.5, false},
		{"clear spike (>2x)", 1.5, 0.5, true},
		{"massive spike", 3.0, 0.5, true},
		{"zero avg: skip (insufficient data)", 0.8, 0, false},
		{"negative avg (corrupt): skip", 0.8, -0.5, false},
		{"both zero: skip", 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &MarketSummary{
				CurrentRate: CurrentRate{SpreadPips: c.currentPips},
				Summary1h:   WindowSummary{AvgSpreadPips: c.avg1hPips},
			}
			if got := IsSpreadSpike(s); got != c.wantSpike {
				t.Errorf("IsSpreadSpike(current=%v, avg=%v) = %v, want %v",
					c.currentPips, c.avg1hPips, got, c.wantSpike)
			}
		})
	}
}
