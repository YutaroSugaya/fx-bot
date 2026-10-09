package command

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/strategy"
	"fx-bot/backend/internal/port"
)

// --- per-symbol filter + account-wide aggregation under admission ---
//
// EntryAdmission.snapshot is the authoritative pre-trade aggregator under the
// shared entry mutex. It must:
//   1. Filters per-symbol caps via *BySymbol queries (sibling-symbol trades
//      must NOT count toward this bundle's per-symbol cap).
//   2. Populates the account-wide AccountSnapshot fields so risk.Gate's
//      account_open_positions / account_daily_loss branches can fire.

func newPhase4Admission(t *testing.T, sym string, posRepo port.PositionRepository, tradeRepo port.TradeRepository, flagPath string, risk config.RiskSection, mu *sync.Mutex) *EntryAdmission {
	t.Helper()
	if mu == nil {
		mu = &sync.Mutex{}
	}
	return &EntryAdmission{
		Symbol:    sym,
		Mutex:     mu,
		Positions: posRepo,
		Trades:    tradeRepo,
		BotConfig: &config.BotConfig{
			Bot:    config.BotSection{Mode: config.ModePaperConfig, Timezone: "UTC"},
			Symbol: sym, // legacy field still populated by Normalize()
			Risk:   risk,
		},
		EmergencyFlagPath: flagPath,
		Logger:            silentLogger(),
		Clock:             time.Now,
	}
}

func TestEntryAdmission_PerSymbolCapIgnoresSiblingTrades(t *testing.T) {
	// Sibling symbol's losing trade must NOT count toward THIS bundle's
	// per-symbol max_loss_in_window cap. Previously the account-wide
	// CountClosedSince / SumClosedLossJPYSince were used for the per-symbol
	// gate; that breaks the moment two symbols share the same active window.
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	now := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)

	// EUR_JPY losses (sibling) — must not pollute USD_JPY's per-symbol cap.
	for i, pnl := range []float64{-400, -500} {
		_ = tradeRepo.Insert(context.Background(), port.TradeRecord{
			Symbol: "EUR_JPY", Side: "BUY", Quantity: 100,
			ProfitLossJPY: pnl,
			OpenedAt:      now.Add(time.Duration(i) * time.Minute),
			ClosedAt:      now.Add(time.Duration(i+1) * time.Minute),
		})
	}

	a := newPhase4Admission(t, "USD_JPY", posRepo, tradeRepo, flag,
		config.RiskSection{MaxOpenPositions: 5}, nil)
	// per-symbol active config with a tight loss cap (300) — USD_JPY itself
	// has zero trades so it should be allowed; if the gate aggregated EUR_JPY
	// losses (900) it would reject with loss_in_window.
	cfg := &config.StrategyConfig{
		ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
		Risk: config.ConfigRiskSection{
			MaxOpenPositions: 5, MaxLossInThisWindowJPY: 300, MaxTradesInThisWindow: 5,
		},
		Entry: config.EntrySection{Direction: config.DirectionBoth},
	}

	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	verdict, release, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal: sig, ActiveConfig: cfg, Source: "auto",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if !verdict.Allowed {
		t.Fatalf("per-symbol loss cap must not fire on sibling trades; got reason=%q", verdict.Reason)
	}
	if release != nil {
		release()
	}
}

func TestEntryAdmission_AccountWideOpenCapBlocksAcrossSymbols(t *testing.T) {
	// Account-wide AccountMaxOpenPositions blocks when a sibling symbol
	// already holds the global slot — even though THIS bundle has no
	// per-symbol open.
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	now := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)

	// EUR_JPY holds 1 open position (sibling, not USD_JPY).
	_, _ = posRepo.Insert(context.Background(), port.PositionInsertInput{Position: port.PositionRecord{
		Symbol: "EUR_JPY", Status: port.PositionStatusOpen, OpenedAt: now,
	}})

	a := newPhase4Admission(t, "USD_JPY", posRepo, tradeRepo, flag,
		config.RiskSection{MaxOpenPositions: 5, AccountMaxOpenPositions: 1}, nil)

	cfg := &config.StrategyConfig{
		ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
		Risk:  config.ConfigRiskSection{MaxOpenPositions: 5},
		Entry: config.EntrySection{Direction: config.DirectionBoth},
	}
	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}
	verdict, _, err := a.CheckAndHold(context.Background(), AdmissionRequest{
		Signal: sig, ActiveConfig: cfg, Source: "auto",
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	if verdict.Allowed {
		t.Fatalf("account-wide open cap must block; got Allowed")
	}
}

func TestEntryAdmission_SharedMutexSerializesAccountCap(t *testing.T) {
	// Two bundles (USD_JPY + EUR_JPY) share the entry mutex. With
	// AccountMaxOpenPositions=1 and zero open positions, concurrent
	// admissions race: exactly one must win (because the loser sees the
	// winner's just-inserted position under the lock).
	flag := filepath.Join(t.TempDir(), "emergency_stop.flag")
	posRepo := backtest.NewInMemoryPositionRepo()
	tradeRepo := backtest.NewInMemoryTradeRepo()
	now := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	shared := &sync.Mutex{}

	mkCfg := func() *config.StrategyConfig {
		return &config.StrategyConfig{
			ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
			Risk:  config.ConfigRiskSection{MaxOpenPositions: 5},
			Entry: config.EntrySection{Direction: config.DirectionBoth},
		}
	}

	usd := newPhase4Admission(t, "USD_JPY", posRepo, tradeRepo, flag,
		config.RiskSection{MaxOpenPositions: 5, AccountMaxOpenPositions: 1}, shared)
	eur := newPhase4Admission(t, "EUR_JPY", posRepo, tradeRepo, flag,
		config.RiskSection{MaxOpenPositions: 5, AccountMaxOpenPositions: 1}, shared)

	sig := strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, TakeProfitPips: 10, StopLossPips: 10}

	type result struct {
		allowed bool
		sym     string
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	// Caller pattern (matches ExecuteOrder.OnSignal): take the verdict, if
	// allowed, simulate a successful PlaceOrder by inserting an OPEN row
	// BEFORE releasing the lock. That's what the production caller does;
	// without it, the test would not exercise the account-wide cap race.
	run := func(a *EntryAdmission, sym string) {
		defer wg.Done()
		verdict, release, err := a.CheckAndHold(context.Background(), AdmissionRequest{
			Signal: sig, ActiveConfig: mkCfg(), Source: "auto",
		})
		if err != nil {
			t.Errorf("CheckAndHold(%s): %v", sym, err)
		}
		if verdict.Allowed {
			_, _ = posRepo.Insert(context.Background(), port.PositionInsertInput{Position: port.PositionRecord{
				Symbol: sym, Status: port.PositionStatusOpen, OpenedAt: now,
			}})
		}
		if release != nil {
			release()
		}
		results <- result{allowed: verdict.Allowed, sym: sym}
	}
	go run(usd, "USD_JPY")
	go run(eur, "EUR_JPY")
	wg.Wait()
	close(results)

	allowedCount := 0
	for r := range results {
		if r.allowed {
			allowedCount++
		}
	}
	if allowedCount != 1 {
		t.Errorf("expected exactly 1 winner under account cap=1, got %d", allowedCount)
	}
}
