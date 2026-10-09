package command

import (
	"context"
	"fmt"
	"time"

	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/safety"
)

// Reconcile は「dispatcher (Run) + handler (各 mode 別の処理ロジック)」に分割している。
//
// naked_broker_position / stale_db_position の 2 ループ × 3 mode
// (Live runtime / Paper runtime / Paper startup または Live startup) の
// マトリクスを 1 関数に詰め込むと、各分岐の理解とテストが困難になるため。
//
// Handler は副作用 (DB INSERT / emergency_stop trip / log) を持つため
// 純粋関数ではなく `*Reconcile` のメソッドとして抽出。Summary はポインタで
// 渡して直接 mutate する。

// handleNakedBroker は「broker にあるが DB にない」ポジションを 1 件処理する。
// 3 分岐:
//  1. Live (any phase): external_broker として adopt
//  2. Paper runtime: trip (PaperBroker は bot 自身なので bot bug 扱い)
//  3. Paper startup: recovered_position として adopt
func (u *Reconcile) handleNakedBroker(ctx context.Context, p position.Position, now time.Time, summary *ReconcileSummary) {
	// 0. Race-window guard:
	//    entry saga 進行中 (broker fill 済みだが DB INSERT 未完) の position は
	//    pending tracker に登録されている。adopt せず次の pass まで保留する。
	//    ただし TTL 超過 (saga crash で MarkResolved されず永久 pending) は
	//    stale として skip 対象から外し、通常の naked-broker 経路に戻す
	//    (永久 pending が裸ポジを無期限に隠す穴を塞ぐ)。
	if u.PendingTracker != nil && u.PendingTracker.IsPending(p.BrokerPositionID) {
		if u.isPendingStale(p.BrokerPositionID) {
			u.Logger.Error("reconcile_pending_stale_detected",
				"broker_position_id", p.BrokerPositionID,
				"ttl", u.pendingTTL().String(),
				"note", "entry saga never resolved this pending id; treating as naked broker position")
			u.PendingTracker.MarkResolved(p.BrokerPositionID)
		} else {
			u.Logger.Info("reconcile_naked_broker_skipped_pending_entry",
				"broker_position_id", p.BrokerPositionID,
				"note", "entry saga in flight; will be reconciled after DB insert completes")
			return
		}
	}
	// Genuine naked broker position (broker has it, DB doesn't, not a pending
	// entry saga). Count every detection regardless of how it's handled below.
	if u.Counters != nil {
		u.Counters.IncrNakedPositions()
	}

	// 1. Live: 外部発注 adoption。ExternalAdoptGrace で「config が取れない裸ポジ」を
	//    即トリップせず一定時間 DEFER する(次 pass で消える transient な broker artifact では
	//    稼働中の bot を止めない。永続した時だけ trip = 安全網は維持)。spurious な
	//    emergency_stop を防ぐ。
	if u.LiveMode.IsLive() {
		switch u.adoptExternalLivePosition(ctx, p, now) {
		case extAdopted:
			u.clearExternalUnadoptable(p.BrokerPositionID)
			summary.Adopted++
		case extInsertFailed:
			// 実エラー(DB) は従来どおり即トリップ。
			u.tripEmergencyStop("external_adopt_insert_failed:" + p.BrokerPositionID)
			summary.Tripped++
		case extNoConfig:
			first := u.externalUnadoptableFirstSeen(p.BrokerPositionID, now)
			if u.ExternalAdoptGrace > 0 && now.Sub(first) < u.ExternalAdoptGrace {
				u.Logger.Warn("reconcile_external_unadoptable_deferred",
					"broker_position_id", p.BrokerPositionID,
					"first_seen", first, "grace", u.ExternalAdoptGrace.String(),
					"note", "no active config for symbol; deferring (likely a transient broker artifact)")
				summary.Deferred++
			} else {
				u.tripEmergencyStop("external_adopt_no_active_config:" + p.BrokerPositionID)
				summary.Tripped++
			}
		}
		return
	}

	// 2. Paper runtime: new naked = bot bug
	if u.Mode == ReconcileModeRuntime {
		summary.Tripped++
		u.warn(ctx, "naked_broker_position",
			fmt.Sprintf("runtime: paper broker has position %s (%s %d @ %v) without DB record",
				p.BrokerPositionID, p.Side, p.Quantity, p.EntryPrice),
			map[string]any{"broker_position_id": p.BrokerPositionID})
		u.tripEmergencyStop("naked_broker_position:" + p.BrokerPositionID)
		return
	}

	// 3. Paper startup: DB lost track (e.g. DB crash / test reset) → adopt
	activeCfgID, lookupErr := u.resolvePaperActiveConfigID(ctx, p.Symbol)
	if lookupErr != nil {
		u.Logger.Error("reconcile_adopt_no_active_config",
			"broker_position_id", p.BrokerPositionID, "err", lookupErr)
		u.tripEmergencyStop("adopt_no_active_config:" + p.BrokerPositionID)
		summary.Tripped++
		return
	}
	rec := port.PositionRecord{
		Symbol:           p.Symbol,
		Side:             string(p.Side),
		Quantity:         p.Quantity,
		EntryPrice:       p.EntryPrice,
		TakeProfitPips:   0,
		StopLossPips:     0,
		MaxHoldMinutes:   safety.DefaultManualMaxHoldMinutes,
		StrategyConfigID: activeCfgID,
		Status:           port.PositionStatusOpen,
		OpenedAt:         coalesceTime(p.OpenedAt, now),
	}
	insertIn := port.PositionInsertInput{
		Position: rec,
		Live: &port.PositionLive{
			BrokerPositionID: p.BrokerPositionID,
		},
		Recovered: &port.RecoveredPositionMeta{
			Reason:      port.RecoveryReasonBrokerNakedAtStartup,
			RecoveredAt: now,
		},
	}
	if _, ierr := u.Positions.Insert(ctx, insertIn); ierr != nil {
		u.Logger.Error("reconcile_adopt_insert_failed",
			"broker_position_id", p.BrokerPositionID, "err", ierr)
		u.tripEmergencyStop("adopt_insert_failed:" + p.BrokerPositionID)
		summary.Tripped++
		return
	}
	summary.Adopted++
	u.Logger.Info("reconcile_adopted_broker_position",
		"broker_position_id", p.BrokerPositionID,
		"side", string(p.Side), "qty", p.Quantity, "entry", p.EntryPrice)
}

// handleStaleDB は「DB が OPEN/CLOSING だが broker にない」ポジションを 1 件処理する。
// 3 分岐:
//  1. Runtime + Live: resolve → (grace 後) 推定 close → DEFER → hard window 超過で trip (0-PnL なし)
//  2. Startup + Live: resolve、未解決なら runtime へ DEFER (blind close 禁止)
//  3. Startup + Paper: synthetic cold close (paper には fill 履歴がない)
func (u *Reconcile) handleStaleDB(
	ctx context.Context,
	p port.PositionRecord,
	live *port.PositionLive,
	now time.Time,
	summary *ReconcileSummary,
) {
	// 1. Runtime
	if u.Mode == ReconcileModeRuntime {
		// Live runtime は leg fill 解決を試行
		if u.LiveMode.IsLive() && u.Closer != nil {
			resolved, rerr := u.resolveAndRecordClose(ctx, p, live, now)
			if rerr == nil && resolved {
				u.clearStaleGrace(p.ID)
				summary.Resolved++
				return
			}
			if rerr != nil {
				u.Logger.Warn("reconcile_resolve_close_failed_deferring",
					"db_position_id", p.ID, "err", rerr)
			}
			// Grace: the real fill usually resolves a cycle
			// later when the GMO executions feed catches up. Within the grace
			// window, DEFER — do not estimated/synthetic-close (inaccurate PnL)
			// nor trip (spurious emergency_stop). Retry resolution next pass.
			if !u.staleGraceElapsed(p.ID, now) {
				summary.Deferred++
				u.Logger.Warn("reconcile_stale_db_position_deferred",
					"db_position_id", p.ID, "broker_position_id", live.BrokerPositionID,
					"grace", u.StaleGracePeriod.String(),
					"note", "real fill not yet resolvable; deferring fallback/trip to a later reconcile pass")
				return
			}
			// 実約定が解決できないとき、現在値が OCO の SL/TP を明確に
			// 超えていれば その水準の実 PnL で記録する (synthetic zero-PnL の前に試す)。
			if u.recordEstimatedClose(ctx, p, now) {
				u.clearStaleGrace(p.ID)
				summary.Resolved++
				return
			}
			// grace 後も実約定が解決できず、現在値も OCO の
			// 内側 (= recordEstimatedClose が ambiguous で false) のとき、**0-PnL の
			// synthetic close は作らない**。この状況は「broker のポジション一覧が建玉を
			// 一時的に落としただけで建玉はまだ生存」の可能性が高く、0-PnL で早期クローズ
			// すると後から来る実約定 (実 PnL) を永久に潰す (実際の損益が 0 円として
			// 記録されてしまう)。実約定が出るまで DEFER (retry) し続け、genuine orphan 用に
			// 十分長い hard window を超えたときだけ emergency_stop で人間に通知する
			// (それでも PnL は捏造しない)。
			if !u.staleHardTripElapsed(p.ID, now) {
				summary.Deferred++
				u.Logger.Warn("reconcile_stale_db_position_deferred_no_synthetic",
					"db_position_id", p.ID, "broker_position_id", live.BrokerPositionID,
					"hard_trip", u.StaleHardTripPeriod.String(),
					"note", "grace elapsed but real fill still unresolvable and price inside OCO band; deferring (NOT booking 0-PnL synthetic) so a later pass records the real fill — position likely still open at broker (feed lag)")
				return
			}
			// hard window even elapsed → genuine orphan. Fall through to the
			// emergency_stop trip below (alert a human) — still no synthetic 0-PnL.
		}
		u.clearStaleGrace(p.ID)
		summary.Tripped++
		u.warn(ctx, "stale_db_position",
			fmt.Sprintf("runtime: DB OPEN position %d (%s %d @ %v) missing from broker; could not resolve fill",
				p.ID, p.Side, p.Quantity, p.EntryPrice),
			map[string]any{"db_position_id": p.ID, "broker_position_id": live.BrokerPositionID})
		u.tripEmergencyStop(fmt.Sprintf("stale_db_position:%d", p.ID))
		return
	}

	// 2. Startup + Live: try to resolve the real fill from the GMO executions
	// feed. If it is not yet resolvable, DEFER to the runtime reconcile loop
	// (which applies a grace window) instead of booking a premature synthetic
	// 0-PnL trade or tripping emergency_stop.
	//
	// Why: the executions feed routinely lags a cold start by a few
	// seconds, so an immediate synthetic close at startup would book the real
	// P&L as 0 (losing the true exit price/PnL until a manual backfill), even
	// though the OCO fills are resolvable seconds later.
	// The 30s runtime loop + 90s grace (LiveRuntimeStaleGracePeriod) owns
	// the resolve→estimate→defer→hard-trip ladder (no synthetic on Live), giving the real fill time to appear.
	// (Pairs with migration 0006: once the real fill resolves, a TP/SL-non-match
	// broker-side close records as close_reason="broker_close" instead of failing
	// the CHECK constraint and falling back to synthetic.)
	if u.LiveMode.IsLive() {
		if u.Closer == nil {
			u.Logger.Error("reconcile_startup_no_closer_live", "db_position_id", p.ID)
			u.tripEmergencyStop(fmt.Sprintf("startup_no_closer_live:%d", p.ID))
			summary.Tripped++
			return
		}
		resolved, rerr := u.resolveAndRecordClose(ctx, p, live, now)
		if rerr == nil && resolved {
			summary.Resolved++
			return
		}
		if rerr != nil {
			u.Logger.Warn("reconcile_startup_resolve_close_failed_deferring_to_runtime",
				"db_position_id", p.ID, "err", rerr)
		}
		// Not resolvable yet — defer to the runtime reconcile loop (grace window)
		// rather than a premature synthetic 0-PnL close or trip.
		summary.Deferred++
		u.Logger.Warn("reconcile_startup_stale_deferred_to_runtime",
			"db_position_id", p.ID, "broker_position_id", live.BrokerPositionID,
			"note", "real fill not resolvable at startup; runtime reconcile (grace) will resolve or close it — avoids premature synthetic 0-PnL")
		return
	}

	// 3. Startup + Paper: synthetic zero-PnL close
	if u.Closer == nil {
		u.Logger.Error("reconcile_startup_no_closer", "db_position_id", p.ID)
		u.tripEmergencyStop(fmt.Sprintf("startup_no_closer:%d", p.ID))
		summary.Tripped++
		return
	}
	// CLOSING row は stuck saga の残骸。Claim を飛ばして
	// CloseAndRecord に進む (CLOSING 専用パス)。
	switch p.Status {
	case port.PositionStatusOpen:
		claimed, cerr := u.Positions.ClaimForClose(ctx, p.ID, now)
		if cerr != nil {
			u.Logger.Error("reconcile_claim_for_close_failed", "db_position_id", p.ID, "err", cerr)
			u.tripEmergencyStop(fmt.Sprintf("claim_for_close_failed:%d", p.ID))
			summary.Tripped++
			return
		}
		if !claimed {
			// 別パスが先に CLOSING→CLOSED に移していた → skip
			return
		}
	case port.PositionStatusClosing:
		// stuck saga: claim 飛ばして直接 CloseAndRecord
	default:
		// CLOSED or unknown — nothing to do
		return
	}
	// Paper synthetic close: 手数料は存在しないので zero costs。
	synthetic := buildCloseTrade(p, p.EntryPrice, 0, 0, "reconcile_cold_close", now, closeCosts{})
	ok, cerr2 := u.Closer.CloseAndRecord(ctx, p.ID, now, synthetic)
	if cerr2 != nil || !ok {
		u.Logger.Error("reconcile_mark_closed_failed", "db_position_id", p.ID, "err", cerr2, "ok", ok)
		u.tripEmergencyStop(fmt.Sprintf("mark_closed_failed:%d", p.ID))
		summary.Tripped++
		return
	}
	summary.MarkedDone++
	u.Logger.Info("reconcile_marked_db_position_closed",
		"db_position_id", p.ID, "broker_position_id", live.BrokerPositionID)
}
