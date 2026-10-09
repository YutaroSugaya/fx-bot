package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/adapter/repository/dbgen"
	"fx-bot/backend/internal/port"
)

// StrategyConfigPromoterRepo implements port.StrategyConfigPromoter.
// 「旧 active を expired にして新 active を入れる」+ activation 行 insert
// を 1 Tx で行うことで、MarkExpired 成功 → Insert 失敗 → "active 行が
// 一時的に存在しない" 空白期間を排除する。
type StrategyConfigPromoterRepo struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
}

func NewStrategyConfigPromoter(pool *pgxpool.Pool) *StrategyConfigPromoterRepo {
	return &StrategyConfigPromoterRepo{pool: pool, q: dbgen.New(pool)}
}

// PromoteActive expires any current active row for (newRec.Symbol,
// newRec.Mode), inserts newRec as active, and writes the
// strategy_config_activations row — all in one Tx.
//
// 23505 (unique_violation) during INSERT は port.ErrDuplicateConfigID に
// 包んで返す。Tx は rollback されるので prev active のまま残り、呼び出し
// 側が config_id を suffix retry できる。
//
// prevConfigID is kept on the signature for compatibility with the
// pre-squash API but no longer drives the expire step — every active row
// for (symbol, mode) is expired so stale caller state can't desync the
// "single active per (symbol, mode)" invariant.
func (r *StrategyConfigPromoterRepo) PromoteActive(ctx context.Context, prevConfigID string, newRec port.StrategyConfigRecord) error {
	_ = prevConfigID
	return withTx(ctx, r.pool, r.q, func(q *dbgen.Queries) error {
		if err := q.ExpireOtherActiveConfigs(ctx, dbgen.ExpireOtherActiveConfigsParams{
			Symbol:   newRec.Symbol,
			Mode:     newRec.Mode,
			ConfigID: newRec.ConfigID,
		}); err != nil {
			return fmt.Errorf("expire prev active by symbol+mode: %w", err)
		}
		if err := insertStrategyConfigRow(ctx, q, newRec); err != nil {
			return err
		}
		// Activation timestamp comes from now() so audit ordering is
		// preserved even when the caller's clock is slightly off.
		return insertStrategyConfigJunctions(ctx, q, newRec, time.Now())
	})
}
