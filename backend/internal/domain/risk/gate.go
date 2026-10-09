// Package risk contains pre-trade risk gate logic. Like strategy, this is pure
// — usecase/evaluate_entry assembles the inputs and calls Gate.Evaluate.
package risk

import (
	"fmt"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/strategy"
)

// Decision is the outcome of running the risk gate on a Signal.
//
// QtyMultiplier scales cfg.Risk.Quantity for the entry. 0 (zero-value)
// means "use cfg.Risk.Quantity as-is" — callers should treat <=0 as 1.0.
// EvaluateSignal currently always returns 1.0 on an allowed entry (loss
// streaks stop entries rather than shrink them). Only meaningful when Allowed=true.
type Decision struct {
	Allowed       bool
	Reason        string
	QtyMultiplier float64
}

// AccountSnapshot is the live trading state passed to Evaluate. It mirrors
// config.AccountState plus per-window aggregates the gate needs.
//
// Cooldown fields are pre-computed by the worker from the last
// closed trade + HardLimits.Cooldown. The gate stays pure: it only reads
// the flag.
type AccountSnapshot struct {
	EmergencyStop bool

	// Per-symbol aggregates. Filled from *BySymbol repository queries by
	// Worker.accountSnapshot / EntryAdmission.snapshot.
	OpenPositions        int
	TradesInWindow       int
	LossInWindowJPY      int
	DailyLossJPY         int
	MaxDailyLossJPY      int
	ConsecutiveLosses    int
	MaxConsecutiveLosses int

	// Account-wide aggregates (multi-symbol). Filled from the account-wide
	// (non-BySymbol) repository methods + Positions.CountOpenAllSymbols.
	// AccountMax* = 0 disables the gate (single-symbol back-compat).
	AccountOpenPositions    int
	AccountDailyLossJPY     int
	AccountMaxOpenPositions int
	AccountMaxDailyLossJPY  int

	// InCooldown=true means a recently-closed trade has not yet aged past
	// HardLimits.Cooldown.{AfterEntry,Loss,TakeProfit}. CooldownUntil and
	// CooldownKind populate the rejection reason for observability.
	InCooldown    bool
	CooldownUntil time.Time
	CooldownKind  string // "after_entry" | "after_loss" | "after_take_profit"

	// Now is the snapshot time. Used for time-based gate decisions such as
	// the graduated consecutive-loss freeze. The caller (worker /
	// admission) sets it from its clock so EvaluateSignal stays pure.
	Now time.Time

	// LastLossClosedAt is the closed_at of the most-recent losing trade in
	// the current consecutive-loss streak (per-symbol, today). Zero when
	// ConsecutiveLosses == 0. Combined with snap.Now to implement the
	// 30-minute freeze after 3 consecutive losses.
	LastLossClosedAt time.Time

	// BuyStopLossesToday / SellStopLossesToday count CloseReason="stop_loss"
	// trades by side since start-of-day.
	// 2 SL hits の方向は当日 no_trade (= 同方向リトライ禁止)。早期 exit の小損は
	// 数えない (SL 限定)。filled by worker / admission snapshot builders.
	BuyStopLossesToday  int
	SellStopLossesToday int

	// ConsecutiveLossGuardsDisabled neutralizes EVERY loss-streak throttle in
	// one switch: the MaxConsecutiveLosses binary cap, the 3-loss 30-min freeze
	// and the same-direction 2-SL daily block. The daily_loss /
	// account_daily_loss caps remain the only blow-up brake.
	// Set from bot_config.risk.disable_consecutive_loss_guards (連敗系ガードを一括で
	// 無効化する switch。既定 false。無効化中は daily_loss / account_daily_loss cap だけが歯止め)。
	ConsecutiveLossGuardsDisabled bool

	// PostLossFreeze freezes NEW entries on this symbol for
	// this duration after ANY losing close (per-symbol, threshold=1). It reuses
	// LastLossClosedAt (non-zero only when the symbol's most-recent closed trade
	// is a loss — any negative PnL incl. ratchet_stoploss). It is INDEPENDENT of
	// ConsecutiveLossGuardsDisabled: even with the loss-streak guards off, the
	// immediate revenge re-entry right after a stopout (損切り直後の即リエントリー
	// がウィップソーで連敗する形) stays blocked. Zero = disabled.
	// Set from bot_config.risk.post_loss_freeze_minutes.
	PostLossFreeze time.Duration

	// ReentryCooldown rejects a NEW entry on the same
	// symbol+side for this duration after ANY close (win or loss, every close
	// reason). 決済直後(利確直後を含む)に同 symbol 同 side へ即再 IN して
	// 往復で SL を踏む形を防ぐ。PostLossFreeze(負け限定・両 side)とは独立で、
	// 勝ち直後の飛び乗りも塞ぐ。Last{Buy,Sell}ClosedAt は per-symbol の当日
	// (bot.timezone の 0:00 境界 = config.StartOfDayIn) 決済から導出される — 日境界を跨いだ持ち越しはしない
	// (新セッションの初回判断は新鮮な判断として扱う)。Zero = disabled。
	// Set from bot_config.risk.reentry_cooldown_minutes.
	ReentryCooldown  time.Duration
	LastBuyClosedAt  time.Time // most-recent BUY close (any reason) today, zero = none
	LastSellClosedAt time.Time // most-recent SELL close (any reason) today, zero = none

	// InEventFreeze は EventCalendar.InFreezeWindow(now) の結果。
	// 経済指標 (NFP / FOMC 等) の発表前後 freeze
	// 窓内なら true。EvaluateSignal + EvaluateHardSafety どちらも reject する
	// hard gate (operator override 不可 — 指標直撃の爆損を防ぐため)。
	InEventFreeze bool

	// OpenBuyInclExternal / OpenSellInclExternal count OPEN+CLOSING positions
	// by side INCLUDING external (GMO-app) positions. Same-
	// symbol same-side pyramiding is blocked off these counts, independent of
	// MaxOpenPositions (which stops being a pyramiding guard once the cap is
	// raised above 1). Unlike OpenPositions (external excluded for capacity), a
	// same-side external position MUST block the bot from stacking the same way.
	OpenBuyInclExternal  int
	OpenSellInclExternal int
}

// Consecutive-loss cooldown thresholds.
// 連敗で qty を減らすステップは持たない。あるのは 3 連敗 30 分
// freeze と、snap.MaxConsecutiveLosses による binary cap
// (bot_config.risk.max_consecutive_losses で operator-tunable)。
const (
	consecutiveLossFreezeThreshold = 3
	consecutiveLossFreezeDuration  = 30 * time.Minute
)

// 当日 SL を 2 回踏んだ方向は no_trade (= 同方向リトライ禁止)。
const sameDirectionStopLossDailyBlockThreshold = 2

// EvaluateSignal returns Allowed=true only if the proposed entry passes every
// guard. The caller (usecase) records rejected entries to signal_rejections.
//
// Checks (order matters for the most-informative reason):
//  1. signal not an entry → trivially allowed (no order to place)
//  2. config nil → reject (expiry is enforced upstream: the strategy engine
//     returns NoTrade for an expired config, so no entry signal reaches here)
//  3. emergency_stop flag → reject
//  4. event_freeze → reject
//  5. cooldown → reject
//  6. daily_loss / account_daily_loss caps → reject
//  7. consecutive-loss cap + 3-loss freeze (skipped if ConsecutiveLossGuardsDisabled) → reject
//  8. post_loss_freeze → reject
//  9. reentry_cooldown → reject
//  10. open positions (max(cfg cap, sig.MaxConcurrent)) / account_open_positions → reject
//  11. pyramiding (same symbol same side, incl. external) → reject
//  12. trades_in_window / loss_in_window → reject
//  13. direction mismatch + same-direction 2-SL daily block → reject
//  14. spread vs config.Entry.MaxSpreadPips (re-check after engine) → reject
func EvaluateSignal(sig strategy.Signal, cfg *config.StrategyConfig, snap AccountSnapshot, summary *market.MarketSummary) Decision {
	if !sig.IsEntry() {
		return Decision{Allowed: true}
	}
	if cfg == nil {
		return Decision{Reason: "no_active_config"}
	}
	if snap.EmergencyStop {
		return Decision{Reason: "emergency_stop"}
	}
	// 経済指標 freeze 窓内なら hard reject。emergency_stop と
	// 並ぶ「市場が危険状態」シグナルとして扱う (operator override 不可)。
	if snap.InEventFreeze {
		return Decision{Reason: "event_freeze (経済指標発表帯)"}
	}
	if snap.InCooldown {
		return Decision{Reason: fmt.Sprintf("cooldown %s until %s",
			snap.CooldownKind, snap.CooldownUntil.Format("15:04:05"))}
	}
	// DailyLossJPY is GROSS realized loss (Σ|negative closed
	// PnL| via SumClosedLossJPYSince) — the conservative measure, NOT signed net.
	// This is the canonical "daily loss" definition; ops DailyDDAlert is aligned
	// to it. A net+unrealized+SL-projected form is deliberately not used, to
	// keep the only live brake simple and stable.
	if snap.MaxDailyLossJPY > 0 && snap.DailyLossJPY >= snap.MaxDailyLossJPY {
		return Decision{Reason: fmt.Sprintf("daily_loss %d >= cap %d", snap.DailyLossJPY, snap.MaxDailyLossJPY)}
	}
	if snap.AccountMaxDailyLossJPY > 0 && snap.AccountDailyLossJPY >= snap.AccountMaxDailyLossJPY {
		return Decision{Reason: fmt.Sprintf("account_daily_loss %d >= cap %d",
			snap.AccountDailyLossJPY, snap.AccountMaxDailyLossJPY)}
	}
	// 連敗系ガード (binary cap / 3 連敗 freeze / 同方向 2SL direction
	// block) は ConsecutiveLossGuardsDisabled で一括バイパスできる。
	if !snap.ConsecutiveLossGuardsDisabled {
		if snap.MaxConsecutiveLosses > 0 && snap.ConsecutiveLosses >= snap.MaxConsecutiveLosses {
			return Decision{Reason: fmt.Sprintf("consecutive_losses %d >= cap %d", snap.ConsecutiveLosses, snap.MaxConsecutiveLosses)}
		}
		// 3 連敗で 30 分 freeze (cap 未到達でも一旦止める)。
		// Reason は admission.isOverridableReason ("consecutive_losses" prefix)
		// に整合する形にする — 手動オーバーライドは可。
		if snap.ConsecutiveLosses >= consecutiveLossFreezeThreshold && !snap.LastLossClosedAt.IsZero() && !snap.Now.IsZero() {
			freezeUntil := snap.LastLossClosedAt.Add(consecutiveLossFreezeDuration)
			if snap.Now.Before(freezeUntil) {
				return Decision{Reason: fmt.Sprintf("consecutive_losses_freeze until %s (count=%d)",
					freezeUntil.Format("15:04:05"), snap.ConsecutiveLosses)}
			}
		}
	}
	// Post-loss freeze: per-symbol, fires after ANY single
	// losing close (threshold=1) and is INDEPENDENT of the consecutive-loss
	// bundle above — even with that bundle disabled, the immediate revenge
	// re-entry after a stopout stays blocked. LastLossClosedAt is non-zero only when
	// this symbol's most-recent closed trade is a loss (any negative PnL incl.
	// ratchet_stoploss), so a subsequent win clears the freeze automatically.
	// Reason "post_loss_freeze" is operator-overridable (isOverridableReason).
	if snap.PostLossFreeze > 0 && !snap.LastLossClosedAt.IsZero() && !snap.Now.IsZero() {
		freezeUntil := snap.LastLossClosedAt.Add(snap.PostLossFreeze)
		if snap.Now.Before(freezeUntil) {
			return Decision{Reason: fmt.Sprintf("post_loss_freeze until %s", freezeUntil.Format("15:04:05"))}
		}
	}
	// Reentry cooldown: same symbol+side blocked for
	// ReentryCooldown after ANY close — event_retrigger 等による「利確直後の即再 IN」を
	// 塞ぐ。勝ち後も対象な点が
	// PostLossFreeze との違い。Reason prefix "reentry_cooldown" は admission の
	// isOverridableReason allowlist に載せない = このゲートで拒否された手動 entry は
	// override できない(自動経路と同じ再エントリー冷却を手動経路にも課すため)。
	// EvaluateHardSafety でも再検査するので、先行する allowlist ゲートを override しても効く。
	if r := reentryCooldownReason(sig, snap); r != "" {
		return Decision{Reason: r}
	}
	// The per-symbol open-positions cap must honor sig.MaxConcurrent too, else it would block the
	// add-to-winner pyramid BEFORE the relaxed same-side block below ever runs: an engine config with
	// max_open_positions:1 (e.g. trend-v4), so a 2nd advisor_v2 entry would be rejected here as
	// "open_positions 1 >= cap 1" and the pyramid would be inert. effMaxOpen = max(cfg cap, MaxConcurrent)
	// keeps the strict single-position rule for every strategy with MaxConcurrent 0/1.
	effMaxOpen := cfg.Risk.MaxOpenPositions
	if sig.MaxConcurrent > effMaxOpen {
		effMaxOpen = sig.MaxConcurrent
	}
	if effMaxOpen > 0 && snap.OpenPositions >= effMaxOpen {
		return Decision{Reason: fmt.Sprintf("open_positions %d >= cap %d", snap.OpenPositions, effMaxOpen)}
	}
	if r := accountOpenPositionsReason(snap); r != "" {
		return Decision{Reason: r}
	}
	// Same-symbol same-side pyramiding block, independent of the
	// MaxOpenPositions cap (which stops guarding pyramiding once it is raised
	// to 2-3). external (app-side) same-side positions count. Hard guard
	// (reason not in isOverridableReason allowlist) — it is the deliberate
	// replacement for the cap=1 implicit wall, not a soft operator gate. It is
	// re-checked by EvaluateHardSafety, so an operator override of an earlier
	// allowlisted gate (e.g. open_positions) cannot stack a same-side position.
	//
	// sig.MaxConcurrent relaxes the cap for a strategy that explicitly allows adding to a winner
	// (advisor v2 pyramid). 0/1 keeps the strict single-position rule; N permits up to N same-side.
	// The STRATEGY is responsible for only emitting the add when safe (advisor_v2: 1st's ratchet armed) —
	// the gate just enforces the hard ceiling so an unbounded stack can never happen.
	if r := pyramidingReason(sig, snap); r != "" {
		return Decision{Reason: r}
	}
	if cfg.Risk.MaxTradesInThisWindow > 0 && snap.TradesInWindow >= cfg.Risk.MaxTradesInThisWindow {
		return Decision{Reason: fmt.Sprintf("trades_in_window %d >= cap %d", snap.TradesInWindow, cfg.Risk.MaxTradesInThisWindow)}
	}
	if cfg.Risk.MaxLossInThisWindowJPY > 0 && snap.LossInWindowJPY >= cfg.Risk.MaxLossInThisWindowJPY {
		return Decision{Reason: fmt.Sprintf("loss_in_window %d >= cap %d", snap.LossInWindowJPY, cfg.Risk.MaxLossInThisWindowJPY)}
	}
	dir := cfg.Entry.Direction
	if dir == config.DirectionNone {
		return Decision{Reason: "direction_none"}
	}
	switch dir {
	case config.DirectionBuyOnly:
		if sig.Side != "BUY" {
			return Decision{Reason: "direction_buy_only_blocks_short"}
		}
	case config.DirectionSellOnly:
		if sig.Side != "SELL" {
			return Decision{Reason: "direction_sell_only_blocks_long"}
		}
	}
	// 同方向 SL 2 回で当日その方向を block。operator 手動オーバーライドは可
	// (reason が "direction_" prefix なので isOverridableReason allowlist に match)。
	// ConsecutiveLossGuardsDisabled でバイパス可。
	if !snap.ConsecutiveLossGuardsDisabled {
		if sig.Side == "BUY" && snap.BuyStopLossesToday >= sameDirectionStopLossDailyBlockThreshold {
			return Decision{Reason: fmt.Sprintf("direction_buy_blocked_after_%d_sl_today (count=%d)",
				sameDirectionStopLossDailyBlockThreshold, snap.BuyStopLossesToday)}
		}
		if sig.Side == "SELL" && snap.SellStopLossesToday >= sameDirectionStopLossDailyBlockThreshold {
			return Decision{Reason: fmt.Sprintf("direction_sell_blocked_after_%d_sl_today (count=%d)",
				sameDirectionStopLossDailyBlockThreshold, snap.SellStopLossesToday)}
		}
	}
	if summary != nil && cfg.Entry.MaxSpreadPips > 0 && summary.CurrentRate.SpreadPips > cfg.Entry.MaxSpreadPips {
		return Decision{Reason: fmt.Sprintf("spread %.2f > cap %.2f", summary.CurrentRate.SpreadPips, cfg.Entry.MaxSpreadPips)}
	}
	// 連敗しても qty は減らさず full qty で発注する。連敗への対処は
	// cap / 30分freeze / 同方向2SLブロック (ロットを減らさず「止める」系) が担う。
	// QtyMultiplier は常に 1.0 (=no scaling)。
	return Decision{Allowed: true, QtyMultiplier: 1.0}
}

// EvaluateHardSafety re-checks ONLY the never-overridable gates and skips
// the operator-overridable ones (cooldown / consecutive_losses /
// open_positions / trades_in_window / loss_in_window / direction_* /
// spread).
//
// Hard gates: no_active_config / emergency_stop / event_freeze / daily_loss /
// account_daily_loss / reentry_cooldown / account_open_positions /
// pyramiding (same symbol same side, external positions included).
//
// Why a second pass: EvaluateSignal short-circuits, so when an
// overridable reason matches first, an override path that relied on it alone
// would fly past subsequent hard rejections. Admission calls this function
// as a second pass — AllowOverride cannot bypass the hard reasons.
//
// `spread` is deliberately NOT a hard-safety gate; it is on the
// operator-override allowlist. The dashboard surfaces the live spread
// next to the manual trade button, so the operator pressing BUY/SELL is
// explicitly accepting the per-trade cost. Spread is a cost gate, not a
// systemic-safety gate (unlike emergency_stop / daily_loss). Auto entry
// is unaffected because it passes AllowOverride=false and never reaches
// the override path.
func EvaluateHardSafety(sig strategy.Signal, cfg *config.StrategyConfig, snap AccountSnapshot, summary *market.MarketSummary) Decision {
	if !sig.IsEntry() {
		return Decision{Allowed: true}
	}
	if cfg == nil {
		return Decision{Reason: "no_active_config"}
	}
	if snap.EmergencyStop {
		return Decision{Reason: "emergency_stop"}
	}
	// event_freeze は emergency と同列 hard gate なので
	// override 用 HardSafety パスでも reject する。
	if snap.InEventFreeze {
		return Decision{Reason: "event_freeze (経済指標発表帯)"}
	}
	if snap.MaxDailyLossJPY > 0 && snap.DailyLossJPY >= snap.MaxDailyLossJPY {
		return Decision{Reason: fmt.Sprintf("daily_loss %d >= cap %d", snap.DailyLossJPY, snap.MaxDailyLossJPY)}
	}
	if snap.AccountMaxDailyLossJPY > 0 && snap.AccountDailyLossJPY >= snap.AccountMaxDailyLossJPY {
		return Decision{Reason: fmt.Sprintf("account_daily_loss %d >= cap %d",
			snap.AccountDailyLossJPY, snap.AccountMaxDailyLossJPY)}
	}
	// The remaining never-overridable gates. EvaluateSignal stops at the first failing gate, so an
	// override of an earlier allowlisted gate (e.g. open_positions) would otherwise skip these.
	if r := reentryCooldownReason(sig, snap); r != "" {
		return Decision{Reason: r}
	}
	if r := accountOpenPositionsReason(snap); r != "" {
		return Decision{Reason: r}
	}
	if r := pyramidingReason(sig, snap); r != "" {
		return Decision{Reason: r}
	}
	return Decision{Allowed: true}
}

// reentryCooldownReason blocks the same symbol+side for snap.ReentryCooldown after ANY close of that
// side (win or loss). Empty when the entry is allowed.
func reentryCooldownReason(sig strategy.Signal, snap AccountSnapshot) string {
	if snap.ReentryCooldown <= 0 || snap.Now.IsZero() {
		return ""
	}
	lastSameSide := snap.LastBuyClosedAt
	if sig.Side == "SELL" {
		lastSameSide = snap.LastSellClosedAt
	}
	if lastSameSide.IsZero() {
		return ""
	}
	if until := lastSameSide.Add(snap.ReentryCooldown); snap.Now.Before(until) {
		return fmt.Sprintf("reentry_cooldown %s until %s", sig.Side, until.Format("15:04:05"))
	}
	return ""
}

// accountOpenPositionsReason enforces the account-wide open-positions cap (0 = disabled).
func accountOpenPositionsReason(snap AccountSnapshot) string {
	if snap.AccountMaxOpenPositions > 0 && snap.AccountOpenPositions >= snap.AccountMaxOpenPositions {
		return fmt.Sprintf("account_open_positions %d >= cap %d",
			snap.AccountOpenPositions, snap.AccountMaxOpenPositions)
	}
	return ""
}

// pyramidingReason is the no-nanpin rule: a same-symbol same-side open position (external positions
// included) blocks a new entry on that side. sig.MaxConcurrent > 1 lets a strategy that explicitly
// adds to a winner hold up to N same-side positions; 0/1 keeps the strict single-position rule.
func pyramidingReason(sig strategy.Signal, snap AccountSnapshot) string {
	maxSameSide := sig.MaxConcurrent
	if maxSameSide < 1 {
		maxSameSide = 1
	}
	if sig.Side == "BUY" && snap.OpenBuyInclExternal >= maxSameSide {
		return fmt.Sprintf("pyramiding_blocked_same_side_buy (%d open incl external, cap %d)", snap.OpenBuyInclExternal, maxSameSide)
	}
	if sig.Side == "SELL" && snap.OpenSellInclExternal >= maxSameSide {
		return fmt.Sprintf("pyramiding_blocked_same_side_sell (%d open incl external, cap %d)", snap.OpenSellInclExternal, maxSameSide)
	}
	return ""
}
