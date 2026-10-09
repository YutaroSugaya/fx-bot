package command

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/safety"
)

// ExecuteOrder turns an entry Signal into a real (or paper) order via the
// Broker, then records the resulting position in the DB.
//
// MVP scope:
//   - Paper mode (mode != live_config): use MARKET. TP/SL are tracked
//     in-process by ManageOpenPositions, not as broker-side OCO.
//   - Live mode (mode == live_config): place MARKET entry, resolve the actual
//     fill, then attach broker-side OCO settle orders for TP/SL.
type ExecuteOrder struct {
	Broker            port.Broker
	Positions         port.PositionRepository
	Mode              config.Mode
	Symbol            string
	EmergencyFlagPath string // ResolveExecution 失敗時に発火する flag。空なら no-op
	Logger            *slog.Logger
	Clock             func() time.Time

	// EntryMutex serialises all entry-side broker calls with ManualTradeCommand
	// so auto+manual cannot place orders concurrently and silently exceed
	// max_open_positions. nil ⇒ no locking (test convenience).
	//
	// When Admission is set, OnSignal goes
	// through it (re-checks gate under lock) instead of using EntryMutex
	// directly. Tests that don't need the gate can still wire EntryMutex.
	EntryMutex *sync.Mutex
	Admission  *EntryAdmission

	// ActiveConfig is optionally injected so OnSignal can hand the active
	// config + summary to the admission service. nil callers fall back to
	// the legacy "lock + place" path (admission re-check skipped).
	ActiveConfig func() *config.StrategyConfig

	// PendingTracker は Live entry saga 進行中の broker_position_id を保持し、
	// reconcile race-window をガードする (DB INSERT 前の新規建玉を reconcile が
	// external として誤採用しないため)。
	// LiveExitProtector へ渡し、DB INSERT 終端で MarkResolved する。nil 許容。
	PendingTracker port.PendingPositionTracker

	// MinQuantity は applyQtyMultiplier の broker 最低発注
	// floor。GMO USD/JPY なら 1000。0 = floor 無効 (テスト・legacy 互換)。
	// 配線は cmd/bot/symbol_bundle_wiring.go が HardLimits.Quantity.Min から
	// 注入する。
	MinQuantity int

	// HardLimits は発注境界での実 Signal 値検査に使う。
	// nil = 検査 skip (テスト・legacy 互換)。配線は symbol_bundle_wiring.go。
	HardLimits *config.HardLimits
}

func NewExecuteOrder(br port.Broker, pos port.PositionRepository, mode config.Mode, symbol string, logger *slog.Logger) *ExecuteOrder {
	if logger == nil {
		logger = slog.Default()
	}
	return &ExecuteOrder{Broker: br, Positions: pos, Mode: mode, Symbol: symbol, Logger: logger, Clock: time.Now}
}

// AdmissionRejectedError is returned by OnSignal when the EntryAdmission gate
// refuses the entry. A refusal is a NORMAL trading outcome (risk cap / config
// gate), not a transport failure — callers surface it as its own stage/log
// instead of treating it as either success or an error.
//
// nil を返してはならない: nil だと LLM journal / dashboard は拒否を
// stage="submitted"(成功)と誤記録し、risk cap による拒否が続いても
// 「発注したのに建たない」無言の凍結に見えてしまう。
type AdmissionRejectedError struct{ Reason string }

func (e *AdmissionRejectedError) Error() string { return "entry admission rejected: " + e.Reason }

// OnSignal places a fresh order based on sig and records the open position.
//
// When Admission is wired, OnSignal performs a
// FINAL gate check under the admission lock before any broker call. The
// pre-tick gate in TradingCycle is still useful for cheap rejections, but
// admission is the authoritative serialisation point — a manual entry
// that fires between snapshot and lock cannot slip past
// max_open_positions / daily_loss.
func (u *ExecuteOrder) OnSignal(ctx context.Context, sig strategy.Signal, ticker *market.Ticker) error {
	if !sig.IsEntry() {
		return nil
	}
	// Fail loud on zero-quantity entries — strategies must populate Quantity
	// from the active config's risk.quantity. Silently sending qty=0 would
	// either be rejected by the broker with a cryptic error or (worse) accepted
	// by paper broker and corrupt PnL.
	if sig.Quantity <= 0 {
		u.Logger.Error("auto_entry_rejected_zero_quantity",
			"config_id", sig.ConfigID, "strategy", string(sig.StrategyName))
		return fmt.Errorf("auto entry rejected: sig.Quantity=%d (strategy=%s config=%s did not populate quantity)",
			sig.Quantity, sig.StrategyName, sig.ConfigID)
	}
	qtyMultiplier := 0.0
	if u.Admission != nil {
		var active *config.StrategyConfig
		if u.ActiveConfig != nil {
			active = u.ActiveConfig()
		}
		verdict, release, aerr := u.Admission.CheckAndHold(ctx, AdmissionRequest{
			Signal:       sig,
			ActiveConfig: active,
			Ticker:       ticker, // C.5: enables admission to re-check spread cap
			Source:       "auto",
		})
		if aerr != nil {
			return fmt.Errorf("admission: %w", aerr)
		}
		if !verdict.Allowed {
			// Warn (not Info): under LOG_LEVEL=warn this must stay visible — an
			// invisible refusal makes a stuck risk cap look like "no signal".
			u.Logger.Warn("auto_entry_rejected_by_admission", "reason", verdict.Reason,
				"config_id", sig.ConfigID, "strategy", string(sig.StrategyName))
			return &AdmissionRejectedError{Reason: verdict.Reason}
		}
		qtyMultiplier = verdict.QtyMultiplier
		defer release()
	} else if u.EntryMutex != nil {
		u.EntryMutex.Lock()
		defer u.EntryMutex.Unlock()
	}
	finalQty := applyQtyMultiplier(defaultQty(sig), qtyMultiplier, u.MinQuantity)

	// Order-boundary check on the REAL emitted Signal values
	// (not the config placeholders). Catches an out-of-range MaxHold / SL/TP
	// decimal typo / qty fat-finger and bounds the per-trade worst-case loss.
	if u.HardLimits != nil {
		rate, rerr := resolveQuoteJPYRate(ctx, u.Broker, u.Symbol)
		if rerr != nil {
			// Transient rate error: skip only the JPY loss cap, keep the rest.
			rate = 0
			u.Logger.Warn("order_boundary_quote_rate_unavailable", "symbol", u.Symbol, "err", rerr)
		}
		if berr := ValidateSignalBoundaries(string(sig.StrategyName), u.Symbol, sig.StopLossPips, sig.TakeProfitPips, sig.MaxHoldMinutes, finalQty, rate, u.HardLimits); berr != nil {
			u.Logger.Error("auto_entry_rejected_order_boundary", "err", berr, "config_id", sig.ConfigID, "strategy", string(sig.StrategyName))
			return fmt.Errorf("auto entry rejected: %w", berr)
		}
	}

	req := order.PlaceOrderRequest{
		Symbol:   u.Symbol,
		Side:     sig.Side,
		Type:     order.OrderTypeMarket,
		Quantity: finalQty,
	}

	// Live entry flow (MARKET+OCO): PlaceOrder は MARKET
	// 限定。Live モードでは ResolveExecution → OCO → ResolveSettleLegs を
	// LiveExitProtector に委譲 (manual_trade.go と共通化)。
	ord, err := u.Broker.PlaceOrder(ctx, req)
	if err != nil {
		return fmt.Errorf("place order: %w", err)
	}

	// Paper mode: ord.Price is the simulated fill price.
	// Live mode:  Protector が ResolveExecution 失敗 / OCO 失敗 / 補償 close 失敗の
	//             すべてを内部で扱う (emergency_stop は protector が発火する)。
	entryPrice := ord.Price
	brokerPosID := ord.OrderID
	var tpOrderID, slOrderID string
	// entry fill の broker 実報告手数料。live のみ (paper は broker
	// 手数料が存在しないため NULL = 未捕捉のまま)。
	var entryFeeJPY *float64
	if u.Mode.IsLive() {
		protector := &LiveExitProtector{
			Broker:            u.Broker,
			Symbol:            u.Symbol,
			PipSize:           market.PipSize(u.Symbol),
			Source:            "execute",
			EmergencyFlagPath: u.EmergencyFlagPath,
			Logger:            u.Logger,
			PendingTracker:    u.PendingTracker,
		}
		res, perr := protector.Attach(ctx, LiveExitProtectionInput{
			Order:          ord,
			Side:           sig.Side,
			Quantity:       req.Quantity,
			TakeProfitPips: sig.TakeProfitPips,
			StopLossPips:   sig.StopLossPips,
		})
		if perr != nil {
			// Attach 内のエラー経路では protector 自身が MarkResolved 済み。
			return perr
		}
		entryPrice = res.EntryPrice
		brokerPosID = res.BrokerPositionID
		tpOrderID = res.TPOrderID
		slOrderID = res.SLOrderID
		fee := res.EntryFeeJPY
		entryFeeJPY = &fee
		// happy path: DB INSERT 終端で必ず tracker 解除 (success / error 両経路)。
		if u.PendingTracker != nil {
			defer u.PendingTracker.MarkResolved(brokerPosID)
		}
	}
	// 発注直前 ticker の実測 spread と実 fill からの slippage を凍結保存。
	entrySpreadPips, entrySlippagePips := entryCostSnapshot(sig.Side, ticker, entryPrice, market.PipSize(u.Symbol))

	rec := port.PositionRecord{
		Symbol:         u.Symbol,
		Side:           string(sig.Side),
		Quantity:       req.Quantity,
		EntryPrice:     entryPrice,
		TakeProfitPips: sig.TakeProfitPips,
		StopLossPips:   sig.StopLossPips,
		MaxHoldMinutes: sig.MaxHoldMinutes,
		// Snapshot extension policy from the signal so mid-trade config swaps
		// can't retroactively alter this position's deadline.
		ExtensionMaxMinutes:              sig.ExtensionMaxMinutes,
		ExtensionUnrealizedPipsThreshold: sig.ExtensionUnrealizedPipsThreshold,
		// EarlyExit policy snapshot: also frozen at entry (open-position
		// protection rule) so a mid-trade config swap
		// can't change this position's early-exit behavior.
		EarlyExitWindowMinutes: sig.EarlyExitWindowMinutes,
		EarlyExitTargetPips:    sig.EarlyExitTargetPips,
		// Ratchet TP snapshot: trailing take-profit settings
		// frozen at entry for the same reason. peak/armed runtime state は
		// DB DEFAULT (0/false) でスタートし OnTick で更新される。
		RatchetArmPips:      sig.RatchetArmPips,
		RatchetGivebackPips: sig.RatchetGivebackPips,
		// Entry-time cost capture (migration 0008): broker 実報告 entry fee と
		// admission 時点 spread / fill slippage を凍結保存 (nil = 未捕捉)。
		EntryFeeJPY:       entryFeeJPY,
		EntrySpreadPips:   entrySpreadPips,
		EntrySlippagePips: entrySlippagePips,
		StrategyConfigID:  sig.ConfigID,
		Status:            port.PositionStatusOpen,
		OpenedAt:          u.Clock(),
	}
	insertIn := port.PositionInsertInput{
		Position: rec,
		Live: &port.PositionLive{
			BrokerPositionID: brokerPosID,
			TPOrderID:        tpOrderID,
			SLOrderID:        slOrderID,
		},
	}
	if _, err := u.Positions.Insert(ctx, insertIn); err != nil {
		// Broker has already accepted the order (Live: MARKET entry is at GMO;
		// TP/SL OCO may also be attached). DB insert failure means the bot has
		// no record of an open position — a "ghost" that the reconciler must
		// surface. Trip emergency_stop so worker stops opening new entries
		// until a human verifies/recovers from this divergence.
		return u.criticalLiveFailure("execute_position_insert_failed", err, ord.OrderID)
	}
	u.Logger.Info("order_placed",
		"side", string(sig.Side),
		"qty", req.Quantity,
		"entry", entryPrice,
		"tp_pips", sig.TakeProfitPips,
		"sl_pips", sig.StopLossPips,
		"config_id", sig.ConfigID,
	)
	return nil
}

// criticalLiveFailure は Live モードで「GMO に注文は投げたが positionId
// や TP/SL 保護を確認できない」状況を扱う。emergency_stop を発火して以降の新規エントリーを
// 全停止し、上位 (worker) にエラーを返す。reconciler が裸ポジを検出して
// クリーンアップする想定。EmergencyFlagPath が空なら flag は書かない。
func (u *ExecuteOrder) criticalLiveFailure(reason string, cause error, orderID string) error {
	u.Logger.Error("execute_order_critical_live_failure",
		"reason", reason, "order_id", orderID, "cause", cause)
	if werr := safety.TripWithDetail(u.EmergencyFlagPath, reason,
		fmt.Sprintf("order=%s cause=%v", orderID, cause)); werr != nil {
		u.Logger.Error("emergency_stop_write_failed", "err", werr)
	}
	return fmt.Errorf("CRITICAL live failure (%s) for order %s: %w", reason, orderID, cause)
}

// defaultQty returns the order size for a signal. Reads sig.Quantity which
// strategies populate from config.Risk.Quantity at evaluation time. OnSignal
// guards Quantity<=0 upstream, so callers can trust the value is positive.
//
// hard_limits.quantity validation (config.Validator) enforces the upper
// bound at config-load time — by the time
// a signal reaches here, the value is already within the safe range.
func defaultQty(sig strategy.Signal) int {
	return sig.Quantity
}

// applyQtyMultiplier scales sigQty by the risk gate's QtyMultiplier, with a
// broker floor (minQty). 例: 0.5 倍で halve する場合でも、GMO 最低発注
// 1000 通貨未満になるなら元の値に戻す (no-op)。現在の risk gate は常に 1.0 を返す。
//
// Semantics:
//   - mul <= 0 or mul >= 1.0 → no scaling (passthrough)
//   - minQty <= 0           → floor 無効、純粋に scale
//   - scaled < minQty       → original を維持 (halving が broker reject される
//     のを未然に防ぐ; "halve だが broker min 未満" は実質 "no halve")
func applyQtyMultiplier(sigQty int, mul float64, minQty int) int {
	if mul <= 0 || mul >= 1.0 {
		return sigQty
	}
	scaled := int(float64(sigQty) * mul)
	if minQty > 0 && scaled < minQty {
		return sigQty
	}
	return scaled
}

// PositionFromRecord converts a DB record + optional live metadata to a
// domain Position. live carries BrokerPositionID (used by Broker.ClosePosition);
// pass nil only when the caller doesn't need the broker id (e.g. paper paths
// that bypass the broker close call). PositionRecord no longer holds ClosedAt —
// only OPEN/CLOSING rows reach this function.
func PositionFromRecord(rec port.PositionRecord, live *port.PositionLive) position.Position {
	brokerID := ""
	if live != nil {
		brokerID = live.BrokerPositionID
	}
	return position.Position{
		ID:               rec.ID,
		BrokerPositionID: brokerID,
		Symbol:           rec.Symbol,
		Side:             order.Side(rec.Side),
		Quantity:         rec.Quantity,
		EntryPrice:       rec.EntryPrice,
		TakeProfitPips:   rec.TakeProfitPips,
		StopLossPips:     rec.StopLossPips,
		MaxHoldMinutes:   rec.MaxHoldMinutes,
		StrategyConfigID: rec.StrategyConfigID,
		Status:           position.Status(rec.Status),
		OpenedAt:         rec.OpenedAt,
	}
}
