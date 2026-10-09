package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/adapter/repository/dbgen"
	"fx-bot/backend/internal/port"
)

// PositionRepo persists positions + the related junction tables
// (positions_live, manual_positions, recovered_positions) and the
// position_state_events append-only ledger. Every state-affecting
// method is transactional so the main row + chosen variant + initial
// state event land together (or not at all).
type PositionRepo struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
}

func NewPositionRepo(pool *pgxpool.Pool) *PositionRepo {
	return &PositionRepo{pool: pool, q: dbgen.New(pool)}
}

// ErrPositionVariantConflict is returned from Insert when both Manual
// and Recovered are set. Live (broker-side metadata) is orthogonal to
// origin and may coexist with either.
var ErrPositionVariantConflict = errors.New("position insert: Manual and Recovered are mutually exclusive")

func (r *PositionRepo) Insert(ctx context.Context, in port.PositionInsertInput) (int64, error) {
	if err := validateVariant(in); err != nil {
		return 0, err
	}
	var id int64
	err := withTx(ctx, r.pool, r.q, func(q *dbgen.Queries) error {
		rec := in.Position
		newID, err := q.InsertPosition(ctx, dbgen.InsertPositionParams{
			Symbol:                           rec.Symbol,
			Side:                             rec.Side,
			Quantity:                         int32(rec.Quantity),
			EntryPrice:                       rec.EntryPrice,
			TakeProfitPips:                   rec.TakeProfitPips,
			StopLossPips:                     rec.StopLossPips,
			MaxHoldMinutes:                   int32(rec.MaxHoldMinutes),
			ExtensionMaxMinutes:              int32(rec.ExtensionMaxMinutes),
			ExtensionUnrealizedPipsThreshold: rec.ExtensionUnrealizedPipsThreshold,
			EarlyExitWindowMinutes:           int32(rec.EarlyExitWindowMinutes),
			EarlyExitTargetPips:              rec.EarlyExitTargetPips,
			RatchetArmPips:                   rec.RatchetArmPips,
			RatchetGivebackPips:              rec.RatchetGivebackPips,
			EntryFeeJpy:                      rec.EntryFeeJPY,
			EntrySpreadPips:                  rec.EntrySpreadPips,
			EntrySlippagePips:                rec.EntrySlippagePips,
			StrategyConfigID:                 rec.StrategyConfigID,
			Status:                           string(rec.Status),
			OpenedAt:                         pgts(rec.OpenedAt),
		})
		if err != nil {
			return fmt.Errorf("positions insert: %w", err)
		}
		id = newID
		if in.Live != nil {
			if err := q.InsertPositionLive(ctx, dbgen.InsertPositionLiveParams{
				PositionID:       id,
				BrokerPositionID: in.Live.BrokerPositionID,
				TpOrderID:        in.Live.TPOrderID,
				SlOrderID:        in.Live.SLOrderID,
			}); err != nil {
				return fmt.Errorf("positions_live insert: %w", err)
			}
		}
		if in.Manual {
			if err := q.InsertManualPosition(ctx, id); err != nil {
				return fmt.Errorf("manual_positions insert: %w", err)
			}
		}
		if in.Recovered != nil {
			if err := q.InsertRecoveredPosition(ctx, dbgen.InsertRecoveredPositionParams{
				PositionID:     id,
				RecoveryReason: in.Recovered.Reason,
				RecoveredAt:    pgts(in.Recovered.RecoveredAt),
			}); err != nil {
				return fmt.Errorf("recovered_positions insert: %w", err)
			}
		}
		if err := q.InsertPositionStateEvent(ctx, dbgen.InsertPositionStateEventParams{
			PositionID:     id,
			State:          string(rec.Status),
			TransitionedAt: pgts(rec.OpenedAt),
		}); err != nil {
			return fmt.Errorf("position_state_events insert (initial): %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

func validateVariant(in port.PositionInsertInput) error {
	if in.Manual && in.Recovered != nil {
		return ErrPositionVariantConflict
	}
	return nil
}

// ClaimForClose flips status OPEN → CLOSING AND appends a CLOSING entry
// to the state event ledger in one Tx.
func (r *PositionRepo) ClaimForClose(ctx context.Context, id int64, claimedAt time.Time) (bool, error) {
	var ok bool
	err := withTx(ctx, r.pool, r.q, func(q *dbgen.Queries) error {
		rows, err := q.ClaimPositionForClose(ctx, id)
		if err != nil {
			return fmt.Errorf("positions claim for close: %w", err)
		}
		if rows == 0 {
			ok = false
			return nil
		}
		if err := q.InsertPositionStateEvent(ctx, dbgen.InsertPositionStateEventParams{
			PositionID:     id,
			State:          string(port.PositionStatusClosing),
			TransitionedAt: pgts(claimedAt),
		}); err != nil {
			return fmt.Errorf("position_state_events insert (CLOSING): %w", err)
		}
		ok = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return ok, nil
}

// CountOpenAllSymbols returns the OPEN+CLOSING count across every symbol.
// External (manually-opened in the GMO app) positions are included because
// the account-wide cap is a margin-protection gate; margin is a shared
// pool external positions also draw from.
func (r *PositionRepo) CountOpenAllSymbols(ctx context.Context) (int, error) {
	v, err := r.q.CountOpenPositionsAllSymbols(ctx)
	if err != nil {
		return 0, fmt.Errorf("positions count open all symbols: %w", err)
	}
	return int(v), nil
}

// ListOpenOrClosing returns OPEN + CLOSING positions for the symbol.
// Reconcile uses this — a position stuck in CLOSING after a saga crash
// must remain visible until resolved. Pass "" for all symbols.
func (r *PositionRepo) ListOpenOrClosing(ctx context.Context, symbol string) ([]port.PositionRecord, error) {
	rows, err := r.q.ListOpenOrClosingPositions(ctx, symbol)
	if err != nil {
		return nil, fmt.Errorf("positions list open/closing: %w", err)
	}
	out := make([]port.PositionRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, port.PositionRecord{
			ID:                               row.ID,
			Symbol:                           row.Symbol,
			Side:                             row.Side,
			Quantity:                         int(row.Quantity),
			EntryPrice:                       row.EntryPrice,
			TakeProfitPips:                   row.TakeProfitPips,
			StopLossPips:                     row.StopLossPips,
			MaxHoldMinutes:                   int(row.MaxHoldMinutes),
			ExtensionMaxMinutes:              int(row.ExtensionMaxMinutes),
			ExtensionUnrealizedPipsThreshold: row.ExtensionUnrealizedPipsThreshold,
			EarlyExitWindowMinutes:           int(row.EarlyExitWindowMinutes),
			EarlyExitTargetPips:              row.EarlyExitTargetPips,
			RatchetArmPips:                   row.RatchetArmPips,
			RatchetGivebackPips:              row.RatchetGivebackPips,
			PeakUnrealizedPips:               row.PeakUnrealizedPips,
			RatchetArmed:                     row.RatchetArmed,
			TroughUnrealizedPips:             row.TroughUnrealizedPips,
			LossRatchetArmed:                 row.LossRatchetArmed,
			EntryFeeJPY:                      row.EntryFeeJpy,
			EntrySpreadPips:                  row.EntrySpreadPips,
			EntrySlippagePips:                row.EntrySlippagePips,
			StrategyConfigID:                 row.StrategyConfigID,
			Status:                           port.PositionStatus(row.Status),
			OpenedAt:                         pgtsTime(row.OpenedAt),
			Source:                           port.RecoveryReasonToSource(row.RecoveryReason),
		})
	}
	return out, nil
}

// GetLive returns the positions_live row for the given position, or
// (nil, nil) when none exists (= Paper position).
func (r *PositionRepo) GetLive(ctx context.Context, positionID int64) (*port.PositionLive, error) {
	live, err := r.q.GetPositionLive(ctx, positionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("positions_live get: %w", err)
	}
	return &port.PositionLive{
		PositionID:       live.PositionID,
		BrokerPositionID: live.BrokerPositionID,
		TPOrderID:        live.TpOrderID,
		SLOrderID:        live.SlOrderID,
	}, nil
}

// IsManual reports whether the position has a manual_positions row.
func (r *PositionRepo) IsManual(ctx context.Context, positionID int64) (bool, error) {
	ok, err := r.q.IsManualPosition(ctx, positionID)
	if err != nil {
		return false, fmt.Errorf("manual_positions exists: %w", err)
	}
	return ok, nil
}

// MarkClosed flips status → CLOSED + appends a CLOSED state event,
// WITHOUT writing a trade row. Paper-startup-only recovery path.
//
// Only insert the state event when the UPDATE actually flipped a row.
// Calling MarkClosed on an already-CLOSED row would otherwise attempt a
// duplicate position_state_events insert that the composite PK rejects,
// and the no-op keeps parity with InMemoryPositionRepo.MarkClosed
// (no-op on already-closed).
func (r *PositionRepo) MarkClosed(ctx context.Context, id int64, closedAt time.Time) error {
	return withTx(ctx, r.pool, r.q, func(q *dbgen.Queries) error {
		rows, err := q.MarkPositionClosedBlind(ctx, id)
		if err != nil {
			return fmt.Errorf("positions mark closed (blind): %w", err)
		}
		if rows == 0 {
			// Already CLOSED (or absent) — nothing to record.
			return nil
		}
		if err := q.InsertPositionStateEvent(ctx, dbgen.InsertPositionStateEventParams{
			PositionID:     id,
			State:          string(port.PositionStatusClosed),
			TransitionedAt: pgts(closedAt),
		}); err != nil {
			return fmt.Errorf("position_state_events insert (CLOSED blind): %w", err)
		}
		return nil
	})
}

// UpdateRatchetState persists OnTick ratchet runtime state. status='OPEN'
// の row のみ更新するので、CLOSING / CLOSED への呼び出しは silently
// no-op (0 rows affected で error にしない)。caller が peak monotonic を
// 保つこと。
func (r *PositionRepo) UpdateRatchetState(ctx context.Context, id int64, peakUnrealizedPips float64, armed bool, troughUnrealizedPips float64, lossArmed bool) error {
	_, err := r.q.UpdatePositionRatchetState(ctx, dbgen.UpdatePositionRatchetStateParams{
		ID:                   id,
		PeakUnrealizedPips:   peakUnrealizedPips,
		RatchetArmed:         armed,
		TroughUnrealizedPips: troughUnrealizedPips,
		LossRatchetArmed:     lossArmed,
	})
	if err != nil {
		return fmt.Errorf("positions update ratchet state: %w", err)
	}
	return nil
}

// ExtendMaxHold は max_hold_minutes に addMinutes を加算する (延長ボタン)。
// status='OPEN' の row のみ対象。マッチしなければ RETURNING が 0 行 →
// pgx.ErrNoRows となり、(nil, nil) を返して「対象なし」を表す (CLOSING /
// CLOSED / 未知 id)。成功時は加算後の合計と opened_at を返す。
func (r *PositionRepo) ExtendMaxHold(ctx context.Context, id int64, addMinutes int) (*port.MaxHoldExtended, error) {
	row, err := r.q.ExtendPositionMaxHold(ctx, dbgen.ExtendPositionMaxHoldParams{
		AddMinutes: int32(addMinutes),
		ID:         id,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("positions extend max hold: %w", err)
	}
	return &port.MaxHoldExtended{
		MaxHoldMinutes: int(row.MaxHoldMinutes),
		OpenedAt:       row.OpenedAt.Time,
	}, nil
}
