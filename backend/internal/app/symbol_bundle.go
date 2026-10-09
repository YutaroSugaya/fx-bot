package app

import (
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/usecase/command"
)

// SymbolBundle groups every per-symbol usecase + adapter instance that drives
// one trading symbol. main.go builds one bundle per entry in
// bot_config.symbols and starts the worker / reconcile loops off each.
//
// Per-symbol isolation rule: every field that holds symbol-tagged state
// (Executor, Manager, Admission, TradingCycle, Reconcile, Advisor, Promoter,
// SummaryStore, AdvisorCycle, Worker, ManualCommand, ClosePositionCommand)
// MUST be a fresh instance per bundle. The only objects shared across
// bundles are:
//   - the *ActiveConfigHolder (slot-keyed, race-safe)
//   - the global entry mutex (used by EntryAdmission.Mutex AND
//     ExecuteOrder.EntryMutex to serialize all entries account-wide)
//   - read-only configs (HardLimits, BotConfig, etc.)
type SymbolBundle struct {
	Symbol       string
	Aggregator   *market.Aggregator
	Worker       *Worker
	Executor     *command.ExecuteOrder
	Manager      *command.ManageOpenPositions
	Admission    *command.EntryAdmission
	TradingCycle *command.TradingCycle
	// StartupReconcile runs once during boot. Required for every mode so a
	// crash-recovery / drifted-DB picture is detected before entering loops.
	StartupReconcile *command.Reconcile
	// RuntimeReconcile is the periodic Live-only reconciler. nil in
	// paper / disabled modes.
	RuntimeReconcile     *command.Reconcile
	Advisor              port.Advisor
	Promoter             *command.Promoter
	SummaryStore         port.MarketSummaryArtifactStore
	AdvisorCycle         *command.AdvisorCycle
	ManualCommand        *command.ManualTradeCommand
	ClosePositionCommand *command.ClosePositionCommand
	// SignatureCycle is the advisor v2 (signature-breakout) periodic cycle. NIL when advisor v2 is
	// disabled OR the symbol is not in advisor_v2.symbols (allowlist) — so every
	// reader MUST nil-check (see runSignatureV2Scheduler's `if b.SignatureCycle == nil { continue }`).
	// Only run when advisor_v2.enabled (default off).
	SignatureCycle *command.SignatureCycle
	// LLMDecisionCycle is the autonomous LLM trade loop's per-symbol cycle. NIL
	// when llm_decision is disabled OR the symbol is not in its allowlist — readers MUST nil-check.
	LLMDecisionCycle *command.LLMDecisionCycle
	// ReflectionCycle is the autonomous learning (Reflexion) loop that rewrites the playbook. NIL when
	// llm_decision is disabled for the symbol. Lower cadence than the decision cycle. Readers nil-check.
	ReflectionCycle *command.ReflectionCycle
}
