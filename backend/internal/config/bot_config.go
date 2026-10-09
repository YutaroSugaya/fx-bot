package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Mode string

const (
	ModeDisabled    Mode = "disabled"
	ModePaperConfig Mode = "paper_config"
	ModeLiveConfig  Mode = "live_config"
)

func (m Mode) Valid() bool {
	switch m {
	case ModeDisabled, ModePaperConfig, ModeLiveConfig:
		return true
	}
	return false
}

// IsLive reports whether m is ModeLiveConfig.
// Use this in production code instead of comparing to the string literal
// "live_config" so the single check site is preserved.
func (m Mode) IsLive() bool { return m == ModeLiveConfig }

type BotSection struct {
	Mode     Mode   `yaml:"mode"`
	Timezone string `yaml:"timezone"`
}

type AIAdvisorSection struct {
	Enabled                 bool   `yaml:"enabled"`
	Provider                string `yaml:"provider"`
	IntervalMinutes         int    `yaml:"interval_minutes"`
	ClaudeCLITimeoutSeconds int    `yaml:"claude_cli_timeout_seconds"`
	ConfigTTLMinutes        int    `yaml:"config_ttl_minutes"`
	PromptPath              string `yaml:"prompt_path"`
	InputPath               string `yaml:"input_path"`
	OutputPath              string `yaml:"output_path"`

	// Multi-symbol parallel advisor cycle tuning. 0 = use derived default
	// (advisorMaxConcurrent heuristic / ClaudeCLITimeoutSeconds+30s).
	MaxConcurrentSymbols    int `yaml:"max_concurrent_symbols,omitempty"`
	PerSymbolTimeoutSeconds int `yaml:"per_symbol_timeout_seconds,omitempty"`
}

// AdvisorV2Section configures advisor v2 (signature-breakout, classic chart-breakout model).
// DEFAULT OFF (opt-in via enabled: true): the periodic LLM only judges go/no-go on a
// deterministic daily breakout; geometry, the risk Gate and broker OCO stay deterministic.
// Enable it at minimum size first — it is never on by default.
type AdvisorV2Section struct {
	Enabled         bool `yaml:"enabled"`
	IntervalMinutes int  `yaml:"interval_minutes"`
	// Exclusive: when true (with Enabled), the engine-tick entry path (the existing fixed active
	// config, e.g. a trend_follow config) is DISABLED so advisor v2 is the ONLY entry source. Open
	// positions keep being managed/exited (exits run on a separate path). Default false = both run.
	Exclusive bool `yaml:"exclusive"`
	// Deterministic: when true, the LLM (breakout-advisor) judgment is SKIPPED — a found signature
	// setup auto-enters. What the holdout backtest measures is the deterministic rule itself;
	// the LLM gate is un-backtestable, so deterministic mode keeps it reproducible and
	// fully testable. Safety stays deterministic (risk Gate: spread/event/single-position/daily-loss).
	// Default false = LLM judges go/no-go.
	Deterministic bool `yaml:"deterministic"`
	// Quantity is the per-entry order size (units) for advisor v2. 0 = fall back to
	// hard_limits.quantity.min (1,000). Must stay within hard_limits.quantity {min,max}; the
	// per-trade ≤max_loss_per_trade_jpy guard still rejects any entry whose SL*size exceeds it.
	Quantity int `yaml:"quantity"`
	// MaxSpreadPips skips advisor v2 entry while the live spread exceeds this (0 = no cap). Defers
	// fills out of the wide-spread window (06:00-07:00 JST daily roll / news); the next hourly cycle
	// retries so the confirmed breakout still gets entered once liquidity normalizes.
	MaxSpreadPips float64 `yaml:"max_spread_pips"`
	// Symbols restricts advisor v2 to this allowlist (e.g. [USD_JPY]). EMPTY = all configured
	// symbols (back-compat). Other symbols get no v2 cycle (and, in exclusive mode, no entries
	// at all).
	Symbols []string `yaml:"symbols"`
	// MaxConcurrent caps simultaneous SAME-SYMBOL same-side v2 positions. 0/1 = the strict single-
	// position rule (no nanpin), identical to every other strategy. 2 = "add to a winner": the cycle
	// only opens the 2nd when the 1st's ratchet is already ARMED (~+3×ATR in profit, trailing in
	// profit), so the 1st can no longer produce a loss while the bot is alive and aggregate risk
	// stays ≈ one unit. The risk Gate enforces the hard cap; the "armed" condition is checked upstream.
	MaxConcurrent int `yaml:"max_concurrent"`
}

// V2SymbolEnabled reports whether advisor v2 should run for the given symbol:
// true when v2 is Enabled AND (the Symbols allowlist is empty OR contains sym).
func (a AdvisorV2Section) V2SymbolEnabled(sym string) bool {
	if !a.Enabled {
		return false
	}
	if len(a.Symbols) == 0 {
		return true
	}
	for _, s := range a.Symbols {
		if s == sym {
			return true
		}
	}
	return false
}

// LLMDecisionSection configures the autonomous LLM trade loop. When
// Enabled, a periodic cycle asks Claude (trade-decider subagent) to choose trade/no-trade + side +
// TP/SL each interval, submitted via the EXISTING order path (risk Gate + broker OCO, forced qty).
// An optional lower-cadence reflection cycle reviews recent trades and auto-updates the playbook the
// decider reads (see ReflectionEnabled). DEFAULT OFF — opt-in via enabled: true (and the live
// invariant: TP/SL go broker-side via OCO).
type LLMDecisionSection struct {
	Enabled         bool `yaml:"enabled"`
	IntervalMinutes int  `yaml:"interval_minutes"` // decision cadence (0 → 60)
	Quantity        int  `yaml:"quantity"`         // forced order size (0 → 1000)
	// QuantityBySymbol optionally overrides Quantity per pair (e.g. for per-currency lane
	// variants). Unset/0 pair = fall back to Quantity. Resolve via QuantityFor.
	QuantityBySymbol    map[string]int `yaml:"quantity_by_symbol,omitempty"`
	MaxSpreadPips       float64        `yaml:"max_spread_pips"`       // pre-submit spread defer (0 = no cap)
	MaxHoldMinutes      int            `yaml:"max_hold_minutes"`      // position MaxHold (0 → 1440)
	RatchetArmPips      float64        `yaml:"ratchet_arm_pips"`      // trailing arm (0 = off; broker OCO TP/SL still protect)
	RatchetGivebackPips float64        `yaml:"ratchet_giveback_pips"` // trailing giveback (0 = off)
	MaxConcurrent       int            `yaml:"max_concurrent"`        // same-symbol same-side cap (0/1 = no nanpin)
	Symbols             []string       `yaml:"symbols"`               // allowlist (empty = all; e.g. [USD_JPY])

	// MTF directional veto: refuse an entry AGAINST the last day's move (counter-trend
	// BUY into a falling day / SELL into a rising day) — where the trade-history analysis
	// found losses concentrated. 0 = OFF. Applies to ALL pairs.
	// Judged on Summary24h.ChangePips — the same yardstick the playbook advertises —
	// so HTFTrendVetoLookback is no longer used (kept only so old yamls still parse).
	HTFTrendVetoPips     float64 `yaml:"htf_trend_veto_pips,omitempty"`
	HTFTrendVetoLookback int     `yaml:"htf_trend_veto_lookback,omitempty"` // DEPRECATED: unused (change_pips-based)
	// HTFTrendVetoExempt{Sell,Buy}Rpos: waive the MTF veto for a POSITIONED counter-trend
	// entry — a SELL from the top of the trailing 24h range (rpos ≥ Sell bound) or a BUY
	// from a genuine pullback (rpos ≤ Buy bound). Mid-range counter-trend entries are still
	// vetoed. PER-PAIR maps (like MaxRangePos24hBuy) because the verified pullback zones
	// differ per pair (e.g. GBP_JPY BUY: rpos<0.4; USD_JPY: ≤0.5) — a global bound would
	// open GBP_JPY's unverified (0.4, 0.5] band. 0/omitted pair = no exemption there.
	// Fraction 0-1. Leave unset to keep the veto strict: any exemption re-opens part of
	// the counter-trend hole the veto exists to close (kept for per-currency lane variants).
	HTFTrendVetoExemptSellRpos map[string]float64 `yaml:"htf_trend_veto_exempt_sell_rpos,omitempty"`
	HTFTrendVetoExemptBuyRpos  map[string]float64 `yaml:"htf_trend_veto_exempt_buy_rpos,omitempty"`

	// ExcludeHoursJST cedes specific JST hours-of-day on a symbol to a deterministic
	// strategy so the hourly LLM loop and the per-tick engine never compete for the
	// single position slot. Keyed by symbol → excluded hours (0-23). Example:
	// {USD_JPY: [4,10,11]} → exhaustion_fade owns USD/JPY in those hours, LLM the rest.
	// Empty/omitted = no exclusion (back-compat).
	ExcludeHoursJST map[string][]int `yaml:"exclude_hours_jst,omitempty"`

	// Per-currency entry discipline. An analysis of the trade history plus 1m candles found
	// the losses concentrated in three behaviours; these deterministic vetoes stop them in code
	// (a prompt instruction alone did not). All default OFF when omitted (back-compat); all fail
	// open on missing market data inside the cycle.
	//
	// NightBuyVetoHoursJST: JST hours (0-23) in which BUY entries are refused on every symbol
	// (SELL passes — downside continues through the night). e.g. [0,1,2,3,4,5].
	NightBuyVetoHoursJST []int `yaml:"night_buy_veto_hours_jst,omitempty"`
	// MaxRangePos24hBuy: per-symbol ceiling on Summary24h range position for a BUY — above it
	// the entry is a high-chase and is refused. 0/omitted = OFF for that symbol.
	MaxRangePos24hBuy map[string]float64 `yaml:"max_range_position_24h_buy,omitempty"`
	// MinRangePos24hSell: per-symbol floor on Summary24h range position for a SELL — below it
	// the entry chases a low on a pair that rebounds (GBP_USD). MUST stay unset for USD_JPY
	// (in the trade-history analysis its sell-low continuation was the least-bad pattern —
	// do not default-ban it). 0/omitted = OFF.
	MinRangePos24hSell map[string]float64 `yaml:"min_range_position_24h_sell,omitempty"`

	// Lane-rulebook vetoes (the playbook's HARD bans, enforced in code).
	// ExhaustionVetoPips: refuse an entry in the SAME direction the 24h window has already
	// travelled ≥ this many pips (chasing a spent move).
	// SpikeVetoPips15m: refuse ANY entry while |15m net move| ≥ this (mid-spike cooldown —
	// an entry inside a spike tends to fill at the extreme just before it reverts).
	// Both 0/omitted = OFF; both fail open on missing market data.
	ExhaustionVetoPips float64 `yaml:"exhaustion_veto_pips,omitempty"`
	SpikeVetoPips15m   float64 `yaml:"spike_veto_pips_15m,omitempty"`
	// Arms: the hourly decision may pre-place conditional entry plans ("if price breaks
	// LEVEL, enter SIDE"); a deterministic per-tick watcher fires them after re-validating
	// every veto AND the lane 土俵 on fresh market state.
	// ArmEnabled gates the whole feature (default false = plans are ignored).
	// ArmMaxDistancePips caps |trigger − current mid| at decision time (0 → 30).
	ArmEnabled         bool    `yaml:"arm_enabled,omitempty"`
	ArmMaxDistancePips float64 `yaml:"arm_max_distance_pips,omitempty"`

	// DailyLossStopCount: stop the symbol for the rest of the trading day
	// (06:00 JST boundary) once it has this many gross-losing closed trades today — the
	// playbook's 同日2敗打ち止め enforced in code, BEFORE the LLM call. 0/omitted = OFF;
	// a trades-fetch error fails open (a DB blip must never halt trading by itself).
	DailyLossStopCount int `yaml:"daily_loss_stop_count,omitempty"`

	// DecisionSingleAgent collapses the per-cycle decision to ONE claude agent (no market-regime/
	// trade-decider sub-agent panel): a single invocation reasons regime→strategy→entry inline and
	// outputs the YAML, invoked with --tools Read (NO Task) so it CANNOT spawn sub-agents. This kills
	// the panel's orchestration fragility (stray agents, lost decisions, parse fallbacks) and lets
	// regime+strategy be judged jointly. Pointer-bool: nil/omitted → true (single-agent
	// is the new default); set false to REVERT to the 2-step sub-agent panel. Reflection is unaffected.
	DecisionSingleAgent *bool `yaml:"decision_single_agent,omitempty"`

	// Reflection (learning) sub-cadence — the Reflexion loop that rewrites the playbook.
	//
	// ReflectionEnabled gates the whole Reflexion loop. Set false to keep the playbook
	// human-reviewed text only — no autonomous rewrite; strategy revisions are reviewed and
	// appended manually. Pointer-bool: nil/omitted → true (back-compat with configs predating
	// the flag). When off, the reflection scheduler never starts (no daily Claude call, no
	// playbook writes).
	ReflectionEnabled         *bool `yaml:"reflection_enabled,omitempty"`
	ReflectionIntervalMinutes int   `yaml:"reflection_interval_minutes"` // 0 → 1440 (daily)
	ReflectionMinTrades       int   `yaml:"reflection_min_trades"`       // overfitting guard; 0 → 20

	// ReflectionStartAt (RFC3339, optional) floors the trades the Reflexion loop reviews
	// so it only learns from THIS loop's own trades. Without it the loop reviewed every
	// recent trade for the symbol — including trades placed by prior strategies that share
	// the borrowed active config_id — and would "learn" from another strategy's losses.
	// Empty = no floor (back-compat, 30d window only).
	// Set this to the moment the LLM loop started trading the symbol.
	ReflectionStartAt string `yaml:"reflection_start_at,omitempty"`

	// EventRetrigger: re-run the decision cycle on specific events on top of the hourly
	// cadence — a position close frees the symbol's slot, and a large move means the hourly
	// judgment is stale. Omitted = OFF (back-compat).
	EventRetrigger EventRetriggerSection `yaml:"event_retrigger,omitempty"`

	// セッションガード — 毎朝 05:45 JST の GMO スプレッド壁 (USD_JPY で 0.5→10pips 程度へ
	// 一時的に拡大。クロス円ではさらに広がる) への構造対策。壁の瞬間は bid 側が建値付近でも
	// ask ジャンプで tight な SL が機械的に刈られる。
	//
	// NoEntryHoursJST: この JST 時間帯は全 side の新規を止める (LLM cycle は判断ごと skip =
	// API 節約、armed plan の発火も拒否)。night_buy_veto の全 side 版。例 = [2,3,4,5]
	// (壁までの滑走路 <3.5h の建玉を排除)。空 = OFF。
	NoEntryHoursJST []int `yaml:"no_entry_hours_jst,omitempty"`
	// SessionFlattenJST: 毎朝この JST 時刻 ("HH:MM") に全 OPEN 玉を market close する
	// (close_reason=session_flatten)。壁の 15 分前 = "05:30" を想定。土曜のこの回が
	// 金曜カットオフ (週末ギャップ回避) の実装を兼ねる。
	// 在位中の broker OCO はそのまま (bot 死 = OCO が守る fail-safe は不変)。
	// 冬時間は壁が 06:45 へ移るが固定 05:30 で安全側 (ダッシュ 6 時境界と同じ固定 JST 主義)。
	// 空 = OFF。
	SessionFlattenJST string `yaml:"session_flatten_jst,omitempty"`
}

// SessionFlattenMinutesJST parses SessionFlattenJST ("HH:MM") into JST
// minutes-of-day. ok=false when the feature is off (empty). Format errors are
// caught LOUDLY at boot by validate() — by the time wiring calls this, a
// non-empty value is guaranteed well-formed (the second return is still safe:
// a malformed value yields ok=false = feature off, never a wrong time).
func (a LLMDecisionSection) SessionFlattenMinutesJST() (int, bool) {
	m, err := parseMinutesOfDay(a.SessionFlattenJST)
	if err != nil {
		return 0, false
	}
	return m, true
}

// parseMinutesOfDay parses "HH:MM" (00:00-23:59) into minutes-of-day.
// Empty input is an error (callers treat it as "off").
func parseMinutesOfDay(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("must be HH:MM (e.g. 05:30): %w", err)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// EventRetriggerSection configures the event-driven LLM re-judgment on top of the hourly
// cycle. Every trigger funnels into the SAME all-pairs cycle path as the manual dashboard
// button (weekend gate + llmCycleRunning guard), so a running scheduled cycle always wins
// and two cycles never overlap. The strategy (playbook) is untouched — this only changes
// WHEN the unchanged checklist is re-judged.
type EventRetriggerSection struct {
	Enabled bool `yaml:"enabled"` // master gate; false = no event ever fires
	// OnPositionClose fires a re-judgment when any position finishes closing (broker OCO
	// fill via reconcile, or a bot-side MaxHold/ratchet/manual close) — the freed slot is
	// re-examined immediately instead of waiting up to 59 minutes.
	OnPositionClose bool `yaml:"on_position_close,omitempty"`
	// MovePips fires a re-judgment when |mid now − mid MoveWindow ago| ≥ this many pips
	// on any pair. 0 = move trigger OFF.
	MovePips          float64 `yaml:"move_pips,omitempty"`
	MoveWindowMinutes int     `yaml:"move_window_minutes,omitempty"` // 0 → 30
	// Cooldowns bound the burn on wild days: a trending afternoon must not turn into a
	// cycle per tick. Each is armed on every fire ATTEMPT (even one refused by the
	// run-guard), global across pairs.
	MoveCooldownMinutes  int `yaml:"move_cooldown_minutes,omitempty"`  // 0 → 20
	CloseCooldownMinutes int `yaml:"close_cooldown_minutes,omitempty"` // 0 → 3 (dedupe multi-close)
}

// MoveWindow returns the lookback window for the big-move trigger (default 30m).
func (e EventRetriggerSection) MoveWindow() time.Duration {
	if e.MoveWindowMinutes > 0 {
		return time.Duration(e.MoveWindowMinutes) * time.Minute
	}
	return 30 * time.Minute
}

// MoveCooldown returns the minimum spacing between big-move fire attempts (default 20m).
func (e EventRetriggerSection) MoveCooldown() time.Duration {
	if e.MoveCooldownMinutes > 0 {
		return time.Duration(e.MoveCooldownMinutes) * time.Minute
	}
	return 20 * time.Minute
}

// CloseCooldown returns the minimum spacing between position-close fire attempts
// (default 3m — mainly dedupes several positions closing together).
func (e EventRetriggerSection) CloseCooldown() time.Duration {
	if e.CloseCooldownMinutes > 0 {
		return time.Duration(e.CloseCooldownMinutes) * time.Minute
	}
	return 3 * time.Minute
}

// SingleAgentDecision reports whether the per-cycle decision runs as a single agent (default true)
// vs the 2-step sub-agent panel (explicit DecisionSingleAgent:false). See the field doc.
func (s LLMDecisionSection) SingleAgentDecision() bool {
	return s.DecisionSingleAgent == nil || *s.DecisionSingleAgent
}

// ReflectionOn reports whether the Reflexion learning loop (playbook auto-rewrite) may run.
// Default true when omitted (back-compat); set reflection_enabled:false to keep playbook
// updates human-reviewed only. See the ReflectionEnabled field doc.
func (s LLMDecisionSection) ReflectionOn() bool {
	return s.ReflectionEnabled == nil || *s.ReflectionEnabled
}

// ReflectionSince returns the lower time bound (on closed_at) for trades fed to the
// Reflexion loop: the LATER of (now-lookback) and ReflectionStartAt when set. A malformed
// ReflectionStartAt falls back to the lookback floor — load-time validation rejects malformed
// values first, so this branch is unreachable in practice.
func (s LLMDecisionSection) ReflectionSince(now time.Time, lookback time.Duration) time.Time {
	floor := now.Add(-lookback)
	if start, ok := s.StartAtTime(); ok && start.After(floor) {
		return start
	}
	return floor
}

// StartAtTime parses ReflectionStartAt (the LLM loop go-live = the performance epoch shared by
// the reflection floor and the dashboard P&L/win-rate aggregates). ok=false (zero time) when
// unset or malformed; load-time validation rejects malformed values when llm_decision is enabled.
func (s LLMDecisionSection) StartAtTime() (time.Time, bool) {
	if s.ReflectionStartAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s.ReflectionStartAt)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// QuantityFor resolves the order size for a symbol: the per-symbol override when set (>0),
// else the global Quantity. A 0 result means "no size configured" — the wiring layer then
// applies the hard-limits floor (1,000), exactly as it always has for a zero Quantity.
func (s LLMDecisionSection) QuantityFor(sym string) int {
	if q := s.QuantityBySymbol[sym]; q > 0 {
		return q
	}
	return s.Quantity
}

// SymbolEnabled reports whether the LLM decision loop should run for the given symbol:
// true when Enabled AND (the Symbols allowlist is empty OR contains sym).
func (a LLMDecisionSection) SymbolEnabled(sym string) bool {
	if !a.Enabled {
		return false
	}
	if len(a.Symbols) == 0 {
		return true
	}
	for _, s := range a.Symbols {
		if s == sym {
			return true
		}
	}
	return false
}

type RiskSection struct {
	// per-symbol cap
	MaxDailyLossJPY      int `yaml:"max_daily_loss_jpy"`
	MaxConsecutiveLosses int `yaml:"max_consecutive_losses"`
	MaxOpenPositions     int `yaml:"max_open_positions"`

	// account-wide cap. 0 = disabled.
	AccountMaxOpenPositions int `yaml:"account_max_open_positions,omitempty"`
	AccountMaxDailyLossJPY  int `yaml:"account_max_daily_loss_jpy,omitempty"`

	// DisableConsecutiveLossGuards: true で連敗系スロットルを一括無効化
	// (MaxConsecutiveLosses cap / 3 連敗 30 分 freeze / 同方向 2-SL の当日 block)。
	// trade 回数を稼いで分析するための knob。daily_loss / account_daily_loss cap は残る。
	// 既定 false。ガードを外すと連敗中も同じサイズで発注し続けるので、live では推奨しない
	// (live で実効 qty >= app.QtyGuardThreshold のときは起動時に拒否される)。
	DisableConsecutiveLossGuards bool `yaml:"disable_consecutive_loss_guards,omitempty"`

	// PostLossFreezeMinutes: per-symbol、任意の負け決済 (ratchet_stoploss 含む) の直後に
	// そのシンボルの新規エントリーを N 分止める。
	// DisableConsecutiveLossGuards から独立 (連敗系ガードを無効化していても freeze だけ効く)。
	// 損切り直後の即リベンジ発注 (ウィップソーでの連敗) を防ぐ。0 = 無効。
	PostLossFreezeMinutes int `yaml:"post_loss_freeze_minutes,omitempty"`

	// ReentryCooldownMinutes: 同 symbol 同 side は「任意の決済 (勝ち負け・close reason 問わず)」
	// から N 分間新規禁止。PostLossFreeze(負け限定・両 side)と独立で、勝ち直後の即再 IN も
	// 塞ぐ — event_retrigger は決済直後に再判断を
	// 走らせるため、利確の数分後に同 symbol 同 side へ再 IN して伸び切った所を掴みうる。
	// 当日 (bot.timezone の 0:00 境界 = config.StartOfDayIn) の決済のみ対象=日境界を跨いだ
	// 持ち越しなし (0:00 直前の決済は 0:00 以降の再 IN を塞がない)。0 = 無効。
	ReentryCooldownMinutes int `yaml:"reentry_cooldown_minutes,omitempty"`
}

type GMOSection struct {
	PublicBaseURL          string `yaml:"public_base_url"`
	PrivateBaseURL         string `yaml:"private_base_url"`
	PrivateGetLimitPerSec  int    `yaml:"private_get_limit_per_sec"`
	PrivatePostLimitPerSec int    `yaml:"private_post_limit_per_sec"`
}

// OrdersSection is parsed for back-compat but currently unused: no production code reads
// either field. Live entries are always MARKET followed by a broker-side OCO (TP/SL), with the
// order path chosen by bot.mode — not by these values.
type OrdersSection struct {
	PreferOrderType   string `yaml:"prefer_order_type"`   // unused (e.g. MARKET_THEN_OCO, documentation only)
	FallbackOrderType string `yaml:"fallback_order_type"` // unused (reserved)
}

type SchedulerSection struct {
	WeekdaysOnly bool   `yaml:"weekdays_only"`
	StartDay     string `yaml:"start_day"`
	StartTime    string `yaml:"start_time"`
	EndDay       string `yaml:"end_day"`
	EndTime      string `yaml:"end_time"`
}

type BotConfig struct {
	Bot         BotSection         `yaml:"bot"`
	Symbol      string             `yaml:"symbol,omitempty"`  // legacy single-symbol field; kept for backward compat
	Symbols     []string           `yaml:"symbols,omitempty"` // Normalize() keeps in sync with Symbol
	AIAdvisor   AIAdvisorSection   `yaml:"ai_advisor"`
	AdvisorV2   AdvisorV2Section   `yaml:"advisor_v2"`
	LLMDecision LLMDecisionSection `yaml:"llm_decision"`
	Risk        RiskSection        `yaml:"risk"`
	GMO         GMOSection         `yaml:"gmo"`
	Orders      OrdersSection      `yaml:"orders"`
	// position_guard (naked_position_action / emergency_close_positions) was removed: it
	// had zero production readers and contradicted the actual behaviour (it implied
	// CLOSE_MARKET for naked broker positions, but the reconciler ADOPTS them).
	// yaml keys, if still present, are ignored.
	Scheduler SchedulerSection `yaml:"scheduler"`
}

// ResolveSymbols returns the bot's target symbol list, falling back to the
// legacy Symbol field for callers that bypass Normalize().
func (c *BotConfig) ResolveSymbols() []string {
	if len(c.Symbols) > 0 {
		return c.Symbols
	}
	if c.Symbol != "" {
		return []string{c.Symbol}
	}
	return nil
}

// Normalize fills whichever of Symbols / Symbol is missing from the other,
// so legacy callers reading Symbol see Symbols[0] and vice versa.
// LoadBotConfig calls this automatically.
func (c *BotConfig) Normalize() {
	if len(c.Symbols) == 0 && c.Symbol != "" {
		c.Symbols = []string{c.Symbol}
	}
	if c.Symbol == "" && len(c.Symbols) > 0 {
		c.Symbol = c.Symbols[0]
	}
}

// ValidateAgainstHardLimits asserts every configured symbol is whitelisted
// in hard_limits.allowed_symbols. Called at startup as a fail-close guard
// so YAML drift (symbol added to bot_config but not hard_limits) is caught.
func (c *BotConfig) ValidateAgainstHardLimits(limits *HardLimits) error {
	if limits == nil {
		return fmt.Errorf("hard_limits is nil")
	}
	symbols := c.ResolveSymbols()
	if len(symbols) == 0 {
		return fmt.Errorf("bot_config has no symbols to validate")
	}
	for _, sym := range symbols {
		if !limits.SymbolAllowed(sym) {
			return fmt.Errorf("bot_config symbol %q not in hard_limits.allowed_symbols=%v",
				sym, limits.AllowedSymbols)
		}
	}
	return nil
}

func LoadBotConfig(path string) (*BotConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bot_config: %w", err)
	}
	var cfg BotConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse bot_config: %w", err)
	}
	cfg.Normalize()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validate bot_config: %w", err)
	}
	return &cfg, nil
}

func (c *BotConfig) validate() error {
	if !c.Bot.Mode.Valid() {
		return fmt.Errorf("invalid bot.mode: %q", c.Bot.Mode)
	}
	if len(c.Symbols) == 0 && c.Symbol == "" {
		return fmt.Errorf("symbols (or legacy symbol) is required")
	}
	for i, s := range c.Symbols {
		if s == "" {
			return fmt.Errorf("symbols[%d] is empty", i)
		}
	}
	if c.AIAdvisor.IntervalMinutes <= 0 {
		return fmt.Errorf("ai_advisor.interval_minutes must be > 0")
	}
	if c.AIAdvisor.ConfigTTLMinutes <= 0 {
		return fmt.Errorf("ai_advisor.config_ttl_minutes must be > 0")
	}
	if c.AIAdvisor.ClaudeCLITimeoutSeconds <= 0 {
		return fmt.Errorf("ai_advisor.claude_cli_timeout_seconds must be > 0")
	}
	if c.AIAdvisor.PromptPath == "" || c.AIAdvisor.InputPath == "" || c.AIAdvisor.OutputPath == "" {
		return fmt.Errorf("ai_advisor paths must be set")
	}
	if c.LLMDecision.Enabled && c.LLMDecision.ReflectionStartAt != "" {
		if _, err := time.Parse(time.RFC3339, c.LLMDecision.ReflectionStartAt); err != nil {
			return fmt.Errorf("llm_decision.reflection_start_at must be RFC3339 (e.g. 2026-06-19T16:00:00Z): %w", err)
		}
	}
	// Per-currency entry vetoes: a typo'd value must fail LOUDLY at boot, not
	// silently disable the gate (hour 24 never matches jstHourIn; an rpos bound outside [0,1]
	// can never trip — the operator would believe a protection is on when it is a no-op).
	for _, h := range c.LLMDecision.NightBuyVetoHoursJST {
		if h < 0 || h > 23 {
			return fmt.Errorf("llm_decision.night_buy_veto_hours_jst: hour %d out of range 0-23", h)
		}
	}
	// Session guard: same loud-fail rule as the vetoes above.
	for _, h := range c.LLMDecision.NoEntryHoursJST {
		if h < 0 || h > 23 {
			return fmt.Errorf("llm_decision.no_entry_hours_jst: hour %d out of range 0-23", h)
		}
	}
	if s := c.LLMDecision.SessionFlattenJST; s != "" {
		if _, err := parseMinutesOfDay(s); err != nil {
			return fmt.Errorf("llm_decision.session_flatten_jst: %w", err)
		}
	}
	if c.Risk.ReentryCooldownMinutes < 0 {
		return fmt.Errorf("risk.reentry_cooldown_minutes: %d must be >= 0 (0 = off)", c.Risk.ReentryCooldownMinutes)
	}
	for sym, v := range c.LLMDecision.MaxRangePos24hBuy {
		if v < 0 || v > 1 {
			return fmt.Errorf("llm_decision.max_range_position_24h_buy[%s]: %v out of range 0-1 (range position is a fraction; 0 = off)", sym, v)
		}
	}
	for sym, v := range c.LLMDecision.MinRangePos24hSell {
		if v < 0 || v > 1 {
			return fmt.Errorf("llm_decision.min_range_position_24h_sell[%s]: %v out of range 0-1 (range position is a fraction; 0 = off)", sym, v)
		}
	}
	// Per-symbol order size: a negative size is always a typo — fail loudly
	// at boot rather than let it flow toward the order path.
	for sym, q := range c.LLMDecision.QuantityBySymbol {
		if q < 0 {
			return fmt.Errorf("llm_decision.quantity_by_symbol[%s]: %d must be >= 0 (0 = fall back to llm_decision.quantity)", sym, q)
		}
	}
	// MTF-veto discipline exemption: same loud-fail rule — an exempt rpos of
	// 60 instead of 0.60 would waive the veto for EVERY sell, a typo'd negative for no buy.
	for sym, v := range c.LLMDecision.HTFTrendVetoExemptSellRpos {
		if v < 0 || v > 1 {
			return fmt.Errorf("llm_decision.htf_trend_veto_exempt_sell_rpos[%s]: %v out of range 0-1 (range position is a fraction; 0 = off)", sym, v)
		}
	}
	for sym, v := range c.LLMDecision.HTFTrendVetoExemptBuyRpos {
		if v < 0 || v > 1 {
			return fmt.Errorf("llm_decision.htf_trend_veto_exempt_buy_rpos[%s]: %v out of range 0-1 (range position is a fraction; 0 = off)", sym, v)
		}
	}
	// Event retrigger: negatives are typos — fail loudly at boot.
	er := c.LLMDecision.EventRetrigger
	if er.MovePips < 0 {
		return fmt.Errorf("llm_decision.event_retrigger.move_pips: %v must be >= 0 (0 = move trigger off)", er.MovePips)
	}
	if er.MoveWindowMinutes < 0 {
		return fmt.Errorf("llm_decision.event_retrigger.move_window_minutes: %d must be >= 0 (0 = default 30)", er.MoveWindowMinutes)
	}
	if er.MoveCooldownMinutes < 0 {
		return fmt.Errorf("llm_decision.event_retrigger.move_cooldown_minutes: %d must be >= 0 (0 = default 20)", er.MoveCooldownMinutes)
	}
	if er.CloseCooldownMinutes < 0 {
		return fmt.Errorf("llm_decision.event_retrigger.close_cooldown_minutes: %d must be >= 0 (0 = default 3)", er.CloseCooldownMinutes)
	}
	return nil
}
