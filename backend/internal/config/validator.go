package config

import (
	"fmt"
	"strings"
	"time"
)

// ValidationType categorises a single validation step.
type ValidationType string

const (
	ValidationSchema    ValidationType = "schema"
	ValidationHardLimit ValidationType = "hard_limit"
	ValidationSemantic  ValidationType = "semantic"
	ValidationRisk      ValidationType = "risk"
)

// lowConfidenceFloor は低確度 trade を弾く Go-side gate。
// skill 02 で「confidence < 0.35 で no_trade」と書いてあるが、Claude が
// 違反したときの sanity check として enabled config をこの閾値未満で reject する。
// 値の根拠は skill 02 (35% threshold) と完全一致。skill 緩和時はここも同期。
const lowConfidenceFloor = 0.35

// ratchetMinLockedFracOfTP は ratchet が確保すべき最小含み益を take_profit_pips の
// 割合で表す (RR floor)。ratchet は本来 TP 手前のトレーリング (skill 03) なので、
// 利確するなら最低 TP×この割合 の含み益を確保してから。
// 狙い: arm5/give3 → locked 2pips 利確 / SL 15-20pips = 実効RR≈0.2 のような
// 逆RRを撲滅する。0.5 → 0.65 に上げれば worst-case 実効RR≈1.0 を強制できる
// (skill 03 の ratchet 推奨表と同期して調整すること)。
const ratchetMinLockedFracOfTP = 0.5

// ValidationError is a single failure reason produced during validation.
type ValidationError struct {
	Type    ValidationType
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("[%s] %s", e.Type, e.Message)
	}
	return fmt.Sprintf("[%s] %s: %s", e.Type, e.Field, e.Message)
}

// ValidationResult collects errors from one or more validation passes.
type ValidationResult struct {
	Errors []ValidationError
}

func (r *ValidationResult) OK() bool              { return len(r.Errors) == 0 }
func (r *ValidationResult) add(e ValidationError) { r.Errors = append(r.Errors, e) }

func (r *ValidationResult) Summary() string {
	if r.OK() {
		return "ok"
	}
	parts := make([]string, 0, len(r.Errors))
	for _, e := range r.Errors {
		parts = append(parts, e.Error())
	}
	return strings.Join(parts, "; ")
}

// AccountState captures the runtime state needed for risk validation.
type AccountState struct {
	EmergencyStop         bool
	DailyLossJPY          int
	MaxDailyLossJPY       int
	ConsecutiveLosses     int
	MaxConsecutiveLosses  int
	UnknownPositionExists bool
	UnresolvedOrderExists bool
	OpenPositions         int
	MaxOpenPositions      int
}

// Validator performs schema / hard-limit / risk validation on a StrategyConfig.
type Validator struct {
	Limits *HardLimits
	Now    func() time.Time
}

func NewValidator(limits *HardLimits) *Validator {
	return &Validator{Limits: limits, Now: time.Now}
}

// ValidateSchema checks that mandatory structural fields are present and
// internally consistent.
func (v *Validator) ValidateSchema(c *StrategyConfig) *ValidationResult {
	r := &ValidationResult{}

	if c == nil {
		r.add(ValidationError{Type: ValidationSchema, Message: "config is nil"})
		return r
	}

	validateRequiredFields(c, r)
	validateAdvisorInterval(c, r)
	validateExtensionBounds(c, r)
	validateEarlyExitBounds(c, r)
	validateRatchetBounds(c, r)
	validateAllowedHours(c, r)
	validateChaseFilter(c, r)

	// no_trade consistency: enabled (= entry) configs get the entry guards (confidence
	// floor / early_exit-or-ratchet / trailing TP required / RR floor); no_trade configs get
	// the strict 0/none consistency checks. Mutually exclusive — only one branch runs.
	if c.IsNoTradeDecision() {
		validateNoTradeConsistency(c, r)
	} else {
		validateEnabledConfigGuards(c, r)
	}
	return r
}

func validateRequiredFields(c *StrategyConfig, r *ValidationResult) {
	if c.ConfigID == "" {
		r.add(ValidationError{Type: ValidationSchema, Field: "config_id", Message: "required"})
	}
	if c.Symbol == "" {
		r.add(ValidationError{Type: ValidationSchema, Field: "symbol", Message: "required"})
	}
	if c.ValidFrom.IsZero() {
		r.add(ValidationError{Type: ValidationSchema, Field: "valid_from", Message: "required"})
	}
	if c.ValidUntil.IsZero() {
		r.add(ValidationError{Type: ValidationSchema, Field: "valid_until", Message: "required"})
	}
	if !c.ValidFrom.IsZero() && !c.ValidUntil.IsZero() && !c.ValidUntil.After(c.ValidFrom) {
		r.add(ValidationError{Type: ValidationSchema, Field: "valid_until", Message: "must be after valid_from"})
	}
	if !c.MarketRegime.Type.Valid() {
		r.add(ValidationError{Type: ValidationSchema, Field: "market_regime.type", Message: fmt.Sprintf("invalid: %q", c.MarketRegime.Type)})
	}
	if !c.Strategy.Name.Valid() {
		r.add(ValidationError{Type: ValidationSchema, Field: "strategy.name", Message: fmt.Sprintf("invalid: %q", c.Strategy.Name)})
	}
	if !c.Entry.Direction.Valid() {
		r.add(ValidationError{Type: ValidationSchema, Field: "entry.direction", Message: fmt.Sprintf("invalid: %q", c.Entry.Direction)})
	}
}

// validateAdvisorInterval: next_advisor_run_in_minutes は任意フィールド。値が
// あれば 10-480 または 0 (fallback)。0 / 未指定 は scheduler 側でデフォルト
// (auto) にフォールバック。default より短い値は "event" として scheduler が
// 短縮 fire する。下限 10: 急変・経済指標・
// volatile 帯で 10 分間隔の高頻度再評価を許可するため。60 以上は長期 cooldown
// 用 (連敗後など)。
func validateAdvisorInterval(c *StrategyConfig, r *ValidationResult) {
	if c.NextAdvisorRunInMinutes != 0 {
		if c.NextAdvisorRunInMinutes < 10 || c.NextAdvisorRunInMinutes > 480 {
			r.add(ValidationError{
				Type: ValidationSchema, Field: "next_advisor_run_in_minutes",
				Message: fmt.Sprintf("must be 0 or 10..480, got %d", c.NextAdvisorRunInMinutes),
			})
		}
	}
}

// validateExtensionBounds: MaxHold extension policy bounds. Both default
// 0 = disabled. When set, extension_max_minutes must be 1..240 (= up to 4h
// grace) and threshold must be > 0 to be meaningful. Validate so a typo can't
// unlock unbounded hold time.
func validateExtensionBounds(c *StrategyConfig, r *ValidationResult) {
	if c.Exit.ExtensionMaxMinutes < 0 {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.extension_max_minutes",
			Message: "must be ≥ 0"})
	}
	if c.Exit.ExtensionMaxMinutes > 240 {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.extension_max_minutes",
			Message: fmt.Sprintf("%d exceeds 240 (= 4h grace cap)", c.Exit.ExtensionMaxMinutes)})
	}
	if c.Exit.ExtensionUnrealizedPipsThreshold < 0 {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.extension_unrealized_pips_threshold",
			Message: "must be ≥ 0"})
	}
	if c.Exit.ExtensionMaxMinutes > 0 && c.Exit.ExtensionUnrealizedPipsThreshold == 0 {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.extension_unrealized_pips_threshold",
			Message: "must be > 0 when extension_max_minutes > 0 (otherwise extension never triggers)"})
	}
}

// validateEarlyExitBounds: Early-exit window bounds. Default 0 =
// disabled. When set, window must be 0..240 and ≤ max_hold_minutes (otherwise
// window starts before opening = fires immediately, defeating the deadline
// semantic). target_pips is a sanity range (typo guard): ±100 pips covers any
// reasonable target while blocking misplaced decimals (e.g. -200 = -2 yen).
func validateEarlyExitBounds(c *StrategyConfig, r *ValidationResult) {
	if c.Exit.EarlyExitWindowMinutes < 0 {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.early_exit_window_minutes",
			Message: "must be ≥ 0"})
	}
	if c.Exit.EarlyExitWindowMinutes > 240 {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.early_exit_window_minutes",
			Message: fmt.Sprintf("%d exceeds 240 (= 4h cap)", c.Exit.EarlyExitWindowMinutes)})
	}
	if c.Exit.EarlyExitWindowMinutes > c.Exit.MaxHoldMinutes {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.early_exit_window_minutes",
			Message: fmt.Sprintf("%d exceeds max_hold_minutes %d (window cannot start before position opens)",
				c.Exit.EarlyExitWindowMinutes, c.Exit.MaxHoldMinutes)})
	}
	if c.Exit.EarlyExitTargetPips < -100 || c.Exit.EarlyExitTargetPips > 100 {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.early_exit_target_pips",
			Message: fmt.Sprintf("%.2f outside [-100, 100] (sanity range; suggests a decimal typo)",
				c.Exit.EarlyExitTargetPips)})
	}
}

// validateRatchetBounds: Ratchet TP (trailing take-profit). Either both > 0
// (enabled) or both == 0 (disabled). Partial config is rejected to prevent
// advisor from silently degrading to "no buffer" behavior.
//
// 制約:
//   - arm >= 3: micro-noise / spike で即発火するのを防ぐ
//   - give >= 1: 1pip 未満の retrace で発火させない
//   - arm > give: 「peak 到達」より「peak からの戻り」が小さくないと
//     ratchet が即発火して意味がない
//   - arm <= 50 / give <= 50: 50pips 超は本来 TP 領域。typo / decimal 誤り検知
func validateRatchetBounds(c *StrategyConfig, r *ValidationResult) {
	armEnabled := c.Exit.RatchetArmPips > 0
	giveEnabled := c.Exit.RatchetGivebackPips > 0
	if c.Exit.RatchetArmPips < 0 {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.ratchet_arm_pips",
			Message: "must be ≥ 0"})
	}
	if c.Exit.RatchetGivebackPips < 0 {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.ratchet_giveback_pips",
			Message: "must be ≥ 0"})
	}
	if armEnabled != giveEnabled {
		// partial config — どちらかが 0 で片方だけ > 0 は禁止
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.ratchet",
			Message: fmt.Sprintf("ratchet TP は arm_pips/giveback_pips を同時指定 (arm=%.2f, give=%.2f)",
				c.Exit.RatchetArmPips, c.Exit.RatchetGivebackPips)})
	}
	if armEnabled && c.Exit.RatchetArmPips < 3 {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.ratchet_arm_pips",
			Message: fmt.Sprintf("%.2f < 3 (micro-noise/spike で即発火回避のため最小 3pips)",
				c.Exit.RatchetArmPips)})
	}
	if c.Exit.RatchetArmPips > 50 {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.ratchet_arm_pips",
			Message: fmt.Sprintf("%.2f > 50 (sanity cap; suggests typo)", c.Exit.RatchetArmPips)})
	}
	if giveEnabled && c.Exit.RatchetGivebackPips < 1 {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.ratchet_giveback_pips",
			Message: fmt.Sprintf("%.2f < 1 (1pip 未満の retrace で発火させないため最小 1pip)",
				c.Exit.RatchetGivebackPips)})
	}
	if c.Exit.RatchetGivebackPips > 50 {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.ratchet_giveback_pips",
			Message: fmt.Sprintf("%.2f > 50 (sanity cap; suggests typo)", c.Exit.RatchetGivebackPips)})
	}
	if armEnabled && giveEnabled && c.Exit.RatchetGivebackPips >= c.Exit.RatchetArmPips {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.ratchet_giveback_pips",
			Message: fmt.Sprintf("%.2f >= ratchet_arm_pips %.2f (giveback は arm より小さく)",
				c.Exit.RatchetGivebackPips, c.Exit.RatchetArmPips)})
	}
}

// validateAllowedHours: allowed_hours_jst optional whitelist. Empty = all
// hours. Each hour 0-23, no duplicates. Rejecting bad shapes here keeps
// Engine.Evaluate trivial.
func validateAllowedHours(c *StrategyConfig, r *ValidationResult) {
	if len(c.Entry.AllowedHoursJST) > 0 {
		seen := make(map[int]bool, len(c.Entry.AllowedHoursJST))
		for _, h := range c.Entry.AllowedHoursJST {
			if h < 0 || h > 23 {
				r.add(ValidationError{Type: ValidationSchema, Field: "entry.allowed_hours_jst",
					Message: fmt.Sprintf("%d outside [0,23]", h)})
			}
			if seen[h] {
				r.add(ValidationError{Type: ValidationSchema, Field: "entry.allowed_hours_jst",
					Message: fmt.Sprintf("duplicate hour %d", h)})
			}
			seen[h] = true
		}
	}
}

// validateChaseFilter: 追いかけ防止フィルタ。両方 0 = 無効
// (back-compat) なので範囲 sanity のみ。max_chase は typo guard で ≤ 100pips、
// lookback は 5m candle 本数なので ≤ 288 (= 24h)。
func validateChaseFilter(c *StrategyConfig, r *ValidationResult) {
	if c.Entry.MaxChasePips < 0 {
		r.add(ValidationError{Type: ValidationSchema, Field: "entry.max_chase_pips",
			Message: "must be ≥ 0"})
	}
	if c.Entry.MaxChasePips > 100 {
		r.add(ValidationError{Type: ValidationSchema, Field: "entry.max_chase_pips",
			Message: fmt.Sprintf("%.2f > 100 (sanity cap; suggests typo)", c.Entry.MaxChasePips)})
	}
	if c.Entry.ChaseLookbackCandles < 0 {
		r.add(ValidationError{Type: ValidationSchema, Field: "entry.chase_lookback_candles",
			Message: "must be ≥ 0"})
	}
	if c.Entry.ChaseLookbackCandles > 288 {
		r.add(ValidationError{Type: ValidationSchema, Field: "entry.chase_lookback_candles",
			Message: fmt.Sprintf("%d > 288 (= 24h of 5m candles; sanity cap)", c.Entry.ChaseLookbackCandles)})
	}
}

// validateEnabledConfigGuards applies the entry guards that only make sense on
// enabled (= entry) configs. no_trade is exempt (= 既に entry しない方針なので
// 確認不要)。
func validateEnabledConfigGuards(c *StrategyConfig, r *ValidationResult) {
	// 低確度 trade <0.35 を Go 側でも reject (skill 02 の sanity 二重化)。
	// Claude が skill 違反で enabled config を出してきた場合の最後の壁。
	if c.MarketRegime.Confidence < lowConfidenceFloor {
		r.add(ValidationError{Type: ValidationSchema, Field: "market_regime.confidence",
			Message: fmt.Sprintf("%.2f < %.2f (低確度 trade は no_trade に倒す方針)",
				c.MarketRegime.Confidence, lowConfidenceFloor)})
	}
	// early_exit window=0 (OFF) は ratchet が ON のときだけ許可する。MaxHold 強制 close の最悪損失の歯止めは early_exit
	// か ratchet の少なくとも一方が担保すれば足りる。range/scalp tier は
	// early_exit ON、trend/event tier は early_exit OFF + ratchet 主体、という
	// レジーム別 tier 切替を validator で阻害しないための緩和。
	// 両方 OFF (= 歯止めゼロ) は引き続き禁止。
	ratchetOn := c.Exit.RatchetArmPips > 0 && c.Exit.RatchetGivebackPips > 0
	// ma_pullback_v2 は trailing を一切持たず、出口を「固定 broker OCO
	// = 構造的 SL + 固定 TP(=2×SL)」一本に統一する設計 (浅い ratchet は伸びる
	// winner を早期に刈り取って矮小化するため)。最悪損失の歯止めは broker 側に置く
	// 構造的 SL が担う (bot 死でも守りが残る) ので、この戦略 (および同じく構造的
	// TP/SL を持つ下記の strategy_computed 系) に限り early_exit/ratchet 両 OFF を許可。
	if c.Exit.EarlyExitWindowMinutes == 0 && !ratchetOn &&
		c.Strategy.Name != StrategyMAPullbackV2 && c.Strategy.Name != StrategyGotobiFix &&
		c.Strategy.Name != StrategyLondonBreakout && c.Strategy.Name != StrategyTrendFollow && c.Strategy.Name != StrategyDailyTrend &&
		c.Strategy.Name != StrategyExhaustionFade {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.early_exit_window_minutes",
			Message: "early_exit OFF (window=0) は ratchet ON のときのみ許可 (early_exit / ratchet の少なくとも一方で最悪損失を緩和すること)"})
	}
	// ratchet OFF (0/0) を禁止。peak から戻ったら確定で「利益取り逃し」を減らす。
	// 例外: mtf_pullback / ma_pullback は元になった公開されている裁量手法どおり
	// ratchet/トレーリングを使わず、出口を構造的 TP/SL (建値時点で算出し pip 距離として
	// 既存 Signal 経路へ流す) に置く。他の strategy_computed 系戦略も同様。
	// 最悪損失の歯止めは構造的 SL (+ early_exit) が担うので、これらの戦略に限り
	// ratchet OFF を許可する。
	if c.Strategy.Name != StrategyMTFPullback && c.Strategy.Name != StrategyMAPullback &&
		c.Strategy.Name != StrategyMAPullbackV2 && c.Strategy.Name != StrategyGotobiFix &&
		c.Strategy.Name != StrategyLondonBreakout && c.Strategy.Name != StrategyTrendFollow &&
		c.Strategy.Name != StrategyDailyTrend && c.Strategy.Name != StrategyExhaustionFade &&
		c.Exit.RatchetArmPips == 0 && c.Exit.RatchetGivebackPips == 0 {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.ratchet",
			Message: "ratchet_arm_pips/giveback_pips both 0 = OFF is not allowed on enabled configs (trailing TP を強制)"})
	}
	// RR floor: ratchet の確保利益 (arm-giveback) が小さすぎると
	// 「勝ち薄利・負けフルSL」の逆RRになる (例: arm5/give3 と SL 15-20 で実効RR≈0.2)。
	// ratchet が利確するなら最低 take_profit_pips × ratchetMinLockedFracOfTP を確保してから。
	// 上の trailing TP 強制で arm/give 両 > 0 が保証される前提だが、片方 0 の異常時は上で
	// error 済みなので ここは両 > 0 のときだけ判定 (二重 error 回避)。
	// The RR-floor compares locked profit against the config take_profit_pips.
	// For strategy_computed configs that TP is a placeholder (the real TP is the
	// structural 30-80 the strategy computes), so the floor is meaningless and
	// would force the config to carry a dishonest placeholder TP just to pass.
	// Skip it for strategy_computed; the strategy's own arm/give ARE the real,
	// audited trailing values.
	if c.Exit.RatchetArmPips > 0 && c.Exit.RatchetGivebackPips > 0 && !c.IsStrategyComputedExit() {
		minLocked := c.Exit.TakeProfitPips * ratchetMinLockedFracOfTP
		locked := c.Exit.RatchetArmPips - c.Exit.RatchetGivebackPips
		if locked < minLocked {
			r.add(ValidationError{Type: ValidationSchema, Field: "exit.ratchet",
				Message: fmt.Sprintf("locked profit (arm−giveback=%.2f) < take_profit_pips×%.2f=%.2f (逆RR防止: ratchet は TP の手前で利を確保する設計)",
					locked, ratchetMinLockedFracOfTP, minLocked)})
		}
	}
}

// validateNoTradeConsistency enforces that a no_trade decision carries the
// canonical 0/none payload (strategy.name=no_trade, direction=none, qty=0,
// tp/sl/max_hold=0, ratchet=0/0) so a no_trade config can't smuggle entry-like
// fields.
func validateNoTradeConsistency(c *StrategyConfig, r *ValidationResult) {
	if c.Strategy.Name != StrategyNoTrade {
		r.add(ValidationError{Type: ValidationSchema, Field: "strategy.name", Message: "must be 'no_trade' when no_trade.enabled or enabled=false"})
	}
	if c.Entry.Direction != DirectionNone {
		r.add(ValidationError{Type: ValidationSchema, Field: "entry.direction", Message: "must be 'none' when no_trade"})
	}
	if c.Risk.Quantity != 0 {
		r.add(ValidationError{Type: ValidationSchema, Field: "risk.quantity", Message: "must be 0 when no_trade"})
	}
	if c.Exit.TakeProfitPips != 0 || c.Exit.StopLossPips != 0 || c.Exit.MaxHoldMinutes != 0 {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit", Message: "tp/sl/max_hold must be 0 when no_trade"})
	}
	// ratchet も no_trade では OFF 必須 (position が開かないので意味がないが、
	// 一貫性のため 0/0 を強制)。
	if c.Exit.RatchetArmPips != 0 || c.Exit.RatchetGivebackPips != 0 {
		r.add(ValidationError{Type: ValidationSchema, Field: "exit.ratchet", Message: "ratchet_arm_pips/giveback_pips must be 0 when no_trade"})
	}
}

// ValidateHardLimit ensures the config sits inside hard_limits.yaml bounds.
// no_trade configs are exempt from numeric bound checks (values are 0).
func (v *Validator) ValidateHardLimit(c *StrategyConfig) *ValidationResult {
	r := &ValidationResult{}
	if c == nil || v.Limits == nil {
		r.add(ValidationError{Type: ValidationHardLimit, Message: "validator or config nil"})
		return r
	}
	if !v.Limits.SymbolAllowed(c.Symbol) {
		r.add(ValidationError{Type: ValidationHardLimit, Field: "symbol", Message: fmt.Sprintf("%q not in allowed_symbols", c.Symbol)})
	}

	// TTL is a lifecycle constraint, NOT a trade numeric constraint. A no_trade
	// config with valid_until-valid_from = 0 or 99h still breaks the scheduler /
	// re-evaluation cadence. Check TTL BEFORE the no_trade early return.
	//
	// strategy_computed configs (ma_pullback / mtf_pullback etc.) are
	// permanent-window by design (far-future valid_until), so the [60,120]
	// advisor TTL window does not apply — this is what lets startup validation
	// (ValidateStatic) run on such active configs without tripping fail-close.
	// config_driven (advisor) configs are still bound to the window so the
	// re-evaluation cadence holds.
	if !c.IsStrategyComputedExit() {
		ttlMin := int(c.ValidUntil.Sub(c.ValidFrom).Minutes())
		if !v.Limits.ConfigTTLMinutes.Contains(ttlMin) {
			r.add(ValidationError{Type: ValidationHardLimit, Field: "valid_until",
				Message: fmt.Sprintf("ttl %d min outside [%d,%d]", ttlMin, v.Limits.ConfigTTLMinutes.Min, v.Limits.ConfigTTLMinutes.Max)})
		}
	}

	isNoTrade := c.IsNoTradeDecision()
	if isNoTrade {
		return r
	}

	if !v.Limits.Quantity.Contains(c.Risk.Quantity) {
		r.add(ValidationError{Type: ValidationHardLimit, Field: "risk.quantity",
			Message: fmt.Sprintf("%d outside [%d,%d]", c.Risk.Quantity, v.Limits.Quantity.Min, v.Limits.Quantity.Max)})
	}
	// 戦略別 TP/SL レンジが定義されていればそれを優先 (override)。
	// 定義がなければグローバルの take_profit_pips / stop_loss_pips にフォールバック。
	tpRange := v.Limits.TakeProfitPips
	slRange := v.Limits.StopLossPips
	tpSource := "hard_limits.take_profit_pips"
	slSource := "hard_limits.stop_loss_pips"
	if perStrat := v.Limits.LimitsFor(string(c.Strategy.Name)); perStrat != nil {
		tpRange = perStrat.TakeProfitPips
		slRange = perStrat.StopLossPips
		tpSource = fmt.Sprintf("strategy_limits[%s].take_profit_pips", c.Strategy.Name)
		slSource = fmt.Sprintf("strategy_limits[%s].stop_loss_pips", c.Strategy.Name)
	}
	// For strategy_computed configs the config TP/SL are validator placeholders,
	// not the live exits (which the strategy computes at entry: ma_pullback TP
	// 30-80 / SL 8-20). Range-checking the placeholders would force the config to
	// carry dishonest numbers just to pass; skip them so the audit row can hold
	// the honest structural numbers.
	if !c.IsStrategyComputedExit() {
		if !tpRange.Contains(c.Exit.TakeProfitPips) {
			r.add(ValidationError{Type: ValidationHardLimit, Field: "exit.take_profit_pips",
				Message: fmt.Sprintf("%.2f outside [%.2f,%.2f] (%s)", c.Exit.TakeProfitPips, tpRange.Min, tpRange.Max, tpSource)})
		}
		if !slRange.Contains(c.Exit.StopLossPips) {
			r.add(ValidationError{Type: ValidationHardLimit, Field: "exit.stop_loss_pips",
				Message: fmt.Sprintf("%.2f outside [%.2f,%.2f] (%s)", c.Exit.StopLossPips, slRange.Min, slRange.Max, slSource)})
		}
	}
	if !v.Limits.MaxHoldMinutes.Contains(c.Exit.MaxHoldMinutes) {
		r.add(ValidationError{Type: ValidationHardLimit, Field: "exit.max_hold_minutes",
			Message: fmt.Sprintf("%d outside [%d,%d]", c.Exit.MaxHoldMinutes, v.Limits.MaxHoldMinutes.Min, v.Limits.MaxHoldMinutes.Max)})
	}
	if !v.Limits.MaxSpreadPips.Contains(c.Entry.MaxSpreadPips) {
		r.add(ValidationError{Type: ValidationHardLimit, Field: "entry.max_spread_pips",
			Message: fmt.Sprintf("%.2f outside [%.2f,%.2f]", c.Entry.MaxSpreadPips, v.Limits.MaxSpreadPips.Min, v.Limits.MaxSpreadPips.Max)})
	}
	// max_trades_in_this_window counts trades since active.ValidFrom. For a
	// strategy_computed (permanent-window) config that window spans years, so a
	// POSITIVE cap would halt trading forever after N trades. Such configs must be 0 (=unlimited window;
	// the daily loss cap is the brake). config_driven (advisor) configs have a
	// bounded [60,120] window where a positive [0,5] cap limits churn.
	if c.IsStrategyComputedExit() {
		if c.Risk.MaxTradesInThisWindow != 0 {
			r.add(ValidationError{Type: ValidationHardLimit, Field: "risk.max_trades_in_this_window",
				Message: fmt.Sprintf("%d: a strategy_computed (permanent-window) config must be 0=unlimited — a positive cap over the permanent window halts trading after N trades", c.Risk.MaxTradesInThisWindow)})
		}
	} else if !v.Limits.MaxTradesInThisWindow.Contains(c.Risk.MaxTradesInThisWindow) {
		r.add(ValidationError{Type: ValidationHardLimit, Field: "risk.max_trades_in_this_window",
			Message: fmt.Sprintf("%d outside [%d,%d]", c.Risk.MaxTradesInThisWindow, v.Limits.MaxTradesInThisWindow.Min, v.Limits.MaxTradesInThisWindow.Max)})
	}
	if !v.Limits.MaxLossInThisWindowJPY.Contains(c.Risk.MaxLossInThisWindowJPY) {
		r.add(ValidationError{Type: ValidationHardLimit, Field: "risk.max_loss_in_this_window_jpy",
			Message: fmt.Sprintf("%d outside [%d,%d]", c.Risk.MaxLossInThisWindowJPY, v.Limits.MaxLossInThisWindowJPY.Min, v.Limits.MaxLossInThisWindowJPY.Max)})
	}
	// TTL is checked above, before the no_trade early-return.
	return r
}

// ValidateRisk checks runtime conditions: emergency stop, daily DD, unknown
// positions, etc.
func (v *Validator) ValidateRisk(c *StrategyConfig, state AccountState) *ValidationResult {
	r := &ValidationResult{}
	if state.EmergencyStop {
		r.add(ValidationError{Type: ValidationRisk, Field: "emergency_stop", Message: "active — refusing new config promotion"})
	}
	if state.MaxDailyLossJPY > 0 && state.DailyLossJPY >= state.MaxDailyLossJPY {
		r.add(ValidationError{Type: ValidationRisk, Field: "daily_loss",
			Message: fmt.Sprintf("daily loss %d >= cap %d", state.DailyLossJPY, state.MaxDailyLossJPY)})
	}
	if state.MaxConsecutiveLosses > 0 && state.ConsecutiveLosses >= state.MaxConsecutiveLosses {
		r.add(ValidationError{Type: ValidationRisk, Field: "consecutive_losses",
			Message: fmt.Sprintf("%d >= cap %d", state.ConsecutiveLosses, state.MaxConsecutiveLosses)})
	}
	if state.UnknownPositionExists {
		r.add(ValidationError{Type: ValidationRisk, Field: "positions", Message: "unknown position detected"})
	}
	if state.UnresolvedOrderExists {
		r.add(ValidationError{Type: ValidationRisk, Field: "orders", Message: "unresolved order detected"})
	}
	if c != nil && state.MaxOpenPositions > 0 && c.Risk.MaxOpenPositions > state.MaxOpenPositions {
		r.add(ValidationError{Type: ValidationRisk, Field: "risk.max_open_positions",
			Message: fmt.Sprintf("%d exceeds bot cap %d", c.Risk.MaxOpenPositions, state.MaxOpenPositions)})
	}
	// valid_from sanity: must not be far in the past or far in the future.
	if c != nil && v.Now != nil {
		now := v.Now()
		if c.ValidUntil.Before(now) {
			r.add(ValidationError{Type: ValidationRisk, Field: "valid_until", Message: "already expired"})
		}
		if c.ValidFrom.After(now.Add(2 * time.Hour)) {
			r.add(ValidationError{Type: ValidationRisk, Field: "valid_from", Message: "more than 2h in the future"})
		}
	}
	return r
}

// ValidateSemantic catches configs that pass schema and hard_limits but are
// internally contradictory or violate runtime constraints not expressible in
// schema (semantic-validation pass).
//
// Checks:
//   - market_regime=trend_up + direction=sell_only → contradiction
//   - market_regime=trend_down + direction=buy_only → contradiction
//   - strategy=momentum_pullback + require_breakout=true → wrong strategy/flag pair
//   - strategy=breakout_follow + require_breakout=false → wrong strategy/flag pair
//   - strategy=range_breakout_probe + require_breakout=true → wrong strategy/flag pair
//   - strategy.name not in allowedStrategies (when allowedStrategies is non-empty)
//
// no_trade configs (enabled=false / no_trade.enabled=true) bypass the
// regime/direction check since they're not placing orders.
func (v *Validator) ValidateSemantic(c *StrategyConfig, allowedStrategies []string) *ValidationResult {
	r := &ValidationResult{}
	if c == nil {
		r.add(ValidationError{Type: ValidationSemantic, Message: "config is nil"})
		return r
	}

	// Whitelist enforcement. Empty list = skip (caller hasn't configured it).
	if len(allowedStrategies) > 0 {
		allowed := false
		for _, name := range allowedStrategies {
			if string(c.Strategy.Name) == name {
				allowed = true
				break
			}
		}
		if !allowed {
			r.add(ValidationError{
				Type: ValidationSemantic, Field: "strategy.name",
				Message: fmt.Sprintf("%q not in allowed_strategies %v", c.Strategy.Name, allowedStrategies),
			})
		}
	}

	// Skip regime/direction checks for no_trade — it doesn't place orders.
	isNoTrade := c.IsNoTradeDecision()

	if !isNoTrade {
		// trend_up + sell_only / trend_down + buy_only conflicts.
		switch {
		case c.MarketRegime.Type == RegimeTrendUp && c.Entry.Direction == DirectionSellOnly:
			r.add(ValidationError{
				Type: ValidationSemantic, Field: "entry.direction",
				Message: "trend_up regime with sell_only direction is contradictory",
			})
		case c.MarketRegime.Type == RegimeTrendDown && c.Entry.Direction == DirectionBuyOnly:
			r.add(ValidationError{
				Type: ValidationSemantic, Field: "entry.direction",
				Message: "trend_down regime with buy_only direction is contradictory",
			})
		}

		// Strategy / require_breakout pair check.
		switch c.Strategy.Name {
		case StrategyMomentumPullback:
			if c.Entry.RequireBreakout {
				r.add(ValidationError{
					Type: ValidationSemantic, Field: "entry.require_breakout",
					Message: "momentum_pullback requires require_breakout=false",
				})
			}
		case StrategyBreakoutFollow:
			if !c.Entry.RequireBreakout {
				r.add(ValidationError{
					Type: ValidationSemantic, Field: "entry.require_breakout",
					Message: "breakout_follow requires require_breakout=true",
				})
			}
		case StrategyRangeBreakoutProbe:
			if c.Entry.RequireBreakout {
				r.add(ValidationError{
					Type: ValidationSemantic, Field: "entry.require_breakout",
					Message: "range_breakout_probe requires require_breakout=false",
				})
			}
		case StrategyMTFPullback:
			// mtf_pullback's "breakout" is its own 5m-wall trigger, not the
			// config require_breakout flag (which gates breakout_follow). It must
			// stay false so the two breakout notions don't collide.
			if c.Entry.RequireBreakout {
				r.add(ValidationError{
					Type: ValidationSemantic, Field: "entry.require_breakout",
					Message: "mtf_pullback requires require_breakout=false",
				})
			}
		case StrategyMAPullback:
			// ma_pullback is a rebound/continuation strategy: it enters as
			// price returns TO the 200MA zone, never on a config-level breakout.
			// require_breakout must stay false (it gates breakout_follow only).
			if c.Entry.RequireBreakout {
				r.add(ValidationError{
					Type: ValidationSemantic, Field: "entry.require_breakout",
					Message: "ma_pullback requires require_breakout=false",
				})
			}
		case StrategyMAPullbackV2:
			// ma_pullback_v2 is the same rebound-to-200MA family as ma_pullback;
			// it enters on a pullback, never a config-level breakout.
			if c.Entry.RequireBreakout {
				r.add(ValidationError{
					Type: ValidationSemantic, Field: "entry.require_breakout",
					Message: "ma_pullback_v2 requires require_breakout=false",
				})
			}
		}
	}
	return r
}

// ValidateStatic runs the config-QUALITY passes (schema + hard_limit +
// semantic) but NOT the runtime ValidateRisk pass. It takes no AccountState, so
// it can run at startup (LoadActiveFromDB) to fail-close on a structurally-invalid
// active config independent of the live account state — emergency_stop /
// daily_loss legitimately vary across restarts and must NOT brick startup.
// TTL-exemption for strategy_computed configs (handled inside ValidateHardLimit)
// is what lets those permanent-window configs pass this at startup despite their
// far-future valid_until.
func (v *Validator) ValidateStatic(c *StrategyConfig, allowedStrategies ...string) *ValidationResult {
	combined := &ValidationResult{}
	for _, sub := range []*ValidationResult{
		v.ValidateSchema(c),
		v.ValidateHardLimit(c),
		v.ValidateSemantic(c, allowedStrategies),
	} {
		combined.Errors = append(combined.Errors, sub.Errors...)
	}
	return combined
}

// ValidateAll runs schema, hard-limit, semantic and risk checks and combines
// their errors. The caller can inspect ValidationResult.Errors[i].Type to know
// which pass produced each issue.
//
// allowedStrategies is forwarded to ValidateSemantic for whitelist enforcement.
// Pass nil to skip the whitelist check (back-compat).
func (v *Validator) ValidateAll(c *StrategyConfig, state AccountState, allowedStrategies ...string) *ValidationResult {
	combined := &ValidationResult{}
	for _, sub := range []*ValidationResult{
		v.ValidateSchema(c),
		v.ValidateHardLimit(c),
		v.ValidateSemantic(c, allowedStrategies),
		v.ValidateRisk(c, state),
	} {
		combined.Errors = append(combined.Errors, sub.Errors...)
	}
	return combined
}
