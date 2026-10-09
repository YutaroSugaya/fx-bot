package command

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
)

// settleLegDiscoveryTimeout caps how long the close path waits for
// SettleLegResolver discovery. The entry path can afford to poll for up to
// safety.ResolveExecutionTimeout because OCO settle child orders can take ~500ms
// to materialise after placement — but on the *close* path we already
// know the entry happened long ago, so legs either exist now or never will.
// A short timeout keeps the operator's force-close button responsive.
const settleLegDiscoveryTimeout = 1500 * time.Millisecond

// cancelLiveSettleLegsForPosition cancels the GMO OCO settle legs
// belonging to ONE specific position before the bot issues a market close.
//
// It must NOT cancel every active order on the symbol (USD_JPY): that would
// also cancel unrelated protections on other positions and manual orders. It
// targets only the recorded TP/SL leg orderIds (or discovers them via the active-
// orders endpoint when they weren't recorded — see external-adoption path
// below).
//
// Ordering: cancel TP FIRST, SL
// LAST. If the second cancel fails mid-flight, only one of {TP, SL} remains
// active — we prefer SL to remain so downside is still bounded.
//
// Paper mode: returns (0, nil) immediately — there's nothing to cancel.
//
// Live cases:
//  1. Bot-tracked entry: live.TPOrderID + live.SLOrderID are both set →
//     cancel them in order.
//  2. External / orphan position (= adopted via reconcile): leg ids
//     are empty because the bot didn't place the OCO. Discover legs via
//     SettleLegResolver (= /v1/activeOrders filtered to this positionId);
//     cancel whatever turns up. If nothing turns up, the position truly
//     has no OCO and we proceed straight to market close.
//  3. Partially-tracked (one leg id present, the other empty): try to
//     cancel the one we know, log the other.
//
// A "both ids required, else fail loud" precondition would be too strict: it
// would break operator-initiated force-close on reconcile-adopted
// positions. Discovery + best-effort cancel is the contract.
func cancelLiveSettleLegsForPosition(
	ctx context.Context,
	br port.Broker,
	mode config.Mode,
	positionID int64,
	symbol string,
	live *port.PositionLive,
	logger *slog.Logger,
) (int, error) {
	if !mode.IsLive() {
		return 0, nil
	}
	tpID, slID, brokerPosID := "", "", ""
	if live != nil {
		tpID, slID, brokerPosID = live.TPOrderID, live.SLOrderID, live.BrokerPositionID
	}
	// External / orphan path: legs missing on the position row. Try to
	// discover them via /v1/activeOrders before giving up. Discovery is
	// optional — close still proceeds if no legs are found.
	if (tpID == "" || slID == "") && brokerPosID != "" {
		if resolver, ok := br.(port.SettleLegResolver); ok {
			// Short, dedicated timeout: discovery is best-effort. The
			// underlying ResolveSettleLegs polls until BOTH legs appear
			// or ctx expires; for "no OCO attached" positions both never
			// appear, so we don't want to block the operator's button.
			dctx, dcancel := context.WithTimeout(ctx, settleLegDiscoveryTimeout)
			dTP, dSL, derr := resolver.ResolveSettleLegs(dctx, brokerPosID, symbol)
			dcancel()
			if derr == nil {
				if tpID == "" {
					tpID = dTP
				}
				if slID == "" {
					slID = dSL
				}
			} else if logger != nil {
				logger.Warn("close_settle_legs_discovery_failed_continuing",
					"position_id", positionID, "broker_pos_id", brokerPosID, "err", derr,
					"note", "leg discovery failed (no OCO attached or transient error); close proceeding without explicit cancel — GMO is expected to auto-cancel OCO orphans on market close")
			}
		}
	}
	if tpID == "" && slID == "" {
		if logger != nil {
			logger.Info("close_no_recorded_or_discovered_legs",
				"position_id", positionID,
				"note", "no TP/SL legs to cancel; market close will proceed (GMO auto-cancels OCO orphans on close)")
		}
		return 0, nil
	}
	cancelled := 0
	if tpID != "" {
		if err := br.CancelOrder(ctx, tpID); err != nil {
			return cancelled, fmt.Errorf("cancel TP order=%s: %w", tpID, err)
		}
		cancelled++
	}
	if slID != "" {
		if err := br.CancelOrder(ctx, slID); err != nil {
			return cancelled, fmt.Errorf("cancel SL order=%s (TP already cancelled): %w", slID, err)
		}
		cancelled++
	}
	if logger != nil {
		logger.Info("live_pre_close_cancelled_settle_legs",
			"position_id", positionID, "tp", tpID, "sl", slID, "cancelled", cancelled)
	}
	return cancelled, nil
}
