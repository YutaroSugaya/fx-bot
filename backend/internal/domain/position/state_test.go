package position

import "testing"

// TestState_String checks the wire representation of each state.
// The string must match the CHECK constraint values on
// positions.status / position_state_events.state.
func TestState_String(t *testing.T) {
	cases := []struct {
		state State
		want  string
	}{
		{OpenState{}, "OPEN"},
		{ClosingState{}, "CLOSING"},
		{ClosedState{}, "CLOSED"},
		{UnknownState{}, "UNKNOWN"},
	}
	for _, c := range cases {
		if got := c.state.String(); got != c.want {
			t.Errorf("State.String() = %q, want %q", got, c.want)
		}
	}
}

// TestState_CanTransitionTo enumerates every (from, to) pair and asserts
// the allowed transitions. The state machine is:
//
//	OPEN    -> CLOSING, UNKNOWN
//	CLOSING -> CLOSED, UNKNOWN
//	CLOSED  -> (terminal — no transitions)
//	UNKNOWN -> CLOSED   (operator-resolved)
//
// The matrix is exhaustive on purpose: any addition must be reflected in
// both code and DB CHECK to keep the two layers in sync.
func TestState_CanTransitionTo(t *testing.T) {
	all := []State{OpenState{}, ClosingState{}, ClosedState{}, UnknownState{}}
	allowed := map[string]map[string]bool{
		"OPEN":    {"CLOSING": true, "UNKNOWN": true},
		"CLOSING": {"CLOSED": true, "UNKNOWN": true},
		"CLOSED":  {}, // terminal
		"UNKNOWN": {"CLOSED": true},
	}
	for _, from := range all {
		for _, to := range all {
			want := allowed[from.String()][to.String()]
			got := from.CanTransitionTo(to)
			if got != want {
				t.Errorf("(%s).CanTransitionTo(%s) = %v, want %v",
					from.String(), to.String(), got, want)
			}
		}
	}
}

// TestParseState exercises the string -> State lookup for hydrating
// `positions.status` text into a domain value.
func TestParseState(t *testing.T) {
	cases := []struct {
		in      string
		want    State
		wantErr bool
	}{
		{"OPEN", OpenState{}, false},
		{"CLOSING", ClosingState{}, false},
		{"CLOSED", ClosedState{}, false},
		{"UNKNOWN", UnknownState{}, false},
		{"", nil, true},
		{"open", nil, true}, // case-sensitive
		{"DELETED", nil, true},
	}
	for _, c := range cases {
		got, err := ParseState(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseState(%q) expected error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseState(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got.String() != c.want.String() {
			t.Errorf("ParseState(%q) = %s, want %s", c.in, got.String(), c.want.String())
		}
	}
}
