package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/adapter/repository/dbgen"
	"fx-bot/backend/internal/port"
)

// PositionCloserRepo implements port.PositionCloser. The single Tx
// contains:
//   - positions UPDATE (CLOSING → CLOSED). Uses WHERE status='CLOSING' so a
//     caller that forgot to ClaimForClose first gets rows=0 and trips
//     emergency_stop instead of corrupting the ledger.
//   - position_state_events INSERT('CLOSED')
//   - trades INSERT (NOT NULL columns enforce that close payload is complete)
//   - trade_signals INSERT, when the caller passes a non-empty SignalID
//     (auto trades have a signal id; manual close-and-record does not).
type PositionCloserRepo struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
}

func NewPositionCloserRepo(pool *pgxpool.Pool) *PositionCloserRepo {
	return &PositionCloserRepo{pool: pool, q: dbgen.New(pool)}
}

func (r *PositionCloserRepo) CloseAndRecord(ctx context.Context, positionID int64, closedAt time.Time, trade port.TradeRecord) (bool, error) {
	// trades.position_id is FK to positions(id),
	// so the DB happily accepts any real position id. If caller passes a
	// mismatched trade.PositionID the row gets closed but the trade row
	// is booked against a different position, silently corrupting the
	// ledger. Reject up-front before opening the Tx.
	if trade.PositionID != positionID {
		return false, fmt.Errorf("position_closer: %w (closer=%d trade=%d)", port.ErrTradePositionIDMismatch, positionID, trade.PositionID)
	}
	var ok bool
	err := withTx(ctx, r.pool, r.q, func(q *dbgen.Queries) error {
		rows, err := q.MarkPositionClosed(ctx, positionID)
		if err != nil {
			return fmt.Errorf("positions mark closed: %w", err)
		}
		if rows == 0 {
			ok = false
			return nil
		}
		if err := q.InsertPositionStateEvent(ctx, dbgen.InsertPositionStateEventParams{
			PositionID:     positionID,
			State:          string(port.PositionStatusClosed),
			TransitionedAt: pgts(closedAt),
		}); err != nil {
			return fmt.Errorf("position_state_events insert (CLOSED): %w", err)
		}
		if err := insertTradeRow(ctx, q, trade); err != nil {
			return err
		}
		ok = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return ok, nil
}
