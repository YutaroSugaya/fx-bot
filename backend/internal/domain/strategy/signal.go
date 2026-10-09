// Package strategy holds the pure trading-rule logic: each Strategy looks at
// a MarketSummary + candles and emits a Signal describing what (if anything)
// to trade.
//
// Implementations are deterministic and side-effect free — the risk gate
// and order manager (in usecase/) decide whether to act.
package strategy

import (
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
)

// Decision encodes whether the strategy wants to enter, on which side, or
// stay flat.
type Decision string

const (
	DecisionEnter   Decision = "ENTER"
	DecisionNone    Decision = "NONE"
	DecisionNoTrade Decision = "NO_TRADE" // explicit no-trade signal, distinct from "no condition met"
)

// Signal is what a Strategy emits per evaluation tick. SignalID is empty
// until the order manager promotes the signal to an actual order intent.
type Signal struct {
	Decision       Decision
	SignalID       string
	Side           order.Side
	EntryPrice     float64
	TakeProfitPips float64
	StopLossPips   float64
	MaxHoldMinutes int
	// Quantity is the order size copied from the active config's risk.quantity
	// at signal time. Required on Enter signals; OnSignal refuses entries with
	// Quantity <= 0 to fail loud on strategies that forget to populate it.
	Quantity int
	// MaxHold extension policy snapshot. When set the
	// position is allowed to run up to ExtensionMaxMinutes past MaxHoldMinutes
	// IF |unrealized_pips| ≤ ExtensionUnrealizedPipsThreshold at the soft
	// deadline. 0 = disabled (force close exactly at MaxHoldMinutes).
	ExtensionMaxMinutes              int
	ExtensionUnrealizedPipsThreshold float64
	// EarlyExitWindowMinutes / EarlyExitTargetPips snapshot from config:
	// in the final window before MaxHold soft deadline, close as soon as
	// unrealized_pips >= EarlyExitTargetPips. See port.PositionRecord docs
	// and ManageOpenPositions.evaluateExit for the runtime semantics.
	EarlyExitWindowMinutes int
	EarlyExitTargetPips    float64
	// RatchetArmPips / RatchetGivebackPips snapshot from config:
	// trailing take-profit. OnTick で peak を更新し、armed && peak からの
	// retrace が GivebackPips に達したら MARKET close する。0/0 = OFF。
	// config 駆動の戦略は config の値をそのまま詰める。構造 exit 戦略 (ma_pullback /
	// trend_follow / daily_trend / signature) は自前で計算した値を詰める。
	RatchetArmPips      float64
	RatchetGivebackPips float64
	// MaxConcurrent caps simultaneous SAME-SYMBOL same-side positions the risk Gate allows for this
	// entry. 0/1 = the strict single-position / no-nanpin rule (the default for every strategy). >1
	// lets a strategy "add to a winner" (advisor v2 pyramid): the gate permits up to N same-side,
	// while the strategy itself only emits the add when it is safe (advisor v2: the 1st's ratchet is armed).
	MaxConcurrent int
	Reason        string
	ConfigID      string
	StrategyName  config.StrategyName
	CreatedAt     time.Time
}

// IsEntry reports whether the order manager should attempt to place an order.
func (s Signal) IsEntry() bool { return s.Decision == DecisionEnter && s.Side.Valid() }

// configExitSignal builds an ENTER Signal whose exit plan is the config's
// Exit/Risk snapshot (TP/SL/MaxHold + extension/early-exit/ratchet + quantity).
// Shared by the config-driven strategies (momentum_pullback / breakout_follow /
// range_breakout_probe) which all copy the identical ~11-field snapshot; the
// caller supplies only side, entry, the strategy name, and the reason string.
// (mtf_pullback / ma_pullback compute structural exits instead and do not use
// this.)
func configExitSignal(in EvalInput, side order.Side, entry float64, name config.StrategyName, reason string) Signal {
	return Signal{
		Decision:                         DecisionEnter,
		Side:                             side,
		EntryPrice:                       entry,
		TakeProfitPips:                   in.Config.Exit.TakeProfitPips,
		StopLossPips:                     in.Config.Exit.StopLossPips,
		MaxHoldMinutes:                   in.Config.Exit.MaxHoldMinutes,
		ExtensionMaxMinutes:              in.Config.Exit.ExtensionMaxMinutes,
		ExtensionUnrealizedPipsThreshold: in.Config.Exit.ExtensionUnrealizedPipsThreshold,
		EarlyExitWindowMinutes:           in.Config.Exit.EarlyExitWindowMinutes,
		EarlyExitTargetPips:              in.Config.Exit.EarlyExitTargetPips,
		RatchetArmPips:                   in.Config.Exit.RatchetArmPips,
		RatchetGivebackPips:              in.Config.Exit.RatchetGivebackPips,
		Quantity:                         in.Config.Risk.Quantity,
		Reason:                           reason,
		ConfigID:                         in.Config.ConfigID,
		StrategyName:                     name,
		CreatedAt:                        in.Now,
	}
}
