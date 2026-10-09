// cmd/sweep — walk-forward スイープ基盤。
//
// ma_pullback のチューニングパラメータ (strategy.MAPullbackParams) をグリッドで
// 注入し、train / OOS / holdout の 3 分割で検定する。検証プロトコルを
// 機械的に強制する:
//
//   - 改善・探索は train のみ。何百回試しても可。
//   - 粗選抜: train でコスト後 PF<1 は即棄却 (摩擦込みで負けるものは必ず悪化)。
//   - OOS は上位 -top 件だけが 1 回見る。
//   - holdout は -unlock-holdout を明示したときだけ開封 (1 回だけの最終試験)。
//   - N_trials (試行総数) を必ず記録・出力し、最良成績は多重比較込みで割り引いて
//     解釈する (コイン投げの直感: 1,000 人に投げさせれば誰かは 9 回表を出す)。
//   - プラトー確認: 最良 combo の隣接 (1 次元 ±1 step) も勝っているかを報告する。
//     arm=16 だけ勝ち 15/17 で負けるなら暗記。
//
// retune はオフライン限定。勝者の live 投入は必ず新 config_id + N リセットで、
// 投入判断は人間 (このツールは config を書かない・DB に書かない)。
package main

import (
	"fmt"
	"sort"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/strategy"
)

// GridDim はスイープ 1 次元 (パラメータ名 + 試す値のリスト)。
type GridDim struct {
	Key    string    `yaml:"key"`
	Values []float64 `yaml:"values"`
}

// Combo はグリッドの 1 点。Idx は各次元の index (プラトー隣接判定に使う)。
type Combo struct {
	Params strategy.MAPullbackParams
	Idx    []int
	Label  string
}

// applyParam は snake_case キーで MAPullbackParams の 1 フィールドを設定する。
// int フィールドは値を int に切り詰める (グリッド YAML は float で書ける)。
func applyParam(p *strategy.MAPullbackParams, key string, v float64) error {
	switch key {
	case "trend_slope_lookback_1h":
		// backtest エンジンの履歴キャップ (maxHistoryBars=
		// 20000 1m bar ≈ 333 本の 1h) − 200SMA 期間 = 実効上限 ~133。超えると全 bar で
		// insufficient_1h_candles → N=0 cull になり「エッジ無し」と区別不能。
		// 負値/0 は ma_pullback の slice 演算 panic / 退化なので展開時に弾く。
		if v < 1 || v > 133 {
			return fmt.Errorf("trend_slope_lookback_1h=%g out of range [1,133] (backtest 履歴キャップ 333×1h − 200SMA)", v)
		}
		p.TrendSlopeLookback1h = int(v)
	case "trend_min_slope_pips":
		p.TrendMinSlopePips = v
	case "zone_atr":
		p.ZoneATR = v
	case "confluence_atr":
		p.ConfluenceATR = v
	case "swing_lookback":
		if v < 1 {
			return fmt.Errorf("swing_lookback=%g must be >= 1", v)
		}
		p.SwingLookback = int(v)
	case "swing_prom_pips":
		p.SwingPromPips = v
	case "sl_buffer_pips":
		p.SLBufferPips = v
	case "sl_min_pips":
		p.SLMinPips = v
	case "sl_max_pips":
		p.SLMaxPips = v
	case "tp_cap_pips":
		p.TPCapPips = v
	case "tp_floor_pips":
		p.TPFloorPips = v
	case "cluster_band_atr":
		p.ClusterBandATR = v
	case "ratchet_arm_pips":
		p.RatchetArmPips = v
	case "ratchet_giveback_pips":
		p.RatchetGivebackPips = v
	case "max_hold_minutes":
		if v < 1 {
			return fmt.Errorf("max_hold_minutes=%g must be >= 1", v)
		}
		p.MaxHoldMinutes = int(v)
	default:
		return fmt.Errorf("unknown sweep param %q (see strategy.MAPullbackParams)", key)
	}
	return nil
}

// ExpandGrid はグリッドの直積を展開する。展開数 = N_trials。
// グリッドに無いパラメータは DefaultMAPullbackParams (既定値) のまま。
func ExpandGrid(grid []GridDim) ([]Combo, error) {
	if len(grid) == 0 {
		return nil, fmt.Errorf("grid is empty — sweep には少なくとも 1 次元必要")
	}
	for _, d := range grid {
		if len(d.Values) == 0 {
			return nil, fmt.Errorf("grid dim %q has no values", d.Key)
		}
		// キーと**全値**を展開時に検証する (先頭値だけだと
		// 中間の不正値が数時間後の replay 中に panic / silent cull で発覚する)。
		for _, val := range d.Values {
			probe := strategy.DefaultMAPullbackParams()
			if err := applyParam(&probe, d.Key, val); err != nil {
				return nil, err
			}
		}
	}
	total := 1
	for _, d := range grid {
		total *= len(d.Values)
	}
	combos := make([]Combo, 0, total)
	idx := make([]int, len(grid))
	for {
		p := strategy.DefaultMAPullbackParams()
		label := ""
		for di, d := range grid {
			if err := applyParam(&p, d.Key, d.Values[idx[di]]); err != nil {
				return nil, err
			}
			if di > 0 {
				label += " "
			}
			label += fmt.Sprintf("%s=%g", d.Key, d.Values[idx[di]])
		}
		combos = append(combos, Combo{Params: p, Idx: append([]int(nil), idx...), Label: label})
		// 直積の次の index へ (最後の次元から繰り上げ)。
		di := len(grid) - 1
		for di >= 0 {
			idx[di]++
			if idx[di] < len(grid[di].Values) {
				break
			}
			idx[di] = 0
			di--
		}
		if di < 0 {
			return combos, nil
		}
	}
}

// NeighborsOf は「ちょうど 1 次元だけ index が ±1 違う」combo を返す —
// プラトー確認の隣接定義。
func NeighborsOf(c Combo, all []Combo) []Combo {
	var out []Combo
	for _, o := range all {
		diff, step := 0, 0
		for i := range c.Idx {
			if o.Idx[i] != c.Idx[i] {
				diff++
				step = o.Idx[i] - c.Idx[i]
			}
		}
		if diff == 1 && (step == 1 || step == -1) {
			out = append(out, o)
		}
	}
	return out
}

// CloneForOffline は frozen config をオフライン検証用に複製する。
// ⚠️ 既知の罠: frozen config をそのまま過去期間に回すと
// IsActive の valid_from 検査で**黙って 0 trades** になる。複製側の有効期間を
// 全 backfill を覆う固定値 (2000〜2100) へ広げ、symbol を上書きする。
// 元 config は一切変更しない (live の凍結値を絶対に触らない)。
func CloneForOffline(base *config.StrategyConfig, symbol string) *config.StrategyConfig {
	cl := *base
	cl.ValidFrom = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	cl.ValidUntil = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	cl.Symbol = symbol
	cl.Enabled = true
	return &cl
}

// Splits は train / OOS / holdout の 3 分割 (各区間 [From, To))。
// デフォルト (DefaultSplits): 学習 2023-11-01〜2025-03-31 / OOS 2025-04-01〜
// 2025-12-31 / holdout 2026-01-01〜。
type Splits struct {
	TrainFrom, TrainTo     time.Time
	OOSFrom, OOSTo         time.Time
	HoldoutFrom, HoldoutTo time.Time
}

// DefaultSplits は事前登録の 3 分割。holdoutTo だけ呼出時の「今」を渡す。
func DefaultSplits(now time.Time) Splits {
	return Splits{
		TrainFrom:   time.Date(2023, 11, 1, 0, 0, 0, 0, time.UTC),
		TrainTo:     time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC),
		OOSFrom:     time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC),
		OOSTo:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		HoldoutFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		HoldoutTo:   now,
	}
}

// Validate は区間の順序と非重複を強制する。重複を許すと「検証を見ながらの調整」
// が混入して実験無効。
func (s Splits) Validate() error {
	type span struct {
		name     string
		from, to time.Time
	}
	spans := []span{
		{"train", s.TrainFrom, s.TrainTo},
		{"oos", s.OOSFrom, s.OOSTo},
		{"holdout", s.HoldoutFrom, s.HoldoutTo},
	}
	for _, sp := range spans {
		if !sp.to.After(sp.from) {
			return fmt.Errorf("%s: to (%s) must be after from (%s)", sp.name, sp.to.Format("2006-01-02"), sp.from.Format("2006-01-02"))
		}
	}
	for i := 0; i < len(spans)-1; i++ {
		if spans[i+1].from.Before(spans[i].to) {
			return fmt.Errorf("%s [%s,%s) overlaps %s start %s — 区間重複は実験無効",
				spans[i].name, spans[i].from.Format("2006-01-02"), spans[i].to.Format("2006-01-02"),
				spans[i+1].name, spans[i+1].from.Format("2006-01-02"))
		}
	}
	return nil
}

// sortByExpectancyDesc は結果を期待値 (mean net) 降順に並べる。
func sortByExpectancyDesc(rs []comboResult) {
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Mean > rs[j].Mean })
}
