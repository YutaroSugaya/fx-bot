package backtest

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Time-of-day spread model.
func TestTimeOfDaySpread_TokyoOpenSpike(t *testing.T) {
	m := TimeOfDaySpread{MedianPips: 0.5, TokyoOpenSpikePips: 9.5, SpikeStartJST: 5, SpikeEndJST: 8}
	// 03:00 UTC = 12:00 JST → midday, median.
	if got := m.SpreadPips(time.Date(2025, 1, 15, 3, 0, 0, 0, time.UTC)); got != 0.5 {
		t.Errorf("midday spread = %.2f, want 0.5", got)
	}
	// 20:30 UTC = 05:30 JST → inside the Tokyo-open spike window.
	if got := m.SpreadPips(time.Date(2025, 1, 15, 20, 30, 0, 0, time.UTC)); got != 10.0 {
		t.Errorf("tokyo-open spread = %.2f, want 10.0 (0.5 + 9.5)", got)
	}
	// 23:00 UTC = 08:00 JST → just after the window (exclusive end) → median.
	if got := m.SpreadPips(time.Date(2025, 1, 15, 23, 0, 0, 0, time.UTC)); got != 0.5 {
		t.Errorf("post-window spread = %.2f, want 0.5", got)
	}
}

func TestCostModel_LegSlipPips(t *testing.T) {
	midday := time.Date(2025, 1, 15, 3, 0, 0, 0, time.UTC) // 12:00 JST

	// No spread model → just SlippagePips.
	if got := (CostModel{SlippagePips: 0.5}).legSlipPips(midday); got != 0.5 {
		t.Errorf("no spread model: legSlip = %.2f, want 0.5", got)
	}
	// With spread model → SlippagePips + spread/2 (cross half the spread per leg).
	c := CostModel{SlippagePips: 0.3, SpreadModel: TimeOfDaySpread{MedianPips: 0.6}}
	if got := c.legSlipPips(midday); got != 0.6 { // 0.3 + 0.6/2
		t.Errorf("with spread model: legSlip = %.2f, want 0.6", got)
	}
}

// 時間帯別 (JST 24 バケット) spread モデル。
// 較正ツールが書く実測テーブルを backtest 側で消費する。
func TestHourlySpread_SpreadPips(t *testing.T) {
	var byHour [24]float64
	byHour[5] = 9.8 // Tokyo open spike (05 JST)
	byHour[6] = 2.0
	byHour[12] = 0.4
	m := HourlySpread{ByHourJST: byHour, FallbackPips: 0.5}

	cases := []struct {
		name string
		at   time.Time
		want float64
	}{
		// JST 変換エッジ: 20:59 UTC = 05:59 JST → hour 5 / 21:00 UTC = 06:00 JST → hour 6。
		{"20:59 UTC = 05:59 JST → hour 5 バケット", time.Date(2026, 6, 8, 20, 59, 0, 0, time.UTC), 9.8},
		{"21:00 UTC = 06:00 JST → hour 6 バケット", time.Date(2026, 6, 8, 21, 0, 0, 0, time.UTC), 2.0},
		{"03:00 UTC = 12:00 JST → hour 12 バケット", time.Date(2026, 6, 8, 3, 0, 0, 0, time.UTC), 0.4},
		// 空 (0) エントリは FallbackPips へフォールバック。
		{"01:00 UTC = 10:00 JST → 空バケット → fallback", time.Date(2026, 6, 8, 1, 0, 0, 0, time.UTC), 0.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.SpreadPips(tc.at); math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("SpreadPips(%v) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

// HourlySpread は SpreadModel インターフェイスを満たし、legSlipPips から消費される。
var _ SpreadModel = HourlySpread{}

func TestLoadSpreadFile(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "spreads.yaml")
		body := "spreads:\n" +
			"  USD_JPY:\n" +
			"    fallback_pips: 0.5\n" +
			"    by_hour_jst:\n" +
			"      0: 0.6\n" +
			"      5: 9.8\n" +
			"  EUR_JPY:\n" +
			"    fallback_pips: 0.8\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		spreads, err := LoadSpreadFile(path)
		if err != nil {
			t.Fatalf("LoadSpreadFile: %v", err)
		}
		usd, ok := spreads["USD_JPY"]
		if !ok {
			t.Fatalf("USD_JPY missing: %+v", spreads)
		}
		if usd.FallbackPips != 0.5 || usd.ByHourJST[0] != 0.6 || usd.ByHourJST[5] != 9.8 {
			t.Errorf("USD_JPY parsed wrong: %+v", usd)
		}
		// hour 1 は未指定 → 0 のまま → SpreadPips が fallback を返す。
		// 10:00 JST = 01:00 UTC。
		if got := usd.SpreadPips(time.Date(2026, 6, 8, 1, 0, 0, 0, time.UTC)); got != 0.5 {
			t.Errorf("unset hour should fall back: got %v want 0.5", got)
		}
		if eur := spreads["EUR_JPY"]; eur.FallbackPips != 0.8 {
			t.Errorf("EUR_JPY fallback: got %v want 0.8", eur.FallbackPips)
		}
	})
	t.Run("malformed yaml", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "spreads.yaml")
		if err := os.WriteFile(path, []byte("spreads: [broken"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSpreadFile(path); err == nil {
			t.Error("expected parse error for malformed yaml, got nil")
		}
	})
	t.Run("empty spreads rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "spreads.yaml")
		if err := os.WriteFile(path, []byte("other: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSpreadFile(path); err == nil {
			t.Error("expected error for missing spreads key, got nil")
		}
	})
	t.Run("out-of-range hour rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "spreads.yaml")
		body := "spreads:\n  USD_JPY:\n    fallback_pips: 0.5\n    by_hour_jst:\n      24: 1.0\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSpreadFile(path); err == nil {
			t.Error("expected error for hour 24, got nil")
		}
	})
	t.Run("missing file", func(t *testing.T) {
		if _, err := LoadSpreadFile(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
			t.Error("expected error for missing file, got nil")
		}
	})
}
