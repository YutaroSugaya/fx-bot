// Package position models an open or closed FX position held by the bot.
package position

import (
	"time"

	"fx-bot/backend/internal/domain/order"
)

// Status is the lifecycle status of a position.
type Status string

const (
	StatusOpen    Status = "OPEN"
	StatusClosed  Status = "CLOSED"
	StatusUnknown Status = "UNKNOWN"
)

// Position is the bot-side view of a broker position.
// BrokerPositionID is empty for paper positions; ID is the local DB id.
type Position struct {
	ID               int64
	BrokerPositionID string
	Symbol           string
	Side             order.Side
	Quantity         int
	EntryPrice       float64
	TakeProfitPips   float64
	StopLossPips     float64
	MaxHoldMinutes   int
	StrategyConfigID string
	Status           Status
	OpenedAt         time.Time
	ClosedAt         *time.Time
}

// MaxHoldUntil returns when the position must be force-closed.
func (p Position) MaxHoldUntil() time.Time {
	if p.MaxHoldMinutes <= 0 {
		return time.Time{}
	}
	return p.OpenedAt.Add(time.Duration(p.MaxHoldMinutes) * time.Minute)
}
