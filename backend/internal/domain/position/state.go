package position

import "fmt"

// State is the position lifecycle status, modelled as the classic
// state-pattern interface. The four concrete states are 1:1 with the
// values allowed by the `positions.status` and `position_state_events.state`
// CHECK constraints.
//
// Transition discipline:
//
//	OPEN    -> CLOSING (claim-for-close), UNKNOWN (broker out of sync)
//	CLOSING -> CLOSED  (saga success),    UNKNOWN (saga gave up)
//	CLOSED  -> (terminal — append-only ledger entry)
//	UNKNOWN -> CLOSED  (operator-resolved, fill discovered)
//
// The state machine is enforced in two layers: DB-side via
// position_state_events.PK (position_id, state) plus a CHECK on state, and
// app-side via the repository's status CAS (e.g. ClaimForClose). CanTransitionTo
// states the same matrix in the domain; repositories do not call it today.
// The two layers are not redundant — the DB protects against bypass
// (raw SQL, manual ops) and the app protects against bad call-site logic.
type State interface {
	String() string
	CanTransitionTo(next State) bool
}

// OpenState is the initial state after a fill has been recorded. The
// position is at-risk: TP/SL legs are placed at the broker (Live) or
// monitored by the bot (Paper).
type OpenState struct{}

// ClosingState marks the start of a close saga. Set by the in-Tx CAS in
// PositionRepository.ClaimForClose BEFORE any broker call so a stale
// concurrent OPEN snapshot cannot drive a second close.
type ClosingState struct{}

// ClosedState is terminal. The trade row exists, PnL is finalised, and
// the position takes no further part in reconcile.
type ClosedState struct{}

// UnknownState means the DB and broker disagree on whether the position
// still exists. Reserved: the CHECK constraints allow it but no code path
// writes it today. Operator intervention required.
type UnknownState struct{}

func (OpenState) String() string    { return "OPEN" }
func (ClosingState) String() string { return "CLOSING" }
func (ClosedState) String() string  { return "CLOSED" }
func (UnknownState) String() string { return "UNKNOWN" }

func (OpenState) CanTransitionTo(next State) bool {
	switch next.(type) {
	case ClosingState, UnknownState:
		return true
	}
	return false
}

func (ClosingState) CanTransitionTo(next State) bool {
	switch next.(type) {
	case ClosedState, UnknownState:
		return true
	}
	return false
}

func (ClosedState) CanTransitionTo(_ State) bool { return false }

func (UnknownState) CanTransitionTo(next State) bool {
	_, ok := next.(ClosedState)
	return ok
}

// ParseState hydrates the wire string into a domain State. Returns an
// error on unrecognised input so a caller hydrating positions.status can
// fail loudly instead of silently inserting an UnknownState.
func ParseState(s string) (State, error) {
	switch s {
	case "OPEN":
		return OpenState{}, nil
	case "CLOSING":
		return ClosingState{}, nil
	case "CLOSED":
		return ClosedState{}, nil
	case "UNKNOWN":
		return UnknownState{}, nil
	}
	return nil, fmt.Errorf("position: unknown state %q", s)
}
