package query

import (
	"context"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
)

// --- /api/status multi-symbol expansion ---
//
// BotStatusView additively gains:
//   - Symbols []SymbolStatus — per-symbol breakdown (one entry per
//     bot_config.symbols in stable order)
//   - AccountOpenPositions int — sum across symbols
//
// Existing top-level fields (Symbol, OpenPositions, ActiveConfigID, etc.)
// keep reporting the primary symbol so legacy dashboards continue parsing.

func seedOpen(t *testing.T, repo *backtest.InMemoryPositionRepo, sym string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		_, err := repo.Insert(context.Background(), port.PositionInsertInput{Position: port.PositionRecord{
			Symbol: sym, Side: "BUY", Quantity: 100, EntryPrice: 100,
			Status: port.PositionStatusOpen, OpenedAt: time.Now(),
		}})
		if err != nil {
			t.Fatalf("seed %s: %v", sym, err)
		}
	}
}

func TestGetBotStatus_MultiSymbol_PopulatesSymbolsArray(t *testing.T) {
	repo := backtest.NewInMemoryPositionRepo()
	seedOpen(t, repo, "USD_JPY", 1)
	seedOpen(t, repo, "EUR_JPY", 2)

	cfg := &config.BotConfig{Symbols: []string{"USD_JPY", "EUR_JPY"}}
	cfg.Normalize()
	cfg.Bot.Mode = config.ModePaperConfig

	usdActive := &config.StrategyConfig{ConfigID: "u-cfg", Enabled: true}
	usdActive.Strategy.Name = config.StrategyMomentumPullback
	eurActive := &config.StrategyConfig{ConfigID: "e-cfg", Enabled: false}
	eurActive.Strategy.Name = config.StrategyBreakoutFollow

	q := &GetBotStatusQuery{
		BotConfig: cfg,
		Positions: repo,
		GetActiveConfigs: func() map[string]*config.StrategyConfig {
			return map[string]*config.StrategyConfig{
				"USD_JPY": usdActive,
				"EUR_JPY": eurActive,
			}
		},
		EmergencyActive: func() bool { return false },
		StartedAt:       time.Now().Add(-time.Minute),
		Pnl24hJpyFn: func(_ context.Context, sym string) (float64, error) {
			return map[string]float64{"USD_JPY": 1500, "EUR_JPY": -800}[sym], nil
		},
	}
	v, err := q.Execute(context.Background(), GetBotStatusInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(v.Symbols) != 2 {
		t.Fatalf("Symbols len: %d want 2 (%+v)", len(v.Symbols), v.Symbols)
	}
	// Stable order = bot_config.symbols order.
	if v.Symbols[0].Symbol != "USD_JPY" || v.Symbols[1].Symbol != "EUR_JPY" {
		t.Errorf("symbols order: %+v", v.Symbols)
	}
	if v.Symbols[0].OpenPositions != 1 || v.Symbols[1].OpenPositions != 2 {
		t.Errorf("per-symbol OpenPositions: USD=%d EUR=%d (want 1, 2)",
			v.Symbols[0].OpenPositions, v.Symbols[1].OpenPositions)
	}
	if v.Symbols[0].ActiveConfigID != "u-cfg" || v.Symbols[1].ActiveConfigID != "e-cfg" {
		t.Errorf("per-symbol active configs: %+v", v.Symbols)
	}
	if v.Symbols[0].Pnl24hJpy == nil || *v.Symbols[0].Pnl24hJpy != 1500 {
		t.Errorf("USD pnl: %+v", v.Symbols[0].Pnl24hJpy)
	}
	if v.Symbols[1].Pnl24hJpy == nil || *v.Symbols[1].Pnl24hJpy != -800 {
		t.Errorf("EUR pnl: %+v", v.Symbols[1].Pnl24hJpy)
	}

	if v.AccountOpenPositions != 3 {
		t.Errorf("AccountOpenPositions: %d want 3", v.AccountOpenPositions)
	}

	// Legacy top-level fields keep primary-symbol semantics.
	if v.Symbol != "USD_JPY" {
		t.Errorf("legacy Symbol: %q want USD_JPY", v.Symbol)
	}
	if v.OpenPositions != 1 {
		t.Errorf("legacy OpenPositions (primary): %d want 1", v.OpenPositions)
	}
	if v.ActiveConfigID != "u-cfg" {
		t.Errorf("legacy ActiveConfigID (primary): %q want u-cfg", v.ActiveConfigID)
	}
}

func TestGetBotStatus_SingleSymbol_KeepsLegacyShape(t *testing.T) {
	// Regression: 1-symbol setups still produce a Symbols array of length 1
	// AND populate the legacy top-level fields with that symbol's values
	// (= existing dashboard contract).
	repo := backtest.NewInMemoryPositionRepo()
	seedOpen(t, repo, "USD_JPY", 1)
	cfg := &config.BotConfig{Symbol: "USD_JPY"}
	cfg.Normalize()
	cfg.Bot.Mode = config.ModePaperConfig

	active := &config.StrategyConfig{ConfigID: "u-cfg", Enabled: true}
	q := &GetBotStatusQuery{
		BotConfig: cfg,
		Positions: repo,
		GetActiveConfigs: func() map[string]*config.StrategyConfig {
			return map[string]*config.StrategyConfig{"USD_JPY": active}
		},
		EmergencyActive: func() bool { return false },
		StartedAt:       time.Now().Add(-time.Minute),
	}
	v, err := q.Execute(context.Background(), GetBotStatusInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(v.Symbols) != 1 {
		t.Errorf("Symbols len: %d want 1", len(v.Symbols))
	}
	if v.Symbol != "USD_JPY" || v.OpenPositions != 1 || v.ActiveConfigID != "u-cfg" {
		t.Errorf("legacy fields: %+v", v)
	}
	if v.AccountOpenPositions != 1 {
		t.Errorf("AccountOpenPositions (single): %d want 1", v.AccountOpenPositions)
	}
}
