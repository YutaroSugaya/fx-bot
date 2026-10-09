package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type FloatRange struct {
	Min float64 `yaml:"min"`
	Max float64 `yaml:"max"`
}

func (r FloatRange) Contains(v float64) bool {
	return v >= r.Min && v <= r.Max
}

type IntRange struct {
	Min int `yaml:"min"`
	Max int `yaml:"max"`
}

func (r IntRange) Contains(v int) bool {
	return v >= r.Min && v <= r.Max
}

// StrategyLimit は戦略別の TP/SL 範囲を指定する。設定がある戦略では
// グローバルの TakeProfitPips / StopLossPips よりこちらが優先される
// (override semantics)。設定がない戦略はグローバル値にフォールバック。
type StrategyLimit struct {
	TakeProfitPips FloatRange `yaml:"take_profit_pips"`
	StopLossPips   FloatRange `yaml:"stop_loss_pips"`
}

// PaperConfig は Paper モードのフィル時に加算する execution cost。
// Live と Paper の PnL 乖離を縮めるためのシミュレーション値で、
// Live モードでは無視される。
//
// SimulatedSlippagePips: 注文時に adverse 方向 (BUY 約定なら ask + slip /
//
//	SELL 約定なら bid - slip)。決済も対称に。
//
// APIFeeJPYPerTrade:     決済時の ProfitLossJPY から控除。
type PaperConfig struct {
	SimulatedSlippagePips float64 `yaml:"simulated_slippage_pips"`
	APIFeeJPYPerTrade     float64 `yaml:"api_fee_jpy_per_trade"`
}

// CooldownConfig は直前 trade の決済後、次のエントリーまで待つ秒数。
// risk.Gate が ClosedAt を見て発火させる。
//
// 用途別 cooldown:
//
//	AfterEntrySeconds:      何 reason であれ前回決済直後の連打を防ぐ basic gate
//	AfterLossSeconds:       直前が損切りなら長めに休む (revenge trade 防止)
//	AfterTakeProfitSeconds: 直前が利確なら短めに休む (ノイズの戻り防止)
//
// すべて省略時 (0) は cooldown 無効。
type CooldownConfig struct {
	AfterEntrySeconds      int `yaml:"after_entry_seconds"`
	AfterLossSeconds       int `yaml:"after_loss_seconds"`
	AfterTakeProfitSeconds int `yaml:"after_take_profit_seconds"`
}

// OrderBoundary は実 Signal 値を発注直前に検査する安全網。
// validator は config の placeholder TP/SL しか見ないが、strategy が emit する
// 実 TP/SL/MaxHold/qty はここで検査する。nil = 検査無効 (back-compat / テスト)。
//
//	MaxLossPerTradeJPY: SL_pips × pipSize × qty × quote→JPY が超えたら reject
//	                    (qty スケール時の最悪損失を機械的に縛る本来の安全弁)。
//	MaxStopLossPips:    decimal typo / 異常 SL の sanity cap (runner SL≤20 は通す)。
//	MaxTakeProfitPips:  同上 (runner TP≤80 は通す)。
//	0 のフィールドはその検査を skip。
type OrderBoundary struct {
	MaxLossPerTradeJPY int     `yaml:"max_loss_per_trade_jpy"`
	MaxStopLossPips    float64 `yaml:"max_stop_loss_pips"`
	MaxTakeProfitPips  float64 `yaml:"max_take_profit_pips"`
}

type HardLimits struct {
	AllowedSymbols         []string   `yaml:"allowed_symbols"`
	Quantity               IntRange   `yaml:"quantity"`
	TakeProfitPips         FloatRange `yaml:"take_profit_pips"`
	StopLossPips           FloatRange `yaml:"stop_loss_pips"`
	MaxHoldMinutes         IntRange   `yaml:"max_hold_minutes"`
	MaxTradesInThisWindow  IntRange   `yaml:"max_trades_in_this_window"`
	MaxLossInThisWindowJPY IntRange   `yaml:"max_loss_in_this_window_jpy"`
	MaxSpreadPips          FloatRange `yaml:"max_spread_pips"`
	ConfigTTLMinutes       IntRange   `yaml:"config_ttl_minutes"`

	// StrategyLimits はオプショナル。キーは strategy.name の文字列値
	// (e.g. "momentum_pullback") に一致させること。
	StrategyLimits map[string]StrategyLimit `yaml:"strategy_limits"`

	// Paper は paper モード時のシミュレーション cost。nil なら 0 扱い。
	Paper *PaperConfig `yaml:"paper,omitempty"`

	// Cooldown は決済直後の連打防止。nil なら 0 = 無効。
	Cooldown *CooldownConfig `yaml:"cooldown,omitempty"`

	// OrderBoundary は発注境界での実 Signal 値検査。nil = 無効。
	// これは GLOBAL の sanity cap (intraday/scalp 想定で tight に保つ)。
	OrderBoundary *OrderBoundary `yaml:"order_boundary,omitempty"`

	// StrategyOrderBoundaries は戦略別の発注境界 override。キーは strategy.name。
	// エントリーがあればその戦略はグローバル OrderBoundary ではなくこちらで検査される
	// (全フィールド置換)。日足 signature_breakout は構造 SL 60-150 / measured-move TP が大きいので
	// 専用の広い sanity cap を持たせ、intraday 戦略のグローバル fat-finger 網は tight に保つ。
	// per-trade JPY 損失 cap は各 override にも明示すること (capital control は据え置き)。
	StrategyOrderBoundaries map[string]*OrderBoundary `yaml:"strategy_order_boundary,omitempty"`
}

// OrderBoundaryFor は指定戦略に適用すべき OrderBoundary を返す。strategy_order_boundary に
// エントリーがあればそれを、無ければグローバル OrderBoundary を返す。strategy="" や h==nil は
// グローバルにフォールバック。
func (h *HardLimits) OrderBoundaryFor(strategy string) *OrderBoundary {
	if h == nil {
		return nil
	}
	if strategy != "" && h.StrategyOrderBoundaries != nil {
		if ob, ok := h.StrategyOrderBoundaries[strategy]; ok && ob != nil {
			return ob
		}
	}
	return h.OrderBoundary
}

// LimitsFor は指定された戦略の TP/SL 範囲を返す。strategy_limits に
// エントリーがあればそれを、なければ nil を返す。呼び出し側は nil の場合
// グローバル TakeProfitPips/StopLossPips にフォールバックする。
func (h *HardLimits) LimitsFor(strategy string) *StrategyLimit {
	if h == nil || h.StrategyLimits == nil || strategy == "" {
		return nil
	}
	if sl, ok := h.StrategyLimits[strategy]; ok {
		return &sl
	}
	return nil
}

type hardLimitsFile struct {
	HardLimits HardLimits `yaml:"hard_limits"`
}

func LoadHardLimits(path string) (*HardLimits, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read hard_limits: %w", err)
	}
	var raw hardLimitsFile
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse hard_limits: %w", err)
	}
	if err := raw.HardLimits.validate(); err != nil {
		return nil, fmt.Errorf("validate hard_limits: %w", err)
	}
	return &raw.HardLimits, nil
}

func (h *HardLimits) validate() error {
	if len(h.AllowedSymbols) == 0 {
		return fmt.Errorf("allowed_symbols must contain at least one symbol")
	}
	checks := []struct {
		name string
		min  float64
		max  float64
	}{
		{"quantity", float64(h.Quantity.Min), float64(h.Quantity.Max)},
		{"take_profit_pips", h.TakeProfitPips.Min, h.TakeProfitPips.Max},
		{"stop_loss_pips", h.StopLossPips.Min, h.StopLossPips.Max},
		{"max_hold_minutes", float64(h.MaxHoldMinutes.Min), float64(h.MaxHoldMinutes.Max)},
		{"max_trades_in_this_window", float64(h.MaxTradesInThisWindow.Min), float64(h.MaxTradesInThisWindow.Max)},
		{"max_loss_in_this_window_jpy", float64(h.MaxLossInThisWindowJPY.Min), float64(h.MaxLossInThisWindowJPY.Max)},
		{"max_spread_pips", h.MaxSpreadPips.Min, h.MaxSpreadPips.Max},
		{"config_ttl_minutes", float64(h.ConfigTTLMinutes.Min), float64(h.ConfigTTLMinutes.Max)},
	}
	for _, c := range checks {
		if c.min > c.max {
			return fmt.Errorf("%s: min(%v) > max(%v)", c.name, c.min, c.max)
		}
		if c.min < 0 {
			return fmt.Errorf("%s: min(%v) must be >= 0", c.name, c.min)
		}
	}
	// Paper セクションは optional。設定があれば値が >= 0 であることだけチェック。
	if h.Paper != nil {
		if h.Paper.SimulatedSlippagePips < 0 {
			return fmt.Errorf("paper.simulated_slippage_pips: %v must be >= 0", h.Paper.SimulatedSlippagePips)
		}
		if h.Paper.APIFeeJPYPerTrade < 0 {
			return fmt.Errorf("paper.api_fee_jpy_per_trade: %v must be >= 0", h.Paper.APIFeeJPYPerTrade)
		}
	}
	// Cooldown セクションも optional。負値だけ reject。
	if h.Cooldown != nil {
		cdChecks := []struct {
			field string
			v     int
		}{
			{"after_entry_seconds", h.Cooldown.AfterEntrySeconds},
			{"after_loss_seconds", h.Cooldown.AfterLossSeconds},
			{"after_take_profit_seconds", h.Cooldown.AfterTakeProfitSeconds},
		}
		for _, c := range cdChecks {
			if c.v < 0 {
				return fmt.Errorf("cooldown.%s: %d must be >= 0", c.field, c.v)
			}
		}
	}
	// OrderBoundary は optional。負値だけ reject。
	if h.OrderBoundary != nil {
		obChecks := []struct {
			field string
			v     float64
		}{
			{"max_loss_per_trade_jpy", float64(h.OrderBoundary.MaxLossPerTradeJPY)},
			{"max_stop_loss_pips", h.OrderBoundary.MaxStopLossPips},
			{"max_take_profit_pips", h.OrderBoundary.MaxTakeProfitPips},
		}
		for _, c := range obChecks {
			if c.v < 0 {
				return fmt.Errorf("order_boundary.%s: %v must be >= 0", c.field, c.v)
			}
		}
	}
	// 戦略別 OrderBoundary override も負値だけ reject。
	for name, ob := range h.StrategyOrderBoundaries {
		if ob == nil {
			continue
		}
		obChecks := []struct {
			field string
			v     float64
		}{
			{"max_loss_per_trade_jpy", float64(ob.MaxLossPerTradeJPY)},
			{"max_stop_loss_pips", ob.MaxStopLossPips},
			{"max_take_profit_pips", ob.MaxTakeProfitPips},
		}
		for _, c := range obChecks {
			if c.v < 0 {
				return fmt.Errorf("strategy_order_boundary[%s].%s: %v must be >= 0", name, c.field, c.v)
			}
		}
	}
	// 戦略別 TP/SL レンジも整合性チェック (min<=max, min>=0)。
	for name, sl := range h.StrategyLimits {
		pairs := []struct {
			field string
			r     FloatRange
		}{
			{"take_profit_pips", sl.TakeProfitPips},
			{"stop_loss_pips", sl.StopLossPips},
		}
		for _, p := range pairs {
			if p.r.Min > p.r.Max {
				return fmt.Errorf("strategy_limits[%s].%s: min(%v) > max(%v)", name, p.field, p.r.Min, p.r.Max)
			}
			if p.r.Min < 0 {
				return fmt.Errorf("strategy_limits[%s].%s: min(%v) must be >= 0", name, p.field, p.r.Min)
			}
		}
	}
	return nil
}

// SymbolAllowed returns true if symbol is in AllowedSymbols.
func (h *HardLimits) SymbolAllowed(symbol string) bool {
	for _, s := range h.AllowedSymbols {
		if s == symbol {
			return true
		}
	}
	return false
}
