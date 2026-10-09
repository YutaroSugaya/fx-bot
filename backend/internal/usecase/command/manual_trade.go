package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/safety"
)

// parseBrokerPositionID parses the bot-internal string positionId (stored
// in DB / domain as a string) to the int64 that GMO Forex /v1/closeOrder
// requires. Returns an error on non-numeric input so the caller can trip
// emergency_stop rather than send a malformed payload.
func parseBrokerPositionID(id string) (int64, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("broker position id %q is not numeric: %w", id, err)
	}
	return n, nil
}

// ManualTradeInput は ManualTradeCommand.Execute の引数。
//
// AllowOverride is the explicit operator-override flag.
// Default false makes manual entries subject to the SAME gates as auto;
// set true only when the operator intentionally wants to bypass the
// allowlisted soft gates (see isOverridableReason): open_positions, cooldown,
// consecutive_losses, post_loss_freeze, trades_in_window, loss_in_window,
// the direction_* policy (incl. the same-direction 2-SL block) and the spread
// cap. emergency_stop, no_active_config, event_freeze, daily_loss /
// account_daily_loss, reentry_cooldown, the same-side pyramiding block (no
// nanpin) and account_open_positions are re-checked in a second pass
// (risk.EvaluateHardSafety) and remain enforced regardless of this flag.
type ManualTradeInput struct {
	Side           order.Side // SideBuy or SideSell
	TakeProfitPips float64
	StopLossPips   float64
	MaxHoldMinutes int
	Quantity       int  // 0 → HardLimits.Quantity.Min (HardLimits nil なら safety.DefaultQuantity)
	AllowOverride  bool // default false; only an explicit operator opt-in
}

// ManualTradeOutput は ManualTradeCommand.Execute の戻り値。
type ManualTradeOutput struct {
	PositionID int64
	Side       order.Side
	Quantity   int
	EntryPrice float64
	TPPrice    float64
	SLPrice    float64
}

// ErrInvalidSide は side が SideBuy / SideSell 以外のとき。
var ErrInvalidSide = errors.New("side must be BUY or SELL")

// ErrInvalidPips は TP/SL pips が 0 以下のとき。
var ErrInvalidPips = errors.New("take_profit_pips and stop_loss_pips must be > 0")

// ErrQuantityOutOfRange は HardLimits.Quantity の min/max を超えた quantity
// を要求された場合 (C.3 fix)。hard_limits.quantity.max が数量上限の SSOT で、
// per-order の cap はここ (manual_trade) と config.Validator (auto trade) で
// それぞれ enforce する。
var ErrQuantityOutOfRange = errors.New("quantity outside hard_limits.quantity range")

// ManualTradeCommand は API 経由 (= 手動) の即時エントリー usecase。
//
// Paper モード: MARKET 注文を直接発注。
// Live モード: 成行エントリー後、GMO 側に TP/SL の OCO 決済注文を置く。
// orderId → positionId の解決 (ResolveExecution) 失敗時は emergency_stop を発火して
//
//	DB レコードを作らずに error を返す (= ゴーストレコード防止)。
type ManualTradeCommand struct {
	Mode              config.Mode
	Symbol            string
	PipSize           float64
	Broker            port.Broker
	Positions         port.PositionRepository
	EmergencyFlagPath string
	Logger            *slog.Logger

	// EntryMutex is shared with ExecuteOrder so a Claude auto-signal and a
	// dashboard click cannot race. nil ⇒ no locking (test convenience).
	//
	// When Admission is wired, Execute goes
	// through it so manual trades are subject to the same gates as auto
	// (emergency_stop / daily_loss are NEVER overridable; max_open_positions
	// / cooldown can be overridden by the operator with a logged warning).
	EntryMutex *sync.Mutex
	Admission  *EntryAdmission

	// ActiveConfig is consulted only by the admission gate (for
	// window-based caps and direction policy). nil → admission skips those
	// gates but still enforces emergency_stop / daily_loss.
	ActiveConfig func() *config.StrategyConfig

	// HardLimits caps quantity per-order. When non-nil, Execute
	// rejects requests whose Quantity falls outside [Quantity.Min,
	// Quantity.Max]. nil = legacy behaviour (no cap; rely on caller).
	// hard_limits.quantity.max is the SSOT for the per-order quantity cap.
	HardLimits *config.HardLimits

	// PendingTracker は Live entry saga 進行中の broker_position_id を保持し、
	// reconcile race-window をガードする (DB INSERT 前の新規建玉を reconcile が
	// external として誤採用しないため)。
	// LiveExitProtector へそのまま渡し、DB INSERT 終端で MarkResolved する。
	// nil 許容 (paper / 単体テスト)。
	PendingTracker port.PendingPositionTracker
}

// Execute は手動エントリーの全フローを 1 関数で実行する。
func (c *ManualTradeCommand) Execute(ctx context.Context, in ManualTradeInput) (ManualTradeOutput, error) {
	if in.Side != order.SideBuy && in.Side != order.SideSell {
		return ManualTradeOutput{}, ErrInvalidSide
	}
	if in.TakeProfitPips <= 0 || in.StopLossPips <= 0 {
		return ManualTradeOutput{}, ErrInvalidPips
	}
	// Fetch ticker first so admission can re-check spread cap.
	// Ticker is read-only and doesn't depend on the entry lock, so racing
	// the admission lock is safe.
	ticker, err := c.Broker.GetTicker(ctx, c.Symbol)
	if err != nil || ticker == nil {
		return ManualTradeOutput{}, fmt.Errorf("get ticker: %w", err)
	}

	// Route manual entries through EntryAdmission
	// so they hit the same gate as auto. AllowOverride is an EXPLICIT
	// operator opt-in (not implicit-always-true) — default false makes
	// manual entries subject to the same gate as auto. With
	// AllowOverride=true the admission suppresses only the allowlisted soft
	// gates (see ManualTradeInput.AllowOverride / isOverridableReason);
	// the hard gates (emergency_stop, daily_loss, no nanpin, …) remain enforced.
	if c.Admission != nil {
		var active *config.StrategyConfig
		if c.ActiveConfig != nil {
			active = c.ActiveConfig()
		}
		sig := strategy.Signal{
			Decision:       strategy.DecisionEnter,
			Side:           in.Side,
			TakeProfitPips: in.TakeProfitPips,
			StopLossPips:   in.StopLossPips,
			MaxHoldMinutes: in.MaxHoldMinutes,
			ConfigID:       "manual",
		}
		verdict, release, aerr := c.Admission.CheckAndHold(ctx, AdmissionRequest{
			Signal:        sig,
			ActiveConfig:  active,
			Ticker:        ticker, // C.5: enables admission to re-check spread cap
			AllowOverride: in.AllowOverride,
			Source:        "manual",
		})
		if aerr != nil {
			return ManualTradeOutput{}, fmt.Errorf("admission: %w", aerr)
		}
		if !verdict.Allowed {
			return ManualTradeOutput{}, fmt.Errorf("manual entry rejected by admission: %s", verdict.Reason)
		}
		defer release()
	} else if c.EntryMutex != nil {
		c.EntryMutex.Lock()
		defer c.EntryMutex.Unlock()
	}

	qty := in.Quantity
	if qty <= 0 {
		// Default a Quantity≤0 omission to the
		// SMALLEST safe size (HardLimits.Quantity.Min, e.g. 1000), NOT the cap
		// (.Max, e.g. 100000). Defaulting an omission to the maximum approved
		// size would be a 100x risk amplifier with no second wall. Min is fail-safe; want more → say so explicitly.
		if c.HardLimits != nil && c.HardLimits.Quantity.Min > 0 {
			qty = c.HardLimits.Quantity.Min
		} else {
			qty = safety.DefaultQuantity
		}
	}
	// C.3 fix: per-order quantity must fall inside HardLimits.Quantity when
	// configured. hard_limits.quantity.max is the SSOT for the quantity
	// cap (auto-trade enforces it via config.Validator at config promotion
	// time; manual-trade enforces it here at order time).
	if c.HardLimits != nil {
		if qty < c.HardLimits.Quantity.Min || qty > c.HardLimits.Quantity.Max {
			return ManualTradeOutput{}, fmt.Errorf("%w: requested=%d, allowed=[%d,%d]",
				ErrQuantityOutOfRange, qty, c.HardLimits.Quantity.Min, c.HardLimits.Quantity.Max)
		}
	}
	maxHold := in.MaxHoldMinutes
	if maxHold <= 0 {
		maxHold = safety.DefaultManualMaxHoldMinutes
	}

	// Order-boundary check on the actual manual order values
	// (SL/TP/MaxHold/qty + per-trade worst-case loss). qty range was already
	// checked above; this adds the MaxHold cap, SL/TP sanity and the JPY loss
	// cap so a fat-fingered operator order can't exceed the per-trade risk.
	if c.HardLimits != nil {
		rate, rerr := resolveQuoteJPYRate(ctx, c.Broker, c.Symbol)
		if rerr != nil {
			rate = 0 // transient rate error: skip JPY loss cap, keep other bounds
		}
		// Manual trades use the GLOBAL order boundary (strategyName="") — an operator order is not
		// a strategy and must stay under the tight fat-finger sanity caps.
		if berr := ValidateSignalBoundaries("", c.Symbol, in.StopLossPips, in.TakeProfitPips, maxHold, qty, rate, c.HardLimits); berr != nil {
			return ManualTradeOutput{}, fmt.Errorf("manual entry rejected: %w", berr)
		}
	}

	placeReq := order.PlaceOrderRequest{
		Symbol:   c.Symbol,
		Side:     in.Side,
		Type:     order.OrderTypeMarket,
		Quantity: qty,
	}

	// Live entry flow (MARKET+OCO):
	//
	//   1. PlaceOrder(MARKET)               → entry orderId
	//   2. ResolveExecution(entry orderId)  → broker positionId + actual fillPx
	//   3. PlaceSettleOCO(positionId, …)    → OCO container's rootOrderId
	//   4. ResolveSettleLegs(positionId)    → tp / sl leg orderIds
	//
	// Why not IFDOCO single-call: GMO /v1/ifoOrder rejects sizes < 10,000.
	// /v1/order MARKET accepts the symbols-API minimum (100 currency for
	// USD_JPY), and the OCO settle pair is attached via /v1/closeOrder
	// after the fill (TP/SL must live broker-side, never bot-only). The 1-2s window
	// between step 1 and step 3 is the only naked period; failure at step
	// 3 triggers a compensating market close (LiveExitProtector); emergency_stop
	// fires only if that also fails.
	ord, err := c.Broker.PlaceOrder(ctx, placeReq)
	if err != nil {
		return ManualTradeOutput{}, fmt.Errorf("place order: %w", err)
	}

	// Paper: ord.Price = 約定価格。
	// Live:  Protector が ResolveExecution → OCO → ResolveSettleLegs を一括処理し、
	//        失敗時の emergency_stop / compensating close も内部で処理する。
	//        共通化された詳細は live_exit_protector.go を参照。
	entryPrice := ord.Price
	brokerPosID := ord.OrderID
	var tpOrderID, slOrderID string
	// entry fill の broker 実報告手数料 (live のみ。paper は NULL = 未捕捉)。
	var entryFeeJPY *float64
	if c.Mode.IsLive() {
		protector := &LiveExitProtector{
			Broker:            c.Broker,
			Symbol:            c.Symbol,
			PipSize:           c.PipSize,
			Source:            "manual",
			EmergencyFlagPath: c.EmergencyFlagPath,
			Logger:            c.Logger,
			PendingTracker:    c.PendingTracker,
		}
		res, perr := protector.Attach(ctx, LiveExitProtectionInput{
			Order:          ord,
			Side:           in.Side,
			Quantity:       qty,
			TakeProfitPips: in.TakeProfitPips,
			StopLossPips:   in.StopLossPips,
		})
		if perr != nil {
			// Attach 内のエラー経路では protector 自身が MarkResolved 済み。
			return ManualTradeOutput{}, perr
		}
		entryPrice = res.EntryPrice
		brokerPosID = res.BrokerPositionID
		tpOrderID = res.TPOrderID
		slOrderID = res.SLOrderID
		fee := res.EntryFeeJPY
		entryFeeJPY = &fee
		// happy path: DB INSERT 終端 (成功 / 失敗どちらも) で必ず tracker 解除。
		if c.PendingTracker != nil {
			defer c.PendingTracker.MarkResolved(brokerPosID)
		}
	}
	// 発注直前 ticker の実測 spread と実 fill からの slippage を凍結保存。
	entrySpreadPips, entrySlippagePips := entryCostSnapshot(in.Side, ticker, entryPrice, c.PipSize)

	// Step B: positions.strategy_config_id is a NOT NULL FK. Manual
	// entries snapshot the active config at trade time; the
	// manual_positions junction (Manual: true below) records "this was
	// initiated manually". If no active config exists we refuse to
	// proceed — manual trade has no valid FK target and risk-window
	// aggregates would lose this row's lineage.
	var activeCfgID string
	if c.ActiveConfig != nil {
		if active := c.ActiveConfig(); active != nil {
			activeCfgID = active.ConfigID
		}
	}
	if activeCfgID == "" {
		return ManualTradeOutput{}, c.tripAndFail("manual_no_active_config", ord.OrderID,
			fmt.Errorf("no active strategy_configs row for (%s, %s); cannot FK manual entry",
				c.Symbol, c.Mode))
	}
	rec := port.PositionRecord{
		Symbol:         c.Symbol,
		Side:           string(in.Side),
		Quantity:       qty,
		EntryPrice:     entryPrice,
		TakeProfitPips: in.TakeProfitPips,
		StopLossPips:   in.StopLossPips,
		MaxHoldMinutes: maxHold,
		// Manual trades skip auto MaxHold policies (= force-close at maxHold,
		// no extension grace, no early-exit). Operator can manage hold time
		// interactively; auto-policies would conflict with manual control.
		ExtensionMaxMinutes:              0,
		ExtensionUnrealizedPipsThreshold: 0,
		EarlyExitWindowMinutes:           0,
		EarlyExitTargetPips:              0,
		// Entry-time cost capture (migration 0008): nil = 未捕捉。
		EntryFeeJPY:       entryFeeJPY,
		EntrySpreadPips:   entrySpreadPips,
		EntrySlippagePips: entrySlippagePips,
		StrategyConfigID:  activeCfgID,
		Status:            port.PositionStatusOpen,
		OpenedAt:          time.Now(),
	}
	insertIn := port.PositionInsertInput{
		Position: rec,
		Live: &port.PositionLive{
			BrokerPositionID: brokerPosID,
			TPOrderID:        tpOrderID,
			SLOrderID:        slOrderID,
		},
		Manual: true,
	}
	id, err := c.Positions.Insert(ctx, insertIn)
	if err != nil {
		// Broker has already accepted the order (Live: MARKET entry is at GMO;
		// TP/SL OCO may also be attached depending on where the failure happened).
		// DB insert failure means a ghost position — trip emergency_stop so
		// the reconciler / human can resolve before placing more orders.
		return ManualTradeOutput{}, c.tripAndFail("manual_trade_position_insert_failed", ord.OrderID, err)
	}

	tpPrice, slPrice := position.ComputeTPSLPrices(in.Side, entryPrice, in.TakeProfitPips, in.StopLossPips, c.PipSize)
	return ManualTradeOutput{
		PositionID: id,
		Side:       in.Side,
		Quantity:   qty,
		EntryPrice: entryPrice,
		TPPrice:    tpPrice,
		SLPrice:    slPrice,
	}, nil
}

func (c *ManualTradeCommand) tripAndFail(reason, orderID string, cause error) error {
	c.Logger.Error("manual_trade_critical",
		"reason", reason, "order_id", orderID, "cause", cause)
	if werr := safety.TripWithDetail(c.EmergencyFlagPath, reason,
		fmt.Sprintf("order=%s cause=%v", orderID, cause)); werr != nil {
		c.Logger.Error("emergency_stop_write_failed", "err", werr)
	}
	return fmt.Errorf("CRITICAL manual_trade failure (%s) for order %s: %w", reason, orderID, cause)
}

// 補償 close (OCO 失敗時の rollback) は LiveExitProtector.compensate() が実装している。
