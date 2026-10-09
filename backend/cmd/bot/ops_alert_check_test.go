package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/port"
)

// Regression pin: the daily-DD alert must measure GROSS realized
// loss (the entry-gate brake's SumClosedLossJPYSince measure), NOT signed net
// PnL. DailyDDAlert's contract is gross-positive; if this caller fed
// SumPnLJPYClosedSinceBySymbol (signed net), the alert would fire on profitable
// days and stay silent on losing ones. These two cases pin
// the correct wiring.
func TestOpsAlertCheck_DDAlertUsesGrossLossNotNetPnL(t *testing.T) {
	loc := time.UTC
	now := func() time.Time { return time.Date(2026, 6, 11, 12, 0, 0, 0, loc) }

	mkCfg := func(repo port.TradeRepository) opsAlertConfig {
		return opsAlertConfig{
			Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
			Trades:          repo,
			Symbols:         []string{"USD_JPY"},
			Loc:             loc,
			MaxDailyLossJPY: 2000,
			NoTradeAfter:    0,  // disable no-trade alert (threshold 0 → nil)
			SummaryHour:     24, // hour is 0-23 → daily summary never fires here
			EdgeWindowDays:  90,
			EdgeLimit:       30,
			Now:             now,
		}
	}
	collect := func(cfg opsAlertConfig) []*port.Event {
		var events []*port.Event
		emit := func(ev *port.Event) {
			if ev != nil {
				events = append(events, ev)
			}
		}
		opsAlertCheck(context.Background(), cfg, now, newOpsAlertState(), emit)
		return events
	}
	hasDD := func(events []*port.Event) bool {
		for _, e := range events {
			if e.Title == "daily_dd_breached" {
				return true
			}
		}
		return false
	}

	t.Run("profitable day (net +5000, gross loss 0) must NOT fire", func(t *testing.T) {
		repo := backtest.NewInMemoryTradeRepo()
		mustInsertOpsTrade(t, repo, "USD_JPY", 5000, now().Add(-time.Hour))
		if hasDD(collect(mkCfg(repo))) {
			t.Fatal("daily_dd_breached fired on a PROFITABLE day (net +5000, gross loss 0) — alert is reading signed net instead of gross loss")
		}
	})

	t.Run("losing day (gross loss 3000 >= cap 2000) must fire", func(t *testing.T) {
		repo := backtest.NewInMemoryTradeRepo()
		mustInsertOpsTrade(t, repo, "USD_JPY", -3000, now().Add(-time.Hour))
		if !hasDD(collect(mkCfg(repo))) {
			t.Fatal("daily_dd_breached did NOT fire on a losing day (gross loss 3000 >= cap 2000) — alert is reading signed net instead of gross loss")
		}
	})
}

func mustInsertOpsTrade(t *testing.T, repo *backtest.InMemoryTradeRepo, sym string, pnl float64, closedAt time.Time) {
	t.Helper()
	if err := repo.Insert(context.Background(), port.TradeRecord{
		Symbol: sym, Side: "BUY", ProfitLossJPY: pnl,
		CloseReason: "take_profit",
		OpenedAt:    closedAt.Add(-time.Hour),
		ClosedAt:    closedAt,
	}); err != nil {
		t.Fatalf("insert trade: %v", err)
	}
}
