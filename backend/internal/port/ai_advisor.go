package port

import (
	"context"
	"time"

	"fx-bot/backend/internal/domain/market"
)

// AdvisorRunStatusType mirrors the DB enum but as a runtime concept; the
// repository layer already has its own enum (AdvisorRunStatus). We keep a
// thin alias here so the usecase code reads symmetrically.
type AdvisorRunStatusType = AdvisorRunStatus

// AdvisorRun captures one invocation of the AI advisor and its outcome.
//
// ParsedYAML is the trimmed, fence-stripped YAML that the validator + parser
// should consume next. If Status != Success, ParsedYAML may be empty.
type AdvisorRun struct {
	RunID      string
	StartedAt  time.Time
	FinishedAt time.Time
	InputJSON  []byte
	OutputYAML []byte // raw stdout (for audit)
	ParsedYAML []byte // stripped of ``` fences, ready for yaml.Unmarshal
	Status     AdvisorRunStatusType
	ErrorMsg   string

	// UsageLimited is true when the failure was Claude's usage/session limit
	// (not a transient infra blip). It is NOT persisted to the DB — Status
	// stays cli_error — but the scheduler reads it to AVOID the fast-retry
	// cadence: a usage limit resets hours later, so hammering the CLI every
	// 10 min just burns the (already exhausted) quota: a session limit would
	// otherwise drive a 10-min fast-retry loop that fires repeatedly across the
	// reset window, all mislabeled "event" (see ParseErrorMonitor).
	UsageLimited bool
}

// Advisor produces a next-window strategy config given a MarketSummary.
// The concrete implementation lives in internal/adapter/advisor/ (Claude CLI
// today). The interface stays narrow on purpose — we don't expose the
// subprocess details to the usecase layer.
type Advisor interface {
	Generate(ctx context.Context, summary *market.MarketSummary) (*AdvisorRun, error)
}
