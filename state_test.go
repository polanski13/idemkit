package idemkit

import "testing"

func TestState_String(t *testing.T) {
	cases := []struct {
		state State
		want  string
	}{
		{StateFresh, "fresh"},
		{StateInFlight, "in_flight"},
		{StateDone, "done"},
		{State(99), "State(99)"},
		{State(-1), "State(-1)"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			if got := c.state.String(); got != c.want {
				t.Fatalf("State(%d).String() = %q, want %q", c.state, got, c.want)
			}
		})
	}
}

func TestState_DistinctValues(t *testing.T) {
	values := []State{StateFresh, StateInFlight, StateDone}
	seen := make(map[State]bool, len(values))
	for _, v := range values {
		if seen[v] {
			t.Fatalf("duplicate State value: %d", v)
		}
		seen[v] = true
	}
}
