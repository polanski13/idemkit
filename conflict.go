package idemkit

import "fmt"

type ConflictMode int

const (
	ConflictStripe ConflictMode = iota
	ConflictIETF
)

func (c ConflictMode) String() string {
	switch c {
	case ConflictStripe:
		return "stripe"
	case ConflictIETF:
		return "ietf"
	default:
		return fmt.Sprintf("ConflictMode(%d)", int(c))
	}
}

type ConflictReason int

const (
	ReasonBodyMismatch ConflictReason = iota
)

func (r ConflictReason) String() string {
	switch r {
	case ReasonBodyMismatch:
		return "body_mismatch"
	default:
		return fmt.Sprintf("ConflictReason(%d)", int(r))
	}
}
