package repository

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/port"
)

// NewRepositories wires every concrete repository against the given pgx pool
// and returns the port-level aggregate so callers don't import this package.
func NewRepositories(pool *pgxpool.Pool) *port.Repositories {
	return &port.Repositories{
		StrategyConfigs:  NewStrategyConfigRepo(pool),
		ValidationEvents: NewConfigValidationEventRepo(pool),
		MarketSummaries:  NewMarketSummaryRepo(pool),
		AdvisorRuns:      NewAdvisorRunRepo(pool),
		Positions:        NewPositionRepo(pool),
		Trades:           NewTradeRepo(pool),
		SignalRejections: NewSignalRejectionRepo(pool),
		Candles:          NewCandleRepo(pool),
		Closer:           NewPositionCloserRepo(pool),
		ConfigPromoter:   NewStrategyConfigPromoter(pool),
	}
}
