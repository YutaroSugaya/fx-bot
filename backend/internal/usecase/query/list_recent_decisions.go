package query

import (
	"context"
	"fmt"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
)

// ListRecentDecisionsInput は ListRecentDecisionsQuery.Execute の引数。
type ListRecentDecisionsInput struct {
	Limit int // 0 → 20 (default), >200 → 200 (cap)
}

// AdvisorDecisionView は GET /api/advisor/recent の各行用 DTO。
// raw_yaml から表示用フィールドだけ取り出した derived view。
type AdvisorDecisionView struct {
	ConfigID                string     `json:"config_id"`
	Status                  string     `json:"status"`
	Source                  string     `json:"source"`
	StrategyName            string     `json:"strategy_name"`
	Enabled                 bool       `json:"enabled"`
	MarketRegimeType        string     `json:"market_regime_type"`
	MarketRegimeConfidence  float64    `json:"market_regime_confidence"`
	MarketRegimeReason      string     `json:"market_regime_reason,omitempty"`
	Direction               string     `json:"direction,omitempty"`
	TakeProfitPips          float64    `json:"take_profit_pips,omitempty"`
	StopLossPips            float64    `json:"stop_loss_pips,omitempty"`
	MaxHoldMinutes          int        `json:"max_hold_minutes,omitempty"`
	MaxSpreadPips           float64    `json:"max_spread_pips,omitempty"`
	NoTradeReason           string     `json:"no_trade_reason,omitempty"`
	NextAdvisorRunInMinutes int        `json:"next_advisor_run_in_minutes,omitempty"`
	GeneratedAt             time.Time  `json:"generated_at"`
	ValidFrom               time.Time  `json:"valid_from"`
	ValidUntil              time.Time  `json:"valid_until"`
	CreatedAt               time.Time  `json:"created_at"`
	ActivatedAt             *time.Time `json:"activated_at,omitempty"`
	RejectReason            string     `json:"reject_reason,omitempty"`
}

// ListRecentDecisionsQuery is a read-only CQRS Query.
//
// GET /api/advisor/recent を裏で支える。raw_yaml をパースして表示用に
// 必要なフィールドだけ取り出す (集計・派生値は Query 層で計算する規約)。
type ListRecentDecisionsQuery struct {
	StrategyConfigs port.StrategyConfigRepository
}

// Execute returns up to N recent decisions converted to view DTOs.
func (q *ListRecentDecisionsQuery) Execute(ctx context.Context, in ListRecentDecisionsInput) ([]AdvisorDecisionView, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := q.StrategyConfigs.ListRecent(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("recent: %w", err)
	}
	out := make([]AdvisorDecisionView, 0, len(rows))
	for _, rec := range rows {
		view := AdvisorDecisionView{
			ConfigID:               rec.ConfigID,
			Status:                 string(rec.Status),
			Source:                 rec.Source,
			StrategyName:           rec.StrategyName,
			Enabled:                rec.Enabled,
			MarketRegimeType:       rec.MarketRegimeType,
			MarketRegimeConfidence: rec.MarketRegimeConfidence,
			GeneratedAt:            rec.ValidFrom, // proxy until parsed cfg overrides
			ValidFrom:              rec.ValidFrom,
			ValidUntil:             rec.ValidUntil,
			CreatedAt:              rec.CreatedAt,
			ActivatedAt:            rec.ActivatedAt,
			RejectReason:           rec.RejectReason,
		}
		if cfg, err := config.ParseStrategyConfig([]byte(rec.RawYAML)); err == nil && cfg != nil {
			view.GeneratedAt = cfg.GeneratedAt
			view.MarketRegimeReason = cfg.MarketRegime.Reason
			view.Direction = string(cfg.Entry.Direction)
			view.TakeProfitPips = cfg.Exit.TakeProfitPips
			view.StopLossPips = cfg.Exit.StopLossPips
			view.MaxHoldMinutes = cfg.Exit.MaxHoldMinutes
			view.MaxSpreadPips = cfg.Entry.MaxSpreadPips
			view.NoTradeReason = cfg.NoTrade.Reason
			view.NextAdvisorRunInMinutes = cfg.NextAdvisorRunInMinutes
		}
		out = append(out, view)
	}
	return out, nil
}
