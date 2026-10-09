package command

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/risk"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/safety"
)

// EntryAdmission is the single, authoritative pre-trade gate for ALL entry
// paths (auto-signal from the price loop, manual entries from the dashboard).
//
// Why one gate under one lock: if the auto path built its risk snapshot and ran
// the gate first and only then acquired EntryMutex around broker.PlaceOrder, the
// race window between "snapshot built" and "mutex acquired" would let a manual
// entry slip a position in and silently push the auto path past
// MaxOpenPositions; and a manual path that skipped the gate could bypass
// emergency_stop / daily_loss / cooldown altogether.
//
// CheckAndHold acquires the shared lock, RE-reads live state from the
// repository, runs the gate, and either:
//   - returns Allowed=false (lock released, caller must NOT trade)
//   - returns Allowed=true + a release callback the caller MUST call
//     immediately after PlaceOrder (success OR error)
//
// All callers (ExecuteOrder.OnSignal, ManualTradeCommand.Execute) hold the
// lock from "gate-pass" through "PlaceOrder return" so no concurrent entry
// can sneak past the gate.
type EntryAdmission struct {
	// Symbol pins this admission gate to one trading symbol. Multi-symbol
	// bundles each create their own EntryAdmission with the bundle's symbol
	// so snapshot() reads per-symbol positions/trades correctly.
	// Empty falls back to BotConfig.Symbol (single-symbol legacy).
	Symbol            string
	Mutex             *sync.Mutex
	Positions         port.PositionRepository
	Trades            port.TradeRepository
	BotConfig         *config.BotConfig
	HardLimits        *config.HardLimits
	EmergencyFlagPath string
	Logger            *slog.Logger
	Clock             func() time.Time

	// CooldownFn is the cooldown evaluator shared with the worker pre-gate
	// (C.5 fix). When non-nil, it is invoked under the admission lock to
	// re-evaluate cooldown freshly — a close that landed between worker's
	// pre-tick check and the admission lock is reflected here. Returns
	// (inCooldown, until, kind). nil → cooldown gate skipped (legacy).
	//
	// Wiring (cmd/bot/main.go) passes the same `app.ComputeCooldown` that
	// Worker.accountSnapshot uses, so both paths see identical state.
	CooldownFn func(now time.Time) (bool, time.Time, string)

	// EventCalendar は経済指標発表帯 freeze 用。
	// snapshot 時に InFreezeWindow(now) を問い合わせて
	// risk.AccountSnapshot.InEventFreeze に詰める。nil 許容 (= freeze 無効)。
	// Worker.EventCalendar と同じ instance を cmd/bot から渡す。
	EventCalendar *config.EventCalendar
}

// AdmissionRequest is what the caller asks: "may I open this position?".
//
// Manual entries MAY pass AllowOverride=true (explicit operator opt-in,
// default false) to bypass the allowlisted soft gates (isOverridableReason).
// EmergencyStop and the other hard gates (daily_loss etc.) are never bypassable.
type AdmissionRequest struct {
	Signal       strategy.Signal
	ActiveConfig *config.StrategyConfig
	Summary      *market.MarketSummary
	// Ticker is the live bid/ask used by admission to derive SpreadPips for
	// the final spread gate (C.5 fix). When non-nil, admission constructs a
	// minimal MarketSummary so risk.EvaluateSignal can re-check
	// `cfg.Entry.MaxSpreadPips` even if the caller didn't pre-compute a
	// full MarketSummary. nil → spread gate skipped (legacy compat —
	// integration tests without ticker data).
	Ticker        *market.Ticker
	AllowOverride bool   // manual=true allows operator-override-able rejections through
	Source        string // "auto" | "manual" — for audit logging
}

// AdmissionVerdict is the result. Reason is empty when Allowed=true.
//
// QtyMultiplier is the scaling factor the executor must apply to sig.Quantity
// before placing the order. 0 / 1.0 / 未設定 = scaling なし (現在の risk gate は
// 常に 1.0 を返す)。OnSignal が applyQtyMultiplier で broker min floor 付きで適用する。
type AdmissionVerdict struct {
	Allowed       bool
	Reason        string
	QtyMultiplier float64
}

// CheckAndHold runs the gate under lock. The caller must call release()
// after PlaceOrder if Allowed=true.
func (a *EntryAdmission) CheckAndHold(ctx context.Context, req AdmissionRequest) (AdmissionVerdict, func(), error) {
	if a.Mutex != nil {
		a.Mutex.Lock()
	}
	release := func() {
		if a.Mutex != nil {
			a.Mutex.Unlock()
		}
	}

	// Re-read state INSIDE the lock so a concurrent entry that just
	// committed a new position is reflected here.
	snap, err := a.snapshot(ctx, req.ActiveConfig)
	if err != nil {
		release()
		return AdmissionVerdict{}, nil, fmt.Errorf("admission snapshot: %w", err)
	}

	// EmergencyStop is checked first and is never bypassable (the other hard
	// gates are re-checked below via EvaluateHardSafety).
	if snap.EmergencyStop {
		release()
		return AdmissionVerdict{Reason: "emergency_stop"}, nil, nil
	}

	// C.5 fix: build a synthetic minimal MarketSummary from the live Ticker
	// when the caller didn't supply a Summary, so the spread gate in
	// risk.EvaluateSignal can re-check freshness under the admission lock.
	summary := req.Summary
	if summary == nil && req.Ticker != nil {
		summary = &market.MarketSummary{
			CurrentRate: market.CurrentRate{
				Bid:        req.Ticker.Bid,
				Ask:        req.Ticker.Ask,
				SpreadPips: req.Ticker.SpreadPips(market.PipSize(a.BotConfig.Symbol)),
				Timestamp:  req.Ticker.Timestamp,
			},
		}
	}

	// All other gates: re-use the domain logic so engine + admission stay
	// in agreement. AllowOverride suppresses the gates listed in
	// isOverridableReason (open_positions, cooldown, consecutive_losses,
	// post_loss_freeze, trades_in_window, loss_in_window, direction_*, spread).
	// It does NOT suppress daily_loss / emergency_stop / event_freeze /
	// no_active_config (= hard safety, re-checked below).
	d := risk.EvaluateSignal(req.Signal, req.ActiveConfig, snap, summary)
	if !d.Allowed {
		if !req.AllowOverride || !isOverridableReason(d.Reason) {
			release()
			a.logRejection(req, d.Reason)
			return AdmissionVerdict{Reason: d.Reason}, nil, nil
		}
		// Operator override path. EvaluateSignal short-circuits on the
		// first matching reason, so a hard rejection (emergency_stop /
		// daily_loss / event_freeze) lurking later in the chain would be
		// silently skipped. So run a second-
		// pass hard-safety check that AllowOverride cannot bypass (it also
		// re-checks reentry_cooldown, the same-side pyramiding block and the
		// account-wide open-positions cap).
		if hard := risk.EvaluateHardSafety(req.Signal, req.ActiveConfig, snap, summary); !hard.Allowed {
			release()
			a.logRejection(req, hard.Reason)
			return AdmissionVerdict{Reason: hard.Reason}, nil, nil
		}
		// Manual override path — log so the audit trail captures it.
		// Required fields: source,
		// reason, side, config_id (plus original_reason for traceability
		// when the reason was reformatted from a domain string).
		if a.Logger != nil {
			a.Logger.Warn("admission_manual_override",
				"source", req.Source,
				"reason", d.Reason,
				"side", string(req.Signal.Side),
				"config_id", req.Signal.ConfigID)
		}
	}
	return AdmissionVerdict{Allowed: true, QtyMultiplier: d.QtyMultiplier}, release, nil
}

func (a *EntryAdmission) logRejection(req AdmissionRequest, reason string) {
	if a.Logger == nil {
		return
	}
	// Warn (not Info): rejections are the audit trail of "the bot wanted to trade
	// and was refused" — they must survive LOG_LEVEL=warn (an Info-level rejection is
	// invisible in production and a silently refusing gate looks like "no signal").
	a.Logger.Warn("entry_admission_rejected",
		"reason", reason, "source", req.Source,
		"side", string(req.Signal.Side), "config_id", req.Signal.ConfigID)
}

// isOverridableReason returns true ONLY for rejection reasons explicitly on
// the operator override allowlist. The function is an explicit allowlist —
// new reasons default to NOT overridable until proven safe.
//
// direction_* is on the allowlist: when an operator explicitly confirms a manual
// entry in the dashboard while the active config is no_trade or direction-
// constrained, the operator's discretionary view supersedes Claude's
// strategy recommendation.
//
// `spread` is on the override allowlist too.
// The dashboard surfaces the live spread next to the manual trade button,
// so the operator pressing BUY/SELL is explicitly accepting the per-trade
// cost (qty pips * pip_size yen). Spread is a cost decision, not a
// systemic-safety one — unlike emergency_stop / daily_loss which can
// indicate the bot or the account is in an unsafe state. Auto entry is
// unaffected because it passes AllowOverride=false and is rejected by
// EvaluateSignal before override is consulted.
//
// Allowlist (safe to override with operator opt-in):
//   - open_positions      : operator chooses to add a position past cap
//   - cooldown            : operator skips post-trade quiet period
//   - consecutive_losses  : operator overrides the losing-streak halt
//   - post_loss_freeze    : operator overrides the post-loss freeze
//   - trades_in_window    : operator overrides the per-window trade cap
//   - loss_in_window      : operator overrides the per-window loss cap
//   - direction_none      : operator overrides Claude's no_trade judgment
//   - direction_buy_only_*: operator places a SELL despite buy-only policy
//   - direction_sell_only_*: operator places a BUY despite sell-only policy
//   - spread              : operator accepts the current cost (per above)
//
// NEVER overridable (hard safety, re-checked by risk.EvaluateHardSafety):
//   - emergency_stop, no_active_config, event_freeze, daily_loss / account_daily_loss,
//     reentry_cooldown, pyramiding_blocked_* (no nanpin), account_open_positions
func isOverridableReason(reason string) bool {
	switch {
	case strings.HasPrefix(reason, "open_positions"):
		return true
	case strings.HasPrefix(reason, "cooldown"):
		return true
	case strings.HasPrefix(reason, "consecutive_losses"):
		return true
	case strings.HasPrefix(reason, "post_loss_freeze"):
		return true
	case strings.HasPrefix(reason, "trades_in_window"):
		return true
	case strings.HasPrefix(reason, "loss_in_window"):
		return true
	case strings.HasPrefix(reason, "direction_"):
		return true
	case strings.HasPrefix(reason, "spread "):
		// Note the trailing space — the reason format is "spread X.XX > cap Y.YY"
		// so this matches the spread gate without colliding with any future
		// reason that happens to start with the word "spread".
		return true
	default:
		return false
	}
}

// snapshot builds the same AccountSnapshot the worker would, but READ
// FRESH under the admission lock so we see committed entries from any
// just-released competing caller.
func (a *EntryAdmission) snapshot(ctx context.Context, active *config.StrategyConfig) (risk.AccountSnapshot, error) {
	if a.Clock == nil {
		a.Clock = time.Now
	}
	now := a.Clock()

	// Resolve this admission's symbol. Multi-symbol bundles set
	// EntryAdmission.Symbol explicitly; legacy single-symbol callers fall
	// back to BotConfig.Symbol (Normalize() keeps that field populated).
	sym := a.Symbol
	if sym == "" {
		sym = a.BotConfig.Symbol
	}

	// Include OPEN + CLOSING — CLOSING positions still hold a slot at the
	// broker until the saga completes, so capacity gate must count them.
	open, err := a.Positions.ListOpenOrClosing(ctx, sym)
	if err != nil {
		return risk.AccountSnapshot{}, fmt.Errorf("list_open: %w", err)
	}
	// External positions (opened by the user directly in the GMO app)
	// are display-only — the bot does not manage their lifecycle. Counting
	// them against per-symbol `max_open_positions` would surprise the user:
	// a single app-side trade would freeze the bot. Skip them here so they
	// don't consume per-symbol bot capacity. (Account-wide cap below uses
	// CountOpenAllSymbols, which intentionally DOES count externals because
	// margin is a shared pool external positions also draw from.)
	openCount := 0
	// Side-aware counts INCLUDING external for the pyramiding
	// block (external is excluded from bot capacity above but must still block
	// same-side stacking).
	openBuyInclExt := 0
	openSellInclExt := 0
	for _, p := range open {
		switch p.Side {
		case "BUY":
			openBuyInclExt++
		case "SELL":
			openSellInclExt++
		}
		if p.Source.IsExternal() {
			continue
		}
		openCount++
	}

	// Per-symbol closed_at aggregates via *BySymbol so a sibling symbol's
	// trades cannot fire this gate.
	tradesInWindow := 0
	lossInWindow := 0
	if active != nil && !active.ValidFrom.IsZero() && a.Trades != nil {
		tradesInWindow, err = a.Trades.CountClosedBySymbolSince(ctx, sym, active.ValidFrom)
		if err != nil {
			return risk.AccountSnapshot{}, fmt.Errorf("count_closed_in_window: %w", err)
		}
		lossInWindow, err = a.Trades.SumClosedLossJPYBySymbolSince(ctx, sym, active.ValidFrom)
		if err != nil {
			return risk.AccountSnapshot{}, fmt.Errorf("sum_closed_loss_in_window: %w", err)
		}
	}

	startOfDay := config.StartOfDayIn(now, a.BotConfig.Bot.Timezone)

	dayLoss := 0
	agg := TradeAggregates{}
	if a.Trades != nil {
		dayLoss, err = a.Trades.SumClosedLossJPYBySymbolSince(ctx, sym, startOfDay)
		if err != nil {
			return risk.AccountSnapshot{}, fmt.Errorf("sum_closed_loss_day: %w", err)
		}
		recent, terr := a.Trades.ListClosedBySymbolSince(ctx, sym, startOfDay, 50)
		if terr != nil {
			return risk.AccountSnapshot{}, fmt.Errorf("list_recent_closed_trades: %w", terr)
		}
		agg = DeriveTradeAggregates(recent)
	}

	// Account-wide aggregates. Account totals use the unsuffixed methods.
	var accountOpen int
	var accountDayLoss int
	if a.Positions != nil {
		accountOpen, err = a.Positions.CountOpenAllSymbols(ctx)
		if err != nil {
			return risk.AccountSnapshot{}, fmt.Errorf("count_open_all_symbols: %w", err)
		}
	}
	if a.Trades != nil {
		accountDayLoss, err = a.Trades.SumClosedLossJPYSince(ctx, startOfDay)
		if err != nil {
			return risk.AccountSnapshot{}, fmt.Errorf("sum_closed_loss_day_account: %w", err)
		}
	}

	// C.5 fix: cooldown re-evaluation. CooldownFn is supplied by the
	// wiring layer using the same `app.ComputeCooldown` worker uses, so
	// the two paths can't drift. Nil → cooldown gate stays disabled
	// (backwards compat with admission instances not yet rewired).
	var inCooldown bool
	var cdUntil time.Time
	var cdKind string
	if a.CooldownFn != nil {
		inCooldown, cdUntil, cdKind = a.CooldownFn(now)
	}

	return risk.AccountSnapshot{
		EmergencyStop:           safety.Active(a.EmergencyFlagPath),
		OpenPositions:           openCount,
		OpenBuyInclExternal:     openBuyInclExt,
		OpenSellInclExternal:    openSellInclExt,
		TradesInWindow:          tradesInWindow,
		LossInWindowJPY:         lossInWindow,
		DailyLossJPY:            dayLoss,
		MaxDailyLossJPY:         a.BotConfig.Risk.MaxDailyLossJPY,
		ConsecutiveLosses:       agg.ConsecutiveLosses,
		MaxConsecutiveLosses:    a.BotConfig.Risk.MaxConsecutiveLosses,
		AccountOpenPositions:    accountOpen,
		AccountDailyLossJPY:     accountDayLoss,
		AccountMaxOpenPositions: a.BotConfig.Risk.AccountMaxOpenPositions,
		AccountMaxDailyLossJPY:  a.BotConfig.Risk.AccountMaxDailyLossJPY,
		InCooldown:              inCooldown,
		CooldownUntil:           cdUntil,
		CooldownKind:            cdKind,
		Now:                     now,
		LastLossClosedAt:        agg.LastLossClosedAt,
		BuyStopLossesToday:      agg.BuyStopLossesToday,
		SellStopLossesToday:     agg.SellStopLossesToday,
		InEventFreeze:           a.EventCalendar.InFreezeWindowConsideringAdvisor(now, a.BotConfig.AIAdvisor.Enabled),

		ConsecutiveLossGuardsDisabled: a.BotConfig.Risk.DisableConsecutiveLossGuards,
		PostLossFreeze:                time.Duration(a.BotConfig.Risk.PostLossFreezeMinutes) * time.Minute,

		// Reentry cooldown: worker.accountSnapshot と同じ agg 由来 —
		// 同 symbol 同 side は任意の決済 (勝ち含む) から N 分新規禁止 (決済直後の即再 IN を塞ぐ)。
		ReentryCooldown:  time.Duration(a.BotConfig.Risk.ReentryCooldownMinutes) * time.Minute,
		LastBuyClosedAt:  agg.LastBuyClosedAt,
		LastSellClosedAt: agg.LastSellClosedAt,
	}, nil
}
