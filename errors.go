package idemkit

import "errors"

var (
	ErrBodyMismatch  = errors.New("idemkit: body hash mismatch")
	ErrTokenMismatch = errors.New("idemkit: claim token mismatch")
)
