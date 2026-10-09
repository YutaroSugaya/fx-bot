package query

import (
	"context"
	"fmt"
	"time"

	"fx-bot/backend/internal/port"
)

// ListTradesInput は ListTradesQuery.Execute の引数。
type ListTradesInput struct {
	Limit int // 0 → 50 (default), >500 → 500 (cap)
}

// TradeView is the read-only DTO exposed by /api/trades. The
// Query layer must NOT leak port.TradeRecord — the
// frontend / API consumers see a stable snake_case JSON contract, decoupled
// from the storage schema. New columns on port.TradeRecord do NOT become
// part of the API by accident; they must be explicitly added here.
type TradeView struct {
	PositionID       int64     `json:"position_id"`
	SignalID         string    `json:"signal_id,omitempty"`
	StrategyConfigID string    `json:"strategy_config_id"`
	Symbol           string    `json:"symbol"`
	Side             string    `json:"side"`
	Quantity         int       `json:"quantity"`
	EntryPrice       float64   `json:"entry_price"`
	ExitPrice        float64   `json:"exit_price"`
	ProfitLossPips   float64   `json:"profit_loss_pips"`
	ProfitLossJPY    float64   `json:"profit_loss_jpy"`
	CloseReason      string    `json:"close_reason"`
	OpenedAt         time.Time `json:"opened_at"`
	ClosedAt         time.Time `json:"closed_at"`
	// Origin: "bot" (default) | "external" (GMO-app entry adopted by reconcile)
	// | "manual" (manual_trade command). Lets the dashboard label the operator's
	// own discretionary trades distinctly from bot trades. See port.TradeOrigin.
	Origin string `json:"origin"`
}

// ListTradesQuery is a read-only CQRS Query.
//
// GET /api/trades を裏で支える。直近 30 日の trade を引いて返す。
type ListTradesQuery struct {
	Trades port.TradeRepository
	Clock  func() time.Time
	// Epoch floors closed_at: trades closed before it are excluded so the dashboard's
	// cumulative P&L / win-rate / avg / max aggregates count ONLY from the current strategy
	// regime (since the configured epoch), not prior strategies. Zero = no floor (30-day default only).
	// Old trades stay in the DB; this only scopes what /api/trades returns.
	Epoch time.Time
}

// Execute returns closed trades within the last 30 days as TradeView DTOs.
func (q *ListTradesQuery) Execute(ctx context.Context, in ListTradesInput) ([]TradeView, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	now := time.Now()
	if q.Clock != nil {
		now = q.Clock()
	}
	since := now.AddDate(0, 0, -30)
	if !q.Epoch.IsZero() && q.Epoch.After(since) {
		since = q.Epoch
	}
	rows, err := q.Trades.ListSince(ctx, since, limit)
	if err != nil {
		return nil, fmt.Errorf("list trades: %w", err)
	}
	out := make([]TradeView, len(rows))
	for i, r := range rows {
		out[i] = TradeView{
			PositionID:       r.PositionID,
			SignalID:         r.SignalID,
			StrategyConfigID: r.StrategyConfigID,
			Symbol:           r.Symbol,
			Side:             r.Side,
			Quantity:         r.Quantity,
			EntryPrice:       r.EntryPrice,
			ExitPrice:        r.ExitPrice,
			ProfitLossPips:   r.ProfitLossPips,
			ProfitLossJPY:    r.ProfitLossJPY,
			CloseReason:      r.CloseReason,
			OpenedAt:         r.OpenedAt,
			ClosedAt:         r.ClosedAt,
			Origin:           r.Origin,
		}
	}
	return out, nil
}
