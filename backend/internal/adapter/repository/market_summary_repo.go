package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/adapter/repository/dbgen"
	"fx-bot/backend/internal/port"
)

type MarketSummaryRepo struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
}

func NewMarketSummaryRepo(pool *pgxpool.Pool) *MarketSummaryRepo {
	return &MarketSummaryRepo{pool: pool, q: dbgen.New(pool)}
}

func (r *MarketSummaryRepo) Insert(ctx context.Context, rec port.MarketSummaryRecord) error {
	if err := r.q.InsertMarketSummary(ctx, dbgen.InsertMarketSummaryParams{
		Symbol:        rec.Symbol,
		SummaryWindow: rec.SummaryWindow,
		RawJson:       rec.RawJSON,
	}); err != nil {
		return fmt.Errorf("market_summaries insert: %w", err)
	}
	return nil
}
