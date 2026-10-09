package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/adapter/repository/dbgen"
	"fx-bot/backend/internal/port"
)

type ConfigValidationEventRepo struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
}

func NewConfigValidationEventRepo(pool *pgxpool.Pool) *ConfigValidationEventRepo {
	return &ConfigValidationEventRepo{pool: pool, q: dbgen.New(pool)}
}

func (r *ConfigValidationEventRepo) Insert(ctx context.Context, ev port.ConfigValidationEvent) error {
	if err := r.q.InsertConfigValidationEvent(ctx, dbgen.InsertConfigValidationEventParams{
		ConfigID:       ev.ConfigID,
		ValidationType: ev.ValidationType,
		Status:         ev.Status,
		Message:        nullableString(ev.Message),
	}); err != nil {
		return fmt.Errorf("config_validation_events insert: %w", err)
	}
	return nil
}

// CountFailSince は status='fail' の events 件数を返す (Dashboard 用)。
func (r *ConfigValidationEventRepo) CountFailSince(ctx context.Context, since time.Time) (int, error) {
	v, err := r.q.CountValidationFailSince(ctx, pgts(since))
	if err != nil {
		return 0, fmt.Errorf("config_validation_events count fail: %w", err)
	}
	return int(v), nil
}
