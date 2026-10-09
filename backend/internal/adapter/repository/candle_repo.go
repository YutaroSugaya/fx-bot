package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/adapter/repository/dbgen"
	"fx-bot/backend/internal/port"
)

// CandleRepo persists OHLCV bars to the candles table.
// Upsert key is (symbol, timeframe, opened_at).
type CandleRepo struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
}

func NewCandleRepo(pool *pgxpool.Pool) *CandleRepo {
	return &CandleRepo{pool: pool, q: dbgen.New(pool)}
}

func (r *CandleRepo) Upsert(ctx context.Context, rec port.CandleRecord) error {
	if err := r.q.UpsertCandle(ctx, dbgen.UpsertCandleParams{
		Symbol:    rec.Symbol,
		Timeframe: rec.Timeframe,
		OpenedAt:  pgts(rec.OpenedAt),
		Open:      rec.Open,
		High:      rec.High,
		Low:       rec.Low,
		Close:     rec.Close,
		Volume:    rec.Volume,
	}); err != nil {
		return fmt.Errorf("candles upsert: %w", err)
	}
	return nil
}

// UpsertBatch upserts many rows in one transaction. Used by bootstrap
// when the bot starts cold and pulls a full 24h of klines.
func (r *CandleRepo) UpsertBatch(ctx context.Context, recs []port.CandleRecord) error {
	if len(recs) == 0 {
		return nil
	}
	return withTx(ctx, r.pool, r.q, func(q *dbgen.Queries) error {
		for _, rec := range recs {
			if err := q.UpsertCandle(ctx, dbgen.UpsertCandleParams{
				Symbol:    rec.Symbol,
				Timeframe: rec.Timeframe,
				OpenedAt:  pgts(rec.OpenedAt),
				Open:      rec.Open,
				High:      rec.High,
				Low:       rec.Low,
				Close:     rec.Close,
				Volume:    rec.Volume,
			}); err != nil {
				return fmt.Errorf("candles upsert_batch row: %w", err)
			}
		}
		return nil
	})
}

func (r *CandleRepo) ListSince(ctx context.Context, symbol, timeframe string, since time.Time, limit int) ([]port.CandleRecord, error) {
	cap := int32(limit)
	if cap <= 0 {
		cap = 1_000_000 // 24h × 1m = 1440; effectively unbounded.
	}
	rows, err := r.q.ListCandlesSince(ctx, dbgen.ListCandlesSinceParams{
		Symbol:    symbol,
		Timeframe: timeframe,
		OpenedAt:  pgts(since),
		Limit:     cap,
	})
	if err != nil {
		return nil, fmt.Errorf("candles list: %w", err)
	}
	out := make([]port.CandleRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, port.CandleRecord{
			ID:        row.ID,
			Symbol:    row.Symbol,
			Timeframe: row.Timeframe,
			OpenedAt:  pgtsTime(row.OpenedAt),
			Open:      row.Open,
			High:      row.High,
			Low:       row.Low,
			Close:     row.Close,
			Volume:    row.Volume,
			CreatedAt: pgtsTime(row.CreatedAt),
			UpdatedAt: pgtsTime(row.UpdatedAt),
		})
	}
	return out, nil
}
