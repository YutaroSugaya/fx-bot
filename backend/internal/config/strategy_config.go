package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// StrategyName enumerates the legal strategy.name values for day trading.
type StrategyName string

const (
	StrategyMomentumPullback   StrategyName = "momentum_pullback"
	StrategyBreakoutFollow     StrategyName = "breakout_follow"
	StrategyRangeBreakoutProbe StrategyName = "range_breakout_probe"
	StrategyMTFPullback        StrategyName = "mtf_pullback"
	StrategyMAPullback         StrategyName = "ma_pullback"        // 200MA push/pull (modelled on a published discretionary method)
	StrategyMAPullbackV2       StrategyName = "ma_pullback_v2"     // 200EMA trend + higher-low + fixed 2R OCO exit
	StrategyGotobiFix          StrategyName = "gotobi_fix"         // probe: USD/JPY 09:00 buy → 09:55 Tokyo fix (importer flow)
	StrategyLondonBreakout     StrategyName = "london_breakout"    // probe: Tokyo-range break at the London open (session vol expansion)
	StrategyTrendFollow        StrategyName = "trend_follow"       // probe: 1h 200SMA trend + Donchian-48 continuation, ATR ratchet (time-series momentum)
	StrategyDailyTrend         StrategyName = "daily_trend"        // probe: DAILY 200SMA trend + Donchian-55, ATR ratchet (low-freq, confound-free EUR/USD+GBP/USD)
	StrategySignatureBreakout  StrategyName = "signature_breakout" // advisor v2: daily with-trend breakout (classic breakout), LLM go/no-go gate, ratchet exit
	StrategyLLMDecision        StrategyName = "llm_decision"       // autonomous LLM chooses side+TP/SL each cycle (qty 1000), downstream of risk Gate + broker OCO
	StrategyExhaustionFade     StrategyName = "exhaustion_fade"    // counter-trend: fade an exhausted overshoot at a level (オーバーシュート逆張り), structural SL/TP — backtest-first probe
	StrategyNoTrade            StrategyName = "no_trade"
)

func (s StrategyName) Valid() bool {
	switch s {
	case StrategyMomentumPullback, StrategyBreakoutFollow, StrategyRangeBreakoutProbe, StrategyMTFPullback, StrategyMAPullback, StrategyMAPullbackV2, StrategyGotobiFix, StrategyLondonBreakout, StrategyTrendFollow, StrategyDailyTrend, StrategySignatureBreakout, StrategyLLMDecision, StrategyExhaustionFade, StrategyNoTrade:
		return true
	}
	return false
}

type Direction string

const (
	DirectionBuyOnly  Direction = "buy_only"
	DirectionSellOnly Direction = "sell_only"
	DirectionBoth     Direction = "both"
	DirectionNone     Direction = "none"
)

func (d Direction) Valid() bool {
	switch d {
	case DirectionBuyOnly, DirectionSellOnly, DirectionBoth, DirectionNone:
		return true
	}
	return false
}

type MarketRegimeType string

const (
	RegimeRange     MarketRegimeType = "range"
	RegimeTrendUp   MarketRegimeType = "trend_up"
	RegimeTrendDown MarketRegimeType = "trend_down"
	RegimeVolatile  MarketRegimeType = "volatile"
	RegimeUnclear   MarketRegimeType = "unclear"
)

func (r MarketRegimeType) Valid() bool {
	switch r {
	case RegimeRange, RegimeTrendUp, RegimeTrendDown, RegimeVolatile, RegimeUnclear:
		return true
	}
	return false
}

type MarketRegime struct {
	Type       MarketRegimeType `yaml:"type"`
	Confidence float64          `yaml:"confidence"`
	Reason     string           `yaml:"reason"`
}

type StrategySection struct {
	Name StrategyName `yaml:"name"`
	// timeframe / trend_timeframe は廃止。strategy は読まずに
	// Summary バケット (5m/1h/6h/24h) をハードコードで使うため dead だった。
	// advisor 出力にキーが残っても yaml.Unmarshal が無視する (非 strict)。
}

type EntrySection struct {
	MaxSpreadPips float64 `yaml:"max_spread_pips"`
	// min/max_volatility_pips_5m は廃止。entry filter として
	// 設計されたが production reader が無く dead だった。
	RequireBreakout bool      `yaml:"require_breakout"`
	Direction       Direction `yaml:"direction"`

	// AllowedHoursJST restricts entries to the given hours-of-day (JST, 0-23).
	// Empty (nil or len 0) = all hours allowed (back-compat).
	// Lets a config restrict entries to specific JST hours (e.g. from a
	// per-hour backtest breakdown) without touching strategy code.
	AllowedHoursJST []int `yaml:"allowed_hours_jst,omitempty"`

	// 追いかけ (chasing) 防止フィルタ。momentum_pullback
	// 専用。「もう走り切った後の順張り」= 直近の値動きベースから離れすぎた所での
	// エントリーを抑止する。breakout_follow には適用しない (breakout は仕様上
	// 拡張方向に入る戦略なので追いかけ防止と矛盾する)。
	//
	// 判定: side 確定後、直近 ChaseLookbackCandles 本の 5m から「ベース」を取り
	//   - BUY:  base = min(Low)、extension = (entry - base) / pip
	//   - SELL: base = max(High)、extension = (base - entry) / pip
	// extension > MaxChasePips なら entry を見送り ("chasing_extended")。
	//
	// 両方 > 0 で有効。どちらか 0 = 無効 (back-compat: 既存 config はフィルタ無し)。
	// 例: lookback=12 (=60分), max_chase=12pips。
	MaxChasePips         float64 `yaml:"max_chase_pips,omitempty"`
	ChaseLookbackCandles int     `yaml:"chase_lookback_candles,omitempty"`

	// HTFEfficiencyMax is the higher-timeframe trend-regime kill-switch used by
	// exhaustion_fade (counter-trend): skip the fade when the 1h Kaufman
	// efficiency ratio (over a fixed 24-bar window) is >= this value — i.e. the
	// bigger picture is a clean directional trend that would run a fade over
	// ("don't fade a strong trend"). 0 / omitted = OFF (back-compat; other
	// strategies ignore it). Logic-anchored value ≈0.40; not swept.
	HTFEfficiencyMax float64 `yaml:"htf_efficiency_max,omitempty"`

	// exhaustion_fade refinements (all 0/false = OFF, back-compat; wired into the
	// detector by the ExhaustionFade wrapper, calibrated by backtest). The live
	// probe leaves them unset so its behaviour is unchanged until a value is
	// proven by backtest.

	// HTFTrendVetoPips is the MTF DIRECTIONAL veto: block a fade that OPPOSES a
	// 1h trend whose net move over a fixed 24-bar window reaches this many pips,
	// while ALLOWING a trend-aligned fade (unlike HTFEfficiencyMax which is
	// direction-agnostic). 0 = off.
	HTFTrendVetoPips float64 `yaml:"htf_trend_veto_pips,omitempty"`

	// MinRunEfficiency is the band-pass FLOOR: the run window must be a clean
	// directional push (Kaufman ER >= this) — fade a sharp spike, not a choppy
	// drift. Pairs with HTFEfficiencyMax (ceiling) to band-pass the regime. 0 = off.
	MinRunEfficiency float64 `yaml:"min_run_efficiency,omitempty"`

	// RequireRSIDivergence demands RSI regular divergence at the spike (the
	// oscillator refusing to confirm the new extreme) as a second exhaustion
	// confirmation alongside the rejection wick. false = off.
	RequireRSIDivergence bool `yaml:"require_rsi_divergence,omitempty"`

	// MinEntryATRPips is the VOLATILITY FLOOR entry gate:
	// suppress an entry whose 1h ATR is below this many pips — i.e. "only trade when
	// there is enough volatility / a real trend, not in a low-vol range that whipsaws".
	// 0 / omitted = OFF (back-compat; live configs leave it unset so behaviour is
	// unchanged). Wired into trend_follow. Calibrated by the backtest harness: the
	// floor is swept ONLY on in-sample and validated FIXED on OOS — never tuned on live.
	MinEntryATRPips float64 `yaml:"min_entry_atr_pips,omitempty"`
}

// IsHourAllowed reports whether the given time t (any zone) falls inside the
// AllowedHoursJST whitelist. Empty whitelist always returns true. Conversion
// to JST happens here so callers pass time.Now() in any zone.
func (e EntrySection) IsHourAllowed(t time.Time) bool {
	if len(e.AllowedHoursJST) == 0 {
		return true
	}
	h := t.In(jstZone).Hour()
	for _, allowed := range e.AllowedHoursJST {
		if allowed == h {
			return true
		}
	}
	return false
}

// jstZone is the fixed +09:00 zone used to bucket "allowed hours" against the
// trading session in Japan time. FX runs 24h Mon-Fri so the bucket only
// affects intraday filtering, not weekend gating.
var jstZone = time.FixedZone("JST", 9*60*60)

// ExitPolicy declares who owns a config's exit values.
//   - config_driven: TP/SL/MaxHold/ratchet in the config ARE the live exits
//     (advisor configs). Validator enforces TP/SL ranges, RR-floor, TTL window.
//   - strategy_computed: the strategy computes its real exits from constants at
//     entry time (ma_pullback / mtf_pullback). The config's tp/sl are validator
//     placeholders, the window is permanent. Validator skips TP/SL range +
//     RR-floor, exempts TTL, and requires max_trades=0.
//
// Empty ("") is derived from the strategy name (see IsStrategyComputedExit) so
// existing strategy_computed configs (no exit_policy field) classify correctly
// without a re-seed — backward compatible.
type ExitPolicy string

const (
	ExitPolicyConfigDriven     ExitPolicy = "config_driven"
	ExitPolicyStrategyComputed ExitPolicy = "strategy_computed"
)

type ExitSection struct {
	TakeProfitPips float64 `yaml:"take_profit_pips"`
	StopLossPips   float64 `yaml:"stop_loss_pips"`
	MaxHoldMinutes int     `yaml:"max_hold_minutes"`

	// ExitPolicy is an OPTIONAL override of the strategy-derived default.
	// Omit it and it is derived from strategy.name.
	ExitPolicy ExitPolicy `yaml:"exit_policy,omitempty"`

	// ExtensionMaxMinutes lets a position run past MaxHoldMinutes by up to
	// this many extra minutes IF the position is still flat (see
	// ExtensionUnrealizedPipsThreshold). Default 0 = no extension (force
	// close at MaxHold). Hard cap on total hold time = MaxHold + Extension.
	//
	// Rationale: markets do stall — when an entry hasn't moved either way
	// after 4h, force-closing
	// at break-even ± spread is wasteful. Give it a measured grace window
	// while volatility is genuinely absent, but no longer.
	ExtensionMaxMinutes int `yaml:"extension_max_minutes,omitempty"`

	// ExtensionUnrealizedPipsThreshold defines what "still flat" means.
	// At MaxHold time, if |unrealized_pips| ≤ this value, the bot waits
	// (up to ExtensionMaxMinutes). If unrealized has moved beyond it
	// (either direction), force-close as before. 0 = disabled.
	ExtensionUnrealizedPipsThreshold float64 `yaml:"extension_unrealized_pips_threshold,omitempty"`

	// EarlyExitWindowMinutes / EarlyExitTargetPips form a "least bad" early
	// close policy: in the final EarlyExitWindowMinutes BEFORE the soft
	// MaxHold deadline, close as soon as unrealized_pips >= EarlyExitTargetPips.
	// Caps the worst-case downside that a fixed deadline force-close can
	// lock in.
	//
	// Default 0 / 0 = feature disabled (back-compat). Recommended starter
	// values: window=30 minutes, target=-2 pips (= close at break-even ± a
	// small loss budget when the window opens).
	EarlyExitWindowMinutes int     `yaml:"early_exit_window_minutes,omitempty"`
	EarlyExitTargetPips    float64 `yaml:"early_exit_target_pips,omitempty"`

	// RatchetArmPips / RatchetGivebackPips form a trailing take-profit
	// policy ("ratchet TP"). OnTick の度に未実現損益の peak を更新し、peak
	// が RatchetArmPips 以上に達したら ratchet を armed にする。armed 後、
	// peak から RatchetGivebackPips だけ戻ったら MARKET close する。
	//
	// 例: arm=5, give=3, peak=11pips → 8pips に下落 → exit @ +8pips。
	// 値が伸びている間は peak を更新し続けるので、ガンガン伸びるなら TP
	// まで放置。ratchet で確定するのはあくまで「peak から retrace した」場合。
	//
	// Live でも有効。発火時は既存の ExecuteCloseSaga (cancel→MARKET) を
	// 再利用するので broker_port に追加 API は不要。
	//
	// Default 0 / 0 = feature disabled (back-compat)。advisor (tpsl-designer
	// subagent) が市況に応じて値を埋める。手動 fallback は無し。
	// 有効値の制約 (Promoter で検証): arm >= 3, give >= 1, arm > give。
	RatchetArmPips      float64 `yaml:"ratchet_arm_pips,omitempty"`
	RatchetGivebackPips float64 `yaml:"ratchet_giveback_pips,omitempty"`

	// exhaustion_fade round-number-aware TP (all 0/false = OFF, back-compat;
	// wired by the ExhaustionFade wrapper). When enabled, the take-profit is
	// capped JUST BEFORE the nearest round level in the profit direction (orders
	// cluster there, price stalls); it only ever TIGHTENS the revert give-back.
	RoundNumberTP           bool    `yaml:"round_number_tp,omitempty"`
	RoundNumberTPStepPips   float64 `yaml:"round_number_tp_step_pips,omitempty"`   // round grid for the TP, in pips (e.g. 10 = .x0 minor levels); falls back to the detector level grid when 0
	RoundNumberTPOffsetPips float64 `yaml:"round_number_tp_offset_pips,omitempty"` // exit this many pips before the round level
	// MinTakeProfitPips overrides the detector's cost-floor TP reject (research:
	// >= ~8 pips so the fade clears the ~1.1-pip round-trip floor). 0 = use the
	// detector default.
	MinTakeProfitPips float64 `yaml:"min_take_profit_pips,omitempty"`
}

type ConfigRiskSection struct {
	Quantity               int `yaml:"quantity"`
	MaxOpenPositions       int `yaml:"max_open_positions"`
	MaxTradesInThisWindow  int `yaml:"max_trades_in_this_window"`
	MaxLossInThisWindowJPY int `yaml:"max_loss_in_this_window_jpy"`
}

type NoTradeSection struct {
	Enabled bool   `yaml:"enabled"`
	Reason  string `yaml:"reason"`
}

// StrategyConfig is the YAML produced by Claude every hour and consumed by the
// Go bot to decide trading behaviour for the next window.
type StrategyConfig struct {
	ConfigID     string            `yaml:"config_id"`
	GeneratedAt  time.Time         `yaml:"generated_at"`
	ValidFrom    time.Time         `yaml:"valid_from"`
	ValidUntil   time.Time         `yaml:"valid_until"`
	Symbol       string            `yaml:"symbol"`
	Enabled      bool              `yaml:"enabled"`
	MarketRegime MarketRegime      `yaml:"market_regime"`
	Strategy     StrategySection   `yaml:"strategy"`
	Entry        EntrySection      `yaml:"entry"`
	Exit         ExitSection       `yaml:"exit"`
	Risk         ConfigRiskSection `yaml:"risk"`
	NoTrade      NoTradeSection    `yaml:"no_trade"`

	// NextAdvisorRunInMinutes は次に Claude advisor を再評価するまでの分数。
	// Claude が skill 06 (recheck cadence) を参照して 10-480 の範囲 (validateAdvisorInterval) で決める。
	// auto interval (= bot_config.ai_advisor.interval_minutes) より短い値は
	// scheduler が "event" として短縮 fire (= 経済指標帯など)、長い値は cooldown 延長。
	// 0 / 未指定 は scheduler 側で default (auto interval) にフォールバック。
	// emergency_stop は別系統 (runtime/emergency_stop.flag) で制御するため、ここに
	// 0 を入れても fire は止まらない。
	NextAdvisorRunInMinutes int `yaml:"next_advisor_run_in_minutes,omitempty"`
}

// IsActive reports whether t falls within [ValidFrom, ValidUntil) and
// the config is not in no_trade / disabled state.
func (c *StrategyConfig) IsActive(t time.Time) bool {
	if !c.Enabled {
		return false
	}
	if t.Before(c.ValidFrom) || !t.Before(c.ValidUntil) {
		return false
	}
	if c.NoTrade.Enabled {
		return false
	}
	if c.Strategy.Name == StrategyNoTrade {
		return false
	}
	return true
}

// IsStrategyComputedExit reports whether the strategy (not the config) owns the
// live exit values. An explicit exit_policy wins; otherwise it
// is derived from the strategy name: ma_pullback / mtf_pullback compute their
// TP/SL/MaxHold/ratchet from constants at entry time, so their config tp/sl are
// placeholders. This drives the validator's TTL / TP-SL-range / RR-floor /
// max_trades exemptions. Backward compatible: configs with no exit_policy
// field still classify as strategy_computed via their strategy name.
func (c *StrategyConfig) IsStrategyComputedExit() bool {
	if c == nil {
		return false
	}
	switch c.Exit.ExitPolicy {
	case ExitPolicyStrategyComputed:
		return true
	case ExitPolicyConfigDriven:
		return false
	}
	return c.Strategy.Name == StrategyMAPullback || c.Strategy.Name == StrategyMAPullbackV2 ||
		c.Strategy.Name == StrategyMTFPullback || c.Strategy.Name == StrategyGotobiFix ||
		c.Strategy.Name == StrategyLondonBreakout || c.Strategy.Name == StrategyTrendFollow ||
		c.Strategy.Name == StrategyDailyTrend || c.Strategy.Name == StrategyExhaustionFade
}

// IsNoTradeDecision reports whether this config represents a "do not trade"
// decision. Three independent signals each imply it: an explicit no_trade block,
// strategy.name == no_trade, or enabled == false. ValidateSchema's no_trade
// consistency rules and CanonicalizeNoTrade both key off this single predicate
// so they can never disagree about whether a config is no_trade.
func (c *StrategyConfig) IsNoTradeDecision() bool {
	if c == nil {
		return false
	}
	return c.NoTrade.Enabled || c.Strategy.Name == StrategyNoTrade || !c.Enabled
}

// CanonicalizeNoTrade zeroes the entry/exit/risk fields that carry no meaning for
// a no_trade decision, so a config Claude clearly intended as no_trade is not
// rejected over a single leftover field.
//
// Rationale: Claude can emit a textbook no_trade config — enabled=false,
// strategy.name=no_trade, no_trade.enabled=true, all exit/risk zeroed — but
// leave entry.direction at "both". Without canonicalization ValidateSchema's
// no_trade consistency block rejects it, the previous config stays active past
// its valid_until, and the advisor's fresh judgment is discarded (repeatedly,
// freezing the dashboard at the last good promote). A no_trade config
// never places an order, so these fields are inert; canonicalizing them is
// loss-free and keeps the Validator strict as a safety net. This mirrors the
// existing prompt-parser salvage philosophy: don't throw away a clear Claude
// judgment over a cosmetic inconsistency.
//
// No-op for genuine enabled trading configs (IsNoTradeDecision == false).
func (c *StrategyConfig) CanonicalizeNoTrade() {
	if c == nil || !c.IsNoTradeDecision() {
		return
	}
	c.Strategy.Name = StrategyNoTrade
	c.Entry.Direction = DirectionNone
	c.Risk.Quantity = 0
	// Zero the whole exit policy: not just the tp/sl/max_hold/ratchet fields the
	// no_trade block checks, but also early_exit / extension, whose own schema
	// rules (e.g. early_exit_window ≤ max_hold) would otherwise re-reject once
	// max_hold is forced to 0. No position opens under no_trade, so the entire
	// exit policy is inert.
	c.Exit = ExitSection{}
}

// ParseStrategyConfig parses the YAML bytes into a StrategyConfig without
// applying hard limit or risk checks (that's Validator's job).
func ParseStrategyConfig(data []byte) (*StrategyConfig, error) {
	var c StrategyConfig
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("strategy_config yaml: %w", err)
	}
	return &c, nil
}
