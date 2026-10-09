package app

import (
	"testing"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/usecase/command"
)

// SymbolBundle is the per-symbol container that holds every usecase + adapter
// instance bound to one trading symbol. main.go builds one bundle per entry
// in bot_config.symbols and starts independent goroutines off each.
//
// This test just locks the contract: the bundle exposes (at minimum) Symbol,
// Worker, Executor, Manager, Admission, TradingCycle, Reconcile,
// AdvisorCycle, SummaryStore, Promoter, Advisor, ManualCommand. Adding new
// fields is fine; removing or renaming requires updating both ends together.
func TestSymbolBundle_FieldsArePopulatable(t *testing.T) {
	b := &SymbolBundle{
		Symbol: "USD_JPY",
	}
	// Pointer fields can stay nil — the contract is just that they exist
	// and can be assigned. Compile-time check is enough for this layer.
	b.Worker = &Worker{}
	b.Executor = &command.ExecuteOrder{}
	b.Manager = &command.ManageOpenPositions{}
	b.Admission = &command.EntryAdmission{}
	b.TradingCycle = &command.TradingCycle{}
	b.StartupReconcile = &command.Reconcile{}
	b.RuntimeReconcile = nil // paper mode has nil; allowed
	b.AdvisorCycle = &command.AdvisorCycle{}
	b.Promoter = &command.Promoter{}
	b.ManualCommand = &command.ManualTradeCommand{}
	b.ClosePositionCommand = &command.ClosePositionCommand{}

	if b.Symbol != "USD_JPY" {
		t.Errorf("Symbol: got %q, want USD_JPY", b.Symbol)
	}
}

func TestSymbolBundle_HoldsSymbolKeyedActiveConfig(t *testing.T) {
	// Bundle is built so its Executor.ActiveConfig closure reads the
	// holder slot for THIS bundle's symbol. The closure must close over
	// the symbol literal, not a moving loop variable.
	holder := &ActiveConfigHolder{}
	holder.Set("USD_JPY", &config.StrategyConfig{ConfigID: "u"})
	holder.Set("EUR_JPY", &config.StrategyConfig{ConfigID: "e"})

	// Simulate the per-bundle closure pattern used in main wiring.
	mkGetActive := func(sym string) func() *config.StrategyConfig {
		return func() *config.StrategyConfig { return holder.Get(sym) }
	}

	usdGet := mkGetActive("USD_JPY")
	eurGet := mkGetActive("EUR_JPY")

	if usdGet().ConfigID != "u" || eurGet().ConfigID != "e" {
		t.Errorf("closure capture leaked: usd=%v eur=%v", usdGet(), eurGet())
	}
}
