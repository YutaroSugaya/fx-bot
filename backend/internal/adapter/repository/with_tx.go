package repository

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/adapter/repository/dbgen"
)

// 各 repo が
//
//	pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
//	    q := r.q.WithTx(tx)
//	    // ...
//	})
//
// と同じ shape を繰り返す boilerplate を集約する。
// helper は tx 開始 / WithTx 変換 / rollback 保証 (pgx.BeginFunc が担当) を
// 隠蔽し、callsite は 純粋な business logic だけ書けば良くなる。
//
// 設計上 *Queries を引数に取るのは、各 repo の dbgen 派生 queries を共通の
// 型で受けたいから (各 repo は自分の r.q を持っているが Queries 型は同一)。

// withTx は pool で tx を開始し、tx-bound な Queries を fn に渡す。
// fn が error を返したら rollback、nil で commit。pgx.BeginFunc の薄いラッパ。
//
// 使用例:
//
//	return withTx(ctx, r.pool, r.q, func(q *dbgen.Queries) error {
//	    return q.InsertCandle(ctx, params)
//	})
func withTx(
	ctx context.Context,
	pool *pgxpool.Pool,
	q *dbgen.Queries,
	fn func(*dbgen.Queries) error,
) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return fn(q.WithTx(tx))
	})
}
