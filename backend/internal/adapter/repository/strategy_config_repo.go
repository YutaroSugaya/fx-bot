package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/adapter/repository/dbgen"
	"fx-bot/backend/internal/port"
)

// StrategyConfigRepo persists strategy_configs rows + the rejection /
// parse-failure junctions. Insert and MarkActive are transactional so
// the main row + matching junction row land together (or not at all).
type StrategyConfigRepo struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
}

func NewStrategyConfigRepo(pool *pgxpool.Pool) *StrategyConfigRepo {
	return &StrategyConfigRepo{pool: pool, q: dbgen.New(pool)}
}

func (r *StrategyConfigRepo) Insert(ctx context.Context, rec port.StrategyConfigRecord) error {
	return withTx(ctx, r.pool, r.q, func(q *dbgen.Queries) error {
		if err := insertStrategyConfigRow(ctx, q, rec); err != nil {
			return err
		}
		return insertStrategyConfigJunctions(ctx, q, rec, time.Time{})
	})
}

func (r *StrategyConfigRepo) MarkExpired(ctx context.Context, configID string) error {
	if err := r.q.MarkStrategyConfigExpired(ctx, configID); err != nil {
		return fmt.Errorf("strategy_configs mark expired: %w", err)
	}
	return nil
}

// MarkActive flips status to 'active' AND inserts a
// strategy_config_activations row (UPSERT — re-promotion is idempotent)
// in one Tx.
func (r *StrategyConfigRepo) MarkActive(ctx context.Context, configID string, at time.Time) error {
	return withTx(ctx, r.pool, r.q, func(q *dbgen.Queries) error {
		if err := q.MarkStrategyConfigActive(ctx, configID); err != nil {
			return fmt.Errorf("strategy_configs mark active: %w", err)
		}
		if err := q.UpsertStrategyConfigActivation(ctx, dbgen.UpsertStrategyConfigActivationParams{
			ConfigID:    configID,
			ActivatedAt: pgts(at),
		}); err != nil {
			return fmt.Errorf("strategy_config_activations insert: %w", err)
		}
		return nil
	})
}

func (r *StrategyConfigRepo) ListRecent(ctx context.Context, limit int) ([]port.StrategyConfigRecordWithMeta, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := r.q.ListRecentStrategyConfigs(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("strategy_configs list recent: %w", err)
	}
	out := make([]port.StrategyConfigRecordWithMeta, 0, len(rows))
	for _, row := range rows {
		out = append(out, port.StrategyConfigRecordWithMeta{
			StrategyConfigRecord: port.StrategyConfigRecord{
				ConfigID:               row.ConfigID,
				Source:                 row.Source,
				Mode:                   row.Mode,
				Symbol:                 row.Symbol,
				Enabled:                row.Enabled,
				MarketRegimeType:       row.MarketRegimeType,
				MarketRegimeConfidence: row.MarketRegimeConfidence,
				StrategyName:           row.StrategyName,
				ValidFrom:              pgtsTime(row.ValidFrom),
				ValidUntil:             pgtsTime(row.ValidUntil),
				RawYAML:                row.RawYaml,
				Status:                 port.StrategyConfigStatus(row.Status),
				RejectReason:           row.RejectReason,
			},
			CreatedAt:   pgtsTime(row.CreatedAt),
			ActivatedAt: pgtsTimePtr(row.ActivatedAt),
		})
	}
	return out, nil
}

// GetActive returns the active row for (symbol, mode). The partial unique
// index allows one active per (symbol, mode), so the DB may legitimately
// hold two active rows (paper + live) at once — filtering by both is
// required.
func (r *StrategyConfigRepo) GetActive(ctx context.Context, symbol, mode string) (*port.StrategyConfigRecord, error) {
	row, err := r.q.GetActiveStrategyConfig(ctx, dbgen.GetActiveStrategyConfigParams{
		Symbol: symbol,
		Mode:   mode,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("strategy_configs get active: %w", err)
	}
	return &port.StrategyConfigRecord{
		ConfigID:               row.ConfigID,
		Source:                 row.Source,
		Mode:                   row.Mode,
		Symbol:                 row.Symbol,
		Enabled:                row.Enabled,
		MarketRegimeType:       row.MarketRegimeType,
		MarketRegimeConfidence: row.MarketRegimeConfidence,
		StrategyName:           row.StrategyName,
		ValidFrom:              pgtsTime(row.ValidFrom),
		ValidUntil:             pgtsTime(row.ValidUntil),
		RawYAML:                row.RawYaml,
		Status:                 port.StrategyConfigStatus(row.Status),
		RejectReason:           row.RejectReason,
	}, nil
}

// insertStrategyConfigRow writes the main strategy_configs row inside the
// caller's Tx-scoped Queries, mapping 23505 unique_violation to
// port.ErrDuplicateConfigID so the usecase layer can retry with a
// suffixed config_id.
func insertStrategyConfigRow(ctx context.Context, q *dbgen.Queries, rec port.StrategyConfigRecord) error {
	err := q.InsertStrategyConfig(ctx, dbgen.InsertStrategyConfigParams{
		ConfigID:               rec.ConfigID,
		Source:                 rec.Source,
		Mode:                   rec.Mode,
		Symbol:                 rec.Symbol,
		Enabled:                rec.Enabled,
		MarketRegimeType:       nullableString(rec.MarketRegimeType),
		MarketRegimeConfidence: rec.MarketRegimeConfidence,
		StrategyName:           nullableString(rec.StrategyName),
		ValidFrom:              pgts(rec.ValidFrom),
		ValidUntil:             pgts(rec.ValidUntil),
		RawYaml:                rec.RawYAML,
		Status:                 string(rec.Status),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("strategy_configs insert: %w: %s", port.ErrDuplicateConfigID, pgErr.Message)
		}
		return fmt.Errorf("strategy_configs insert: %w", err)
	}
	return nil
}

// insertStrategyConfigJunctions writes whichever junction rows the
// record demands: rejection (when RejectReason set), parse_failure (when
// ParseFailureRaw set), activation (when status='active' AND activatedAt
// is non-zero). Idempotent for re-inserts via ON CONFLICT.
func insertStrategyConfigJunctions(ctx context.Context, q *dbgen.Queries, rec port.StrategyConfigRecord, activatedAt time.Time) error {
	if rec.RejectReason != "" {
		if err := q.UpsertStrategyConfigRejection(ctx, dbgen.UpsertStrategyConfigRejectionParams{
			ConfigID: rec.ConfigID,
			Reason:   rec.RejectReason,
		}); err != nil {
			return fmt.Errorf("strategy_config_rejections insert: %w", err)
		}
	}
	if rec.ParseFailureRaw != "" {
		if err := q.UpsertStrategyConfigParseFailure(ctx, dbgen.UpsertStrategyConfigParseFailureParams{
			ConfigID: rec.ConfigID,
			RawInput: rec.ParseFailureRaw,
		}); err != nil {
			return fmt.Errorf("strategy_config_parse_failures insert: %w", err)
		}
	}
	if rec.Status == port.StrategyConfigStatusActive && !activatedAt.IsZero() {
		if err := q.UpsertStrategyConfigActivation(ctx, dbgen.UpsertStrategyConfigActivationParams{
			ConfigID:    rec.ConfigID,
			ActivatedAt: pgts(activatedAt),
		}); err != nil {
			return fmt.Errorf("strategy_config_activations insert: %w", err)
		}
	}
	return nil
}
