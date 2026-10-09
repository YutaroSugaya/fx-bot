package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/safety"
)

// ErrPositionAlreadyClosing is returned by ExecuteCloseSaga when the
// ClaimForClose CAS failed — another saga already holds the position (or
// finalized it) — OR when the broker reports the position is already gone
// during the close (GMO ERR-254; the settle-leg OCO filled first). Caller
// (ManageOpenPositions, ClosePositionCommand) treats this as "skip silently";
// it is NOT an emergency.
var ErrPositionAlreadyClosing = errors.New("position not in OPEN state for close")

// ErrCloseRejectedRearmed is returned when the broker rejected the market-close
// AFTER the settle legs were cancelled (which would leave the position naked),
// but the saga successfully RE-ARMED the protective OCO so the position is
// protected again. Callers treat it like ErrPositionAlreadyClosing — a benign
// "did not close, no emergency" skip. The row is left CLOSING; reconcile records
// the eventual OCO fill. Typical trigger: GMO ERR-5218 (close attempted while the
// market is closed over the weekend).
var ErrCloseRejectedRearmed = errors.New("close rejected by broker; protective OCO re-armed")

// ErrCloseRejectedLegsIntact is returned when the broker rejected the close but
// the saga had cancelled ZERO settle legs — the protective OCO was never removed
// (e.g. leg ids unrecorded at entry and discovery found none during a restricted
// window). The position therefore retains its original protection (its OCO is
// still live at GMO). Benign — no emergency, and NO
// re-arm (which would DUPLICATE the live OCO and risk a reverse position on the
// orphan leg). Row left CLOSING; reconcile records the eventual OCO fill.
var ErrCloseRejectedLegsIntact = errors.New("close rejected by broker; settle legs were not cancelled (protection intact)")

// ErrCloseRateUnavailable is returned when the quote→JPY conversion rate could
// not be resolved (USD-quote pair whose USD/JPY ticker fetch failed). It is
// raised BEFORE any irreversible broker action (before the claim), so the
// position is left untouched (OPEN) and the exit simply retries next tick — we
// never close at the broker without being able to record a correct JPY PnL, and
// we never trip emergency_stop for a transient ticker blip. JPY-quote pairs
// never reach here (their rate is a constant 1.0 with no broker call).
var ErrCloseRateUnavailable = errors.New("close deferred: quote→JPY rate (USD/JPY) unavailable")

// CloseSagaResult is the outcome of a successful close: the resolved exit price
// and the realized PnL already converted to JPY. Callers use these directly
// instead of recomputing (which would re-resolve the rate and could fail
// independently after the trade was already recorded).
type CloseSagaResult struct {
	ExitPrice      float64
	ProfitLossPips float64
	ProfitLossJPY  float64
}

// CloseSagaInput bundles everything the saga needs.
type CloseSagaInput struct {
	Mode              config.Mode
	Symbol            string
	Broker            port.Broker
	Positions         port.PositionRepository
	Closer            port.PositionCloser
	EmergencyFlagPath string
	Logger            *slog.Logger
	// OnClosed is called exactly once AFTER the close is fully settled
	// (CloseAndRecord committed) — never on failure/skip paths. nil = no-op.
	// event_retrigger: wired to the LLM re-judgment coordinator so a
	// freed slot is re-examined immediately instead of at the next hourly tick.
	OnClosed func(symbol, reason string)
}

// ExecuteCloseSaga runs the full claim → cancel → close → resolve → record
// sequence for one position. Shared by manual close (ClosePositionCommand)
// and scheduled close (ManageOpenPositions.closeOne) so both paths obey the
// same safety envelope.
//
// Steps:
//  1. ClaimForClose: atomic OPEN → CLOSING (DB CAS). If another saga holds
//     the position, return ErrPositionAlreadyClosing and do nothing.
//  2. (Live) Cancel TP/SL settle legs by recorded (or discovered) order IDs.
//     Cancel failure → emergency_stop; no legs found → the close proceeds.
//  3. Broker.ClosePosition. Failure after leg cancel → re-arm the OCO;
//     emergency_stop only if the re-arm fails (ERR-254 / 0 legs cancelled =
//     benign skip).
//  4. (Live) ResolveExecution mandatory — provides the actual fill price.
//     Failure → emergency_stop (DB will reflect CLOSING, not CLOSED, until
//     reconcile cleans up).
//  5. CloseAndRecord: atomic CLOSING → CLOSED + trade insert. Any error
//     or ok=false → emergency_stop.
//
// Returns a CloseSagaResult (exit price + realized JPY PnL) on success. The
// caller is responsible for nothing further — all DB and safety state changes
// are handled here.
//
// reason is one of: "manual" | "take_profit" | "stop_loss" | "max_hold" |
// "early_exit" | "ratchet_takeprofit" | "ratchet_stoploss" | "session_flatten".
func ExecuteCloseSaga(
	ctx context.Context,
	in CloseSagaInput,
	rec port.PositionRecord,
	reason string,
	paperExitFallback float64,
	now time.Time,
) (CloseSagaResult, error) {
	// 0. Resolve the quote→JPY rate BEFORE any irreversible action (before the
	// claim and the broker close). JPY-quote pairs return a free 1.0 with no
	// broker call, so this is a no-op for JPY-quote pairs. For a USD-quote pair (EUR_USD), a transient USD/JPY ticker failure
	// here aborts the close cleanly — the position stays OPEN and the exit
	// retries next tick (still protected by its broker-side OCO in Live) — so we
	// never close at the broker without being able to record a correct JPY PnL,
	// and never trip emergency_stop for a transient blip.
	quoteJPYRate, qerr := resolveQuoteJPYRate(ctx, in.Broker, in.Symbol)
	if qerr != nil {
		if in.Logger != nil {
			in.Logger.Warn("close_deferred_quote_rate_unavailable",
				"position_id", rec.ID, "symbol", in.Symbol, "reason", reason, "cause", qerr,
				"note", "USD/JPY rate fetch failed before close; deferring (position left OPEN, retried next tick)")
		}
		return CloseSagaResult{}, ErrCloseRateUnavailable
	}

	// 1. Claim — only when the caller's snapshot says the row is still OPEN.
	// If rec.Status is already CLOSING the caller knows we're resuming a
	// previous saga that crashed mid-flight (= operator manually retried
	// after the previous close failed at a later step). Skip the claim and
	// continue from where the prior attempt stopped; downstream steps
	// (cancel legs, market close, resolve, CloseAndRecord) are idempotent
	// or safe to re-attempt.
	if rec.Status == port.PositionStatusOpen {
		ok, err := in.Positions.ClaimForClose(ctx, rec.ID, now)
		if err != nil {
			return CloseSagaResult{}, tripCloseFailure(in, "claim_for_close_db_failed", rec.ID, "", err)
		}
		if !ok {
			// Not OPEN — silently skip. Already CLOSING / CLOSED by another goroutine.
			return CloseSagaResult{}, ErrPositionAlreadyClosing
		}
	}

	// Look up broker-side metadata (broker_position_id always; TP/SL leg ids
	// in Live mode). Paper writes a positions_live row with empty TP/SL.
	live, lerr := in.Positions.GetLive(ctx, rec.ID)
	if lerr != nil {
		return CloseSagaResult{}, tripCloseFailure(in, "get_live_db_failed", rec.ID, "", lerr)
	}

	// 2. Cancel settle legs (Live only). External/orphan positions with no
	// recorded leg ids fall through to a discovery step inside the helper;
	// if no legs are found the close still proceeds (GMO auto-cancels OCO
	// orphans on market close).
	cancelledLegs := 0
	if in.Mode.IsLive() {
		n, cerr := cancelLiveSettleLegsForPosition(ctx, in.Broker, in.Mode, rec.ID, in.Symbol, live, in.Logger)
		if cerr != nil {
			return CloseSagaResult{}, tripCloseFailure(in, "cancel_settle_leg_failed_before_close", rec.ID, "", cerr)
		}
		cancelledLegs = n
	}

	// 3. Broker close
	pos := PositionFromRecord(rec, live)
	ord, err := in.Broker.ClosePosition(ctx, pos)
	if err != nil {
		// Benign race: the broker reports the position is already gone (GMO
		// ERR-254 "Not found position"). The settle leg (SL/TP OCO) filled
		// first and already closed it — the OPPOSITE of a naked position.
		// Skip emergency_stop; leave the row CLOSING so reconcile records the
		// actual broker-side fill. (Typical case: SL fills exactly at the
		// MaxHold deadline and our MaxHold close races the SL OCO fill.)
		if errors.Is(err, port.ErrBrokerPositionNotFound) {
			if in.Logger != nil {
				in.Logger.Warn("close_target_already_flat_at_broker",
					"position_id", rec.ID, "cause", err,
					"hint", "settle leg (OCO) filled before our close; reconcile will record the trade")
			}
			return CloseSagaResult{}, ErrPositionAlreadyClosing
		}
		if in.Mode.IsLive() {
			// Whether the position is NAKED depends on whether step 2 actually
			// removed its protective OCO:
			//
			//  - cancelledLegs == 0: we cancelled nothing — the OCO is still
			//    live at GMO (e.g. leg ids unrecorded at entry + discovery found
			//    none during the closed-market window).
			//    The close rejection did NOT make it naked. Do NOT trip and do
			//    NOT re-arm (re-arming would DUPLICATE the live OCO → a reverse
			//    position when the orphan leg fills). Benign skip; reconcile
			//    records the eventual OCO fill.
			//
			//  - cancelledLegs > 0: we removed the OCO, so the rejected close
			//    leaves the position genuinely naked. RE-ARM the original TP/SL.
			//    A fresh OCO is often accepted even when a market-close is refused
			//    (the restriction is on closing, not on placing settle orders).
			//    Re-arm success → protected again, no trip. Re-arm failure →
			//    genuinely unprotectable → trip.
			if cancelledLegs == 0 && live != nil && live.BrokerPositionID != "" {
				if in.Logger != nil {
					in.Logger.Warn("close_rejected_legs_intact_no_emergency",
						"position_id", rec.ID, "cause", err,
						"note", "broker rejected the close but no settle legs were cancelled — original OCO still protects the broker position; no re-arm (would duplicate)")
				}
				return CloseSagaResult{}, ErrCloseRejectedLegsIntact
			}
			if rearmErr := reArmSettleOCO(ctx, in, rec, live); rearmErr != nil {
				return CloseSagaResult{}, tripCloseFailure(in, "close_position_failed_after_settle_cancel", rec.ID, "",
					fmt.Errorf("naked position (oco re-arm failed: %v): %w", rearmErr, err))
			}
			if in.Logger != nil {
				in.Logger.Warn("close_rejected_oco_rearmed_no_emergency",
					"position_id", rec.ID, "cause", err,
					"note", "broker rejected the close after settle-leg cancel; protective OCO re-placed — position left protected at broker, reconcile records the eventual fill")
			}
			return CloseSagaResult{}, ErrCloseRejectedRearmed
		}
		return CloseSagaResult{}, tripCloseFailure(in, "close_position_failed", rec.ID, "", err)
	}

	// 4. ResolveExecution (Live mandatory, Paper uses ord.Price or fallback)
	exitPrice := ord.Price
	// close fill の broker 実報告コスト (fee / settledSwap)。Paper は
	// broker 実報告が無いので往復 0.002% 推定。
	costs := closeCosts{}
	if in.Mode.IsLive() {
		resolver, ok := in.Broker.(port.ExecutionResolver)
		if !ok {
			return CloseSagaResult{}, tripCloseFailure(in, "close_no_resolver", rec.ID, ord.OrderID,
				fmt.Errorf("live_config requires ExecutionResolver broker; got %T", in.Broker))
		}
		rctx, cancel := context.WithTimeout(ctx, safety.ResolveExecutionTimeout)
		res, rerr := resolver.ResolveExecution(rctx, ord.OrderID)
		cancel()
		if rerr != nil {
			return CloseSagaResult{}, tripCloseFailure(in, "close_resolve_execution_failed", rec.ID, ord.OrderID, rerr)
		}
		exitPrice = res.Price
		costs = composeLiveCloseCosts(rec, exitPrice, res.FeeJPY, res.SettledSwapJPY, true, quoteJPYRate)
	} else {
		if exitPrice == 0 {
			// Paper broker should always return a real price; this fallback is
			// only ever used by tests with a mock broker that returns 0.
			exitPrice = paperExitFallback
		}
		costs = composePaperCloseCosts(rec, exitPrice, quoteJPYRate)
	}

	// 5. CloseAndRecord. quoteJPYRate was resolved up front (step 0) before any
	// irreversible action, so by here the JPY conversion cannot fail.
	pnlPips, pnlJPY, _ := position.ComputeClosePnL(
		rec.EntryPrice, exitPrice, order.Side(rec.Side), rec.Quantity, in.Symbol, quoteJPYRate,
	)
	// err は rec.Side が DB から来た値なので invariant 上 BUY/SELL のみ。
	// 万が一壊れていても silent 0,0 になるだけなので呼出側は変えない。
	trade := buildCloseTrade(rec, exitPrice, pnlPips, pnlJPY, reason, now, costs)
	if in.Closer == nil {
		return CloseSagaResult{}, tripCloseFailure(in, "close_no_closer_wired", rec.ID, ord.OrderID,
			fmt.Errorf("PositionCloser is nil; wiring bug"))
	}
	okClose, cerr := in.Closer.CloseAndRecord(ctx, rec.ID, now, trade)
	if cerr != nil {
		return CloseSagaResult{}, tripCloseFailure(in, "close_and_record_db_failed", rec.ID, ord.OrderID, cerr)
	}
	if !okClose {
		// Position was not in CLOSING — either we never claimed (impossible
		// given step 1 succeeded) or another saga finalized it. Either way:
		// broker.ClosePosition just ran a second time; reverse position risk.
		return CloseSagaResult{}, tripCloseFailure(in, "close_race_double_broker_call", rec.ID, ord.OrderID,
			fmt.Errorf("position not in CLOSING at finalize; broker close may have opened reverse position"))
	}
	if in.OnClosed != nil {
		in.OnClosed(in.Symbol, reason)
	}
	return CloseSagaResult{ExitPrice: exitPrice, ProfitLossPips: pnlPips, ProfitLossJPY: pnlJPY}, nil
}

// reArmSettleOCO re-places the protective TP/SL OCO for a Live position whose
// settle legs were cancelled by the close saga (step 2) but whose market-close
// (step 3) was then rejected by the broker, leaving it naked. It recomputes the
// ORIGINAL TP/SL price levels from the position's entry + recorded pip targets
// and posts a fresh OCO settle order (mirrors LiveExitProtector's PlaceSettleOCO
// step). Returns nil when protection was restored; an error when it could not
// be (no broker position id, broker can't place OCO, no targets, or the broker
// rejected the OCO too) — the caller then trips emergency_stop.
func reArmSettleOCO(ctx context.Context, in CloseSagaInput, rec port.PositionRecord, live *port.PositionLive) error {
	if live == nil || live.BrokerPositionID == "" {
		return fmt.Errorf("no broker position id to re-arm")
	}
	if rec.TakeProfitPips <= 0 || rec.StopLossPips <= 0 {
		return fmt.Errorf("position has no TP/SL targets to re-arm (tp=%v sl=%v)", rec.TakeProfitPips, rec.StopLossPips)
	}
	placer, ok := in.Broker.(port.OCOCloseOrderPlacer)
	if !ok {
		return fmt.Errorf("broker %T is not an OCOCloseOrderPlacer", in.Broker)
	}
	brokerPosIDNum, perr := parseBrokerPositionID(live.BrokerPositionID)
	if perr != nil {
		return fmt.Errorf("broker position id not numeric: %w", perr)
	}
	pip := market.PipSize(in.Symbol)
	if pip <= 0 {
		return fmt.Errorf("unknown pip size for %s", in.Symbol)
	}
	tpPrice, slPrice := position.ComputeTPSLPrices(order.Side(rec.Side), rec.EntryPrice, rec.TakeProfitPips, rec.StopLossPips, pip)
	octx, cancel := context.WithTimeout(ctx, safety.ResolveExecutionTimeout)
	defer cancel()
	if _, oerr := placer.PlaceSettleOCO(octx, port.OCOCloseOrderInput{
		Symbol:           in.Symbol,
		BrokerPositionID: brokerPosIDNum,
		Side:             order.Side(rec.Side).Opposite(),
		Size:             rec.Quantity,
		TPPrice:          tpPrice,
		SLPrice:          slPrice,
	}); oerr != nil {
		return fmt.Errorf("place settle oco: %w", oerr)
	}
	return nil
}

// tripCloseFailure writes emergency_stop + logs + returns a wrapped error.
func tripCloseFailure(in CloseSagaInput, reason string, positionID int64, orderID string, cause error) error {
	if in.Logger != nil {
		in.Logger.Error("close_saga_critical",
			"reason", reason, "position_id", positionID, "order_id", orderID, "cause", cause)
	}
	if werr := safety.TripWithDetail(in.EmergencyFlagPath, reason,
		fmt.Sprintf("position=%d order=%s cause=%v", positionID, orderID, cause)); werr != nil {
		if in.Logger != nil {
			in.Logger.Error("emergency_stop_write_failed", "err", werr)
		}
	}
	return fmt.Errorf("CRITICAL close failure (%s) for position %d: %w", reason, positionID, cause)
}
