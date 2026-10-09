package app

import (
	"context"
	"fmt"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/safety"
	"fx-bot/backend/internal/usecase/command"
)

// BuildPromotionAccountStateInput aggregates the inputs needed to assemble a
// config.AccountState for advisor promotion validation.
//
// Why this exists: cmd/bot/main.go's GetAccountState callback originally
// inlined this logic and omitted ConsecutiveLosses, which silently disabled
// the validator's consecutive_losses cap during promotion. Worker uses the
// same closed_at-based aggregates ([command.DeriveTradeAggregates]) — extracting
// here keeps the two callers in sync.
type BuildPromotionAccountStateInput struct {
	Symbol               string
	EmergencyFlagPath    string
	Timezone             string // bot timezone for day-start (UTC fallback if empty / invalid)
	Now                  func() time.Time
	MaxDailyLossJPY      int
	MaxConsecutiveLosses int
	MaxOpenPositions     int
}

// BuildPromotionAccountState returns a config.AccountState reflecting the
// current open-position count + bot-timezone-day-start daily_loss +
// consecutive_loss count (closed_at DESC). All aggregates match
// Worker.accountSnapshot so promotion and entry gates see the same state.
func BuildPromotionAccountState(
	ctx context.Context,
	positions port.PositionRepository,
	trades port.TradeRepository,
	in BuildPromotionAccountStateInput,
) (config.AccountState, error) {
	now := in.Now
	if now == nil {
		now = time.Now
	}
	open, err := positions.ListOpenOrClosing(ctx, in.Symbol)
	if err != nil {
		return config.AccountState{}, fmt.Errorf("list_open_or_closing: %w", err)
	}

	startOfDay := config.StartOfDayIn(now(), in.Timezone)

	dailyLoss, err := trades.SumClosedLossJPYSince(ctx, startOfDay)
	if err != nil {
		return config.AccountState{}, fmt.Errorf("sum_closed_loss_day: %w", err)
	}
	recent, err := trades.ListClosedSince(ctx, startOfDay, 50)
	if err != nil {
		return config.AccountState{}, fmt.Errorf("list_recent_closed_trades: %w", err)
	}
	agg := command.DeriveTradeAggregates(recent)

	return config.AccountState{
		EmergencyStop:        safety.Active(in.EmergencyFlagPath),
		DailyLossJPY:         dailyLoss,
		MaxDailyLossJPY:      in.MaxDailyLossJPY,
		ConsecutiveLosses:    agg.ConsecutiveLosses,
		MaxConsecutiveLosses: in.MaxConsecutiveLosses,
		OpenPositions:        len(open),
		MaxOpenPositions:     in.MaxOpenPositions,
	}, nil
}
