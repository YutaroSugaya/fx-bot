package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/adapter/repository/dbgen"
	"fx-bot/backend/internal/port"
)

// TradeRepo writes the trades main row + the optional trade_signals
// junction (auto trades have a signal id; manual close has none).
// Production close flow goes through PositionCloserRepo.CloseAndRecord
// which bundles trade insert into the same Tx as the position state
// flip; this standalone Insert is used by tests that just need a
// ledger entry.
type TradeRepo struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
}

func NewTradeRepo(pool *pgxpool.Pool) *TradeRepo {
	return &TradeRepo{pool: pool, q: dbgen.New(pool)}
}

func (r *TradeRepo) Insert(ctx context.Context, rec port.TradeRecord) error {
	return withTx(ctx, r.pool, r.q, func(q *dbgen.Queries) error {
		return insertTradeRow(ctx, q, rec)
	})
}

// insertTradeRow inserts the trades row and, when rec.SignalID is set, the
// trade_signals junction, on the supplied Queries handle (which may be Tx-bound
// via WithTx). Shared by TradeRepo.Insert and PositionCloserRepo.CloseAndRecord
// so the trades / trade_signals column mapping has a single source of truth
// (auto trades carry a signal id; manual close has none).
func insertTradeRow(ctx context.Context, q *dbgen.Queries, rec port.TradeRecord) error {
	id, err := q.InsertTrade(ctx, dbgen.InsertTradeParams{
		PositionID:       rec.PositionID,
		StrategyConfigID: rec.StrategyConfigID,
		Symbol:           rec.Symbol,
		Side:             rec.Side,
		Quantity:         int32(rec.Quantity),
		EntryPrice:       rec.EntryPrice,
		ExitPrice:        rec.ExitPrice,
		ProfitLossPips:   rec.ProfitLossPips,
		ProfitLossJpy:    rec.ProfitLossJPY,
		CloseReason:      rec.CloseReason,
		FeeJpy:           rec.FeeJPY,
		SwapJpy:          rec.SwapJPY,
		FeeEstimated:     rec.FeeEstimated,
		OpenedAt:         pgts(rec.OpenedAt),
		ClosedAt:         pgts(rec.ClosedAt),
	})
	if err != nil {
		return fmt.Errorf("trades insert: %w", err)
	}
	if rec.SignalID != "" {
		if err := q.InsertTradeSignal(ctx, dbgen.InsertTradeSignalParams{
			TradeID:  id,
			SignalID: rec.SignalID,
		}); err != nil {
			return fmt.Errorf("trade_signals insert: %w", err)
		}
	}
	return nil
}

func (r *TradeRepo) ListSince(ctx context.Context, since time.Time, limit int) ([]port.TradeRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.q.ListTradesSinceOpened(ctx, dbgen.ListTradesSinceOpenedParams{
		OpenedAt: pgts(since),
		Limit:    int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("trades list since opened: %w", err)
	}
	out := make([]port.TradeRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, port.TradeRecord{
			PositionID:       row.PositionID,
			SignalID:         row.SignalID,
			StrategyConfigID: row.StrategyConfigID,
			Symbol:           row.Symbol,
			Side:             row.Side,
			Quantity:         int(row.Quantity),
			EntryPrice:       row.EntryPrice,
			ExitPrice:        row.ExitPrice,
			ProfitLossPips:   row.ProfitLossPips,
			ProfitLossJPY:    row.ProfitLossJpy,
			CloseReason:      row.CloseReason,
			FeeJPY:           row.FeeJpy,
			SwapJPY:          row.SwapJpy,
			FeeEstimated:     row.FeeEstimated,
			OpenedAt:         pgtsTime(row.OpenedAt),
			ClosedAt:         pgtsTime(row.ClosedAt),
			Origin:           row.Origin,
		})
	}
	return out, nil
}

func (r *TradeRepo) SumLossJPYSince(ctx context.Context, since time.Time) (int, error) {
	v, err := r.q.SumOpenedLossJPYSince(ctx, pgts(since))
	if err != nil {
		return 0, fmt.Errorf("trades sum loss: %w", err)
	}
	return int(v), nil
}

func (r *TradeRepo) CountSince(ctx context.Context, since time.Time) (int, error) {
	v, err := r.q.CountOpenedSince(ctx, pgts(since))
	if err != nil {
		return 0, fmt.Errorf("trades count: %w", err)
	}
	return int(v), nil
}

// closed_at-based aggregates for the risk gate.
func (r *TradeRepo) ListClosedSince(ctx context.Context, since time.Time, limit int) ([]port.TradeRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.q.ListTradesClosedSince(ctx, dbgen.ListTradesClosedSinceParams{
		ClosedAt: pgts(since),
		Limit:    int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("trades list closed since: %w", err)
	}
	out := make([]port.TradeRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, port.TradeRecord{
			PositionID:       row.PositionID,
			SignalID:         row.SignalID,
			StrategyConfigID: row.StrategyConfigID,
			Symbol:           row.Symbol,
			Side:             row.Side,
			Quantity:         int(row.Quantity),
			EntryPrice:       row.EntryPrice,
			ExitPrice:        row.ExitPrice,
			ProfitLossPips:   row.ProfitLossPips,
			ProfitLossJPY:    row.ProfitLossJpy,
			CloseReason:      row.CloseReason,
			FeeJPY:           row.FeeJpy,
			SwapJPY:          row.SwapJpy,
			FeeEstimated:     row.FeeEstimated,
			OpenedAt:         pgtsTime(row.OpenedAt),
			ClosedAt:         pgtsTime(row.ClosedAt),
			Origin:           row.Origin,
		})
	}
	return out, nil
}

func (r *TradeRepo) SumClosedLossJPYSince(ctx context.Context, since time.Time) (int, error) {
	v, err := r.q.SumClosedLossJPYSince(ctx, pgts(since))
	if err != nil {
		return 0, fmt.Errorf("trades sum closed loss: %w", err)
	}
	return int(v), nil
}

func (r *TradeRepo) CountClosedSince(ctx context.Context, since time.Time) (int, error) {
	v, err := r.q.CountClosedSince(ctx, pgts(since))
	if err != nil {
		return 0, fmt.Errorf("trades count closed: %w", err)
	}
	return int(v), nil
}

// Per-symbol closed_at aggregates for the risk gate. Mirror of the
// account-wide methods above, filtered by symbol so a sibling symbol's
// trades cannot pollute this bundle's per-symbol caps.

func (r *TradeRepo) ListClosedBySymbolSince(ctx context.Context, symbol string, since time.Time, limit int) ([]port.TradeRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.q.ListTradesClosedBySymbolSince(ctx, dbgen.ListTradesClosedBySymbolSinceParams{
		Symbol: symbol, ClosedAt: pgts(since), Limit: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("trades list closed by symbol: %w", err)
	}
	out := make([]port.TradeRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, port.TradeRecord{
			PositionID:       row.PositionID,
			SignalID:         row.SignalID,
			StrategyConfigID: row.StrategyConfigID,
			Symbol:           row.Symbol,
			Side:             row.Side,
			Quantity:         int(row.Quantity),
			EntryPrice:       row.EntryPrice,
			ExitPrice:        row.ExitPrice,
			ProfitLossPips:   row.ProfitLossPips,
			ProfitLossJPY:    row.ProfitLossJpy,
			CloseReason:      row.CloseReason,
			FeeJPY:           row.FeeJpy,
			SwapJPY:          row.SwapJpy,
			FeeEstimated:     row.FeeEstimated,
			OpenedAt:         pgtsTime(row.OpenedAt),
			ClosedAt:         pgtsTime(row.ClosedAt),
			Origin:           row.Origin,
		})
	}
	return out, nil
}

func (r *TradeRepo) SumClosedLossJPYBySymbolSince(ctx context.Context, symbol string, since time.Time) (int, error) {
	v, err := r.q.SumClosedLossJPYBySymbolSince(ctx, dbgen.SumClosedLossJPYBySymbolSinceParams{
		Symbol: symbol, ClosedAt: pgts(since),
	})
	if err != nil {
		return 0, fmt.Errorf("trades sum closed loss by symbol: %w", err)
	}
	return int(v), nil
}

func (r *TradeRepo) CountClosedBySymbolSince(ctx context.Context, symbol string, since time.Time) (int, error) {
	v, err := r.q.CountClosedBySymbolSince(ctx, dbgen.CountClosedBySymbolSinceParams{
		Symbol: symbol, ClosedAt: pgts(since),
	})
	if err != nil {
		return 0, fmt.Errorf("trades count closed by symbol: %w", err)
	}
	return int(v), nil
}

// Dashboard (/api/status) 用の符号付き PnL 集計と早期 exit 発火数。

func (r *TradeRepo) SumPnLJPYClosedSinceBySymbol(ctx context.Context, symbol string, since time.Time) (float64, error) {
	v, err := r.q.SumPnLJPYClosedSinceBySymbol(ctx, dbgen.SumPnLJPYClosedSinceBySymbolParams{
		Symbol: symbol, ClosedAt: pgts(since),
	})
	if err != nil {
		return 0, fmt.Errorf("trades sum pnl by symbol: %w", err)
	}
	return v, nil
}

func (r *TradeRepo) CountEarlyExitTradesSinceBySymbol(ctx context.Context, symbol string, since time.Time) (int, error) {
	v, err := r.q.CountEarlyExitTradesSinceBySymbol(ctx, dbgen.CountEarlyExitTradesSinceBySymbolParams{
		Symbol: symbol, ClosedAt: pgts(since),
	})
	if err != nil {
		return 0, fmt.Errorf("trades count early-exit by symbol: %w", err)
	}
	return int(v), nil
}
