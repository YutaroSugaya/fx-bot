package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/adapter/repository/dbgen"
	"fx-bot/backend/internal/port"
)

// SignalRejectionRepo writes signal_rejections rows. The schema requires
// strategy_config_id NOT NULL — callers must resolve the active config at
// the moment of rejection (an empty "pre-config" id is not accepted).
type SignalRejectionRepo struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
}

func NewSignalRejectionRepo(pool *pgxpool.Pool) *SignalRejectionRepo {
	return &SignalRejectionRepo{pool: pool, q: dbgen.New(pool)}
}

func (r *SignalRejectionRepo) Insert(ctx context.Context, rec port.SignalRejection) error {
	if err := r.q.InsertSignalRejection(ctx, dbgen.InsertSignalRejectionParams{
		StrategyConfigID: rec.StrategyConfigID,
		Reason:           rec.Reason,
		Detail:           rec.Detail,
	}); err != nil {
		return fmt.Errorf("signal_rejections insert: %w", err)
	}
	return nil
}
