package idemkit

import "fmt"

type State int

const (
	StateFresh State = iota
	StateInFlight
	StateDone
)

func (s State) String() string {
	switch s {
	case StateFresh:
		return "fresh"
	case StateInFlight:
		return "in_flight"
	case StateDone:
		return "done"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}
