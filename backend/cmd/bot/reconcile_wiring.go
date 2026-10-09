package main

import (
	"log/slog"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/usecase/command"
)

// reconcileWiringDeps groups the inputs needed to build a *command.Reconcile
// at startup or runtime. Centralising the construction here gives us a small
// surface that reconcile_wiring_test.go can exercise without spinning up the
// full main() pipeline — the regression we want to prevent is a
// "Closer not wired" bug (Closer == nil makes
// stale_db_position trip emergency_stop instead of resolving via TP/SL fills
// in Live and instead of synthetic-closing in Paper).
type reconcileWiringDeps struct {
	Broker            port.Broker
	Repos             *port.Repositories
	Notifier          port.Notifier
	Symbol            string
	Logger            *slog.Logger
	EmergencyFlagPath string
	LiveMode          config.Mode
	// PendingTracker は entry saga 進行中の broker_position_id を共有する。
	// reconcile が ExecuteOrder / ManualTradeCommand と同一 instance を見て、
	// race-window で誤検出した naked broker を skip する。
	PendingTracker port.PendingPositionTracker
	// Counters surfaces reconcile-detected naked_broker_position on /api/status.
	// Typed as the command-side interface so this file need not import app.
	Counters command.OpsCounters
	// OnPositionClosed (event_retrigger): fired after reconcile settles a
	// close (broker OCO fill / estimated close). Wired to the RUNTIME reconciler only —
	// startup reconcile is boot-time cleanup and must not trigger an LLM cycle
	// (the first scheduled cycle runs ~90s after boot anyway). nil = no-op.
	OnPositionClosed func(symbol, reason string)
}

// buildStartupReconcile assembles the *command.Reconcile used during boot.
//
// Invariants enforced here (not optional — covered by reconcile_wiring_test.go):
//   - Closer MUST be wired so Live stale_db_position can resolve via the
//     recorded TP/SL settle leg executions and Paper stale_db_position can
//     CloseAndRecord a synthetic close.
//   - StrategyConfigs MUST be wired so Paper naked_broker_position adoption
//     can FK to the active strategy config row.
func buildStartupReconcile(deps reconcileWiringDeps) *command.Reconcile {
	return &command.Reconcile{
		Broker:            deps.Broker,
		Positions:         deps.Repos.Positions,
		StrategyConfigs:   deps.Repos.StrategyConfigs,
		Closer:            deps.Repos.Closer,
		Notifier:          deps.Notifier,
		Symbol:            deps.Symbol,
		Logger:            deps.Logger,
		EmergencyFlagPath: deps.EmergencyFlagPath,
		Mode:              command.ReconcileModeStartup,
		LiveMode:          deps.LiveMode,
		PendingTracker:    deps.PendingTracker,
		Counters:          deps.Counters,
	}
}

// buildRuntimeReconcile assembles the *command.Reconcile used by the Live
// periodic reconcile loop. Closer is mandatory: runtime mode's only fix
// path for stale_db_position is fill resolution via Closer.CloseAndRecord.
//
// StrategyConfigs is ALSO mandatory here: the runtime
// loop is the ONLY periodic reconciler, so a mid-run external position (user
// opens a trade in the GMO app while the bot runs) is adopted here. Without
// StrategyConfigs the active config_id can't be resolved → adoption always
// fails → the position is deferred the full ExternalAdoptGrace window then
// spuriously trips emergency_stop (the exact halt the grace fix prevents).
func buildRuntimeReconcile(deps reconcileWiringDeps) *command.Reconcile {
	return &command.Reconcile{
		Broker:              deps.Broker,
		Positions:           deps.Repos.Positions,
		StrategyConfigs:     deps.Repos.StrategyConfigs,
		Closer:              deps.Repos.Closer,
		Notifier:            deps.Notifier,
		Symbol:              deps.Symbol,
		Logger:              deps.Logger,
		EmergencyFlagPath:   deps.EmergencyFlagPath,
		Mode:                command.ReconcileModeRuntime,
		LiveMode:            deps.LiveMode,
		PendingTracker:      deps.PendingTracker,
		StaleGracePeriod:    LiveRuntimeStaleGracePeriod,
		StaleHardTripPeriod: LiveRuntimeStaleHardTripPeriod,
		ExternalAdoptGrace:  LiveRuntimeExternalAdoptGrace, // defer transient external positions; trip only if persistent
		Counters:            deps.Counters,
		OnPositionClosed:    deps.OnPositionClosed,
	}
}
