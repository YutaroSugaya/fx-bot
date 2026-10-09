package backtest

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// SpreadModel returns the bid/ask spread (pips) at a given time. The backtest
// crosses half the spread per leg (entry + exit), on top of any fixed
// SlippagePips. It models the time-of-day spread (normal ~0.5pip +
// Tokyo-open spike ~10pips) instead of a single constant.
type SpreadModel interface {
	SpreadPips(t time.Time) float64
}

// btJSTZone is the fixed +09:00 zone used to bucket the Tokyo-open window.
var btJSTZone = time.FixedZone("JST", 9*60*60)

// TimeOfDaySpread is the median + Tokyo-open-spike mixture.
// Outside the spike window the spread is MedianPips; inside [SpikeStartJST,
// SpikeEndJST) it is MedianPips + TokyoOpenSpikePips. The Tokyo open (≈05–08 JST)
// is where USD/JPY spreads blow out to ~10pips on GMO.
//
// ⚠️ Calibration caveat: market_summaries の閉場 snapshot には週末の spread
// (10-11pips) が混入し得るので、このモデルの値を実データから
// 較正するときは週末/閉場を除外すること。
type TimeOfDaySpread struct {
	MedianPips         float64
	TokyoOpenSpikePips float64
	SpikeStartJST      int // inclusive hour, JST
	SpikeEndJST        int // exclusive hour, JST
}

func (m TimeOfDaySpread) SpreadPips(t time.Time) float64 {
	h := t.In(btJSTZone).Hour()
	if h >= m.SpikeStartJST && h < m.SpikeEndJST {
		return m.MedianPips + m.TokyoOpenSpikePips
	}
	return m.MedianPips
}

// HourlySpread is the calibrated per-hour spread
// model: 24 JST hour buckets (pips). A 0 bucket means "no calibration data
// for that hour" and falls back to FallbackPips. Produced by the sibling
// calibration tool via LoadSpreadFile; supersedes TimeOfDaySpread when a
// symbol entry exists in the -spread-file.
//
// ⚠️ TimeOfDaySpread と同じ較正注意: 週末/閉場 snapshot の
// spread 混入を除外してから較正すること。
type HourlySpread struct {
	ByHourJST    [24]float64 // pips per JST hour bucket; 0 = no data → fallback
	FallbackPips float64
}

func (m HourlySpread) SpreadPips(t time.Time) float64 {
	if v := m.ByHourJST[t.In(btJSTZone).Hour()]; v != 0 {
		return v
	}
	return m.FallbackPips
}

// LoadSpreadFile reads a -spread-file YAML into per-symbol HourlySpread models.
// フォーマットは較正ツールが書く形と完全一致させること (安定契約):
//
//	spreads:
//	  USD_JPY:
//	    fallback_pips: 0.5
//	    by_hour_jst:
//	      0: 0.6
//	      5: 9.8
func LoadSpreadFile(path string) (map[string]HourlySpread, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read spread file: %w", err)
	}
	var doc struct {
		Spreads map[string]struct {
			FallbackPips float64         `yaml:"fallback_pips"`
			ByHourJST    map[int]float64 `yaml:"by_hour_jst"`
		} `yaml:"spreads"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse spread file: %w", err)
	}
	if len(doc.Spreads) == 0 {
		return nil, fmt.Errorf("spread file %s: spreads must be a non-empty map", path)
	}
	out := make(map[string]HourlySpread, len(doc.Spreads))
	for sym, s := range doc.Spreads {
		hs := HourlySpread{FallbackPips: s.FallbackPips}
		for h, v := range s.ByHourJST {
			if h < 0 || h > 23 {
				return nil, fmt.Errorf("spread file %s: %s by_hour_jst hour %d out of range [0,23]", path, sym, h)
			}
			hs.ByHourJST[h] = v
		}
		out[sym] = hs
	}
	return out, nil
}

// legSlipPips is the adverse price adjustment applied per leg (entry, exit):
// the fixed SlippagePips plus half the modeled spread (you cross half the
// bid/ask each side). When SpreadModel is nil it is just SlippagePips
// (back-compat with the constant-slippage runs).
func (c CostModel) legSlipPips(t time.Time) float64 {
	slip := c.SlippagePips
	if c.SpreadModel != nil {
		slip += c.SpreadModel.SpreadPips(t) / 2.0
	}
	return slip
}
