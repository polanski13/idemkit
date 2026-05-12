package idemkit

import "testing"

func TestToken_ZeroValueIsInvalid(t *testing.T) {
	var zero Token
	if zero != 0 {
		t.Fatalf("Token zero value: %d, want 0", zero)
	}
}

func TestToken_DistinctValuesAreNotEqual(t *testing.T) {
	a := Token(1)
	b := Token(2)
	if a == b {
		t.Fatalf("Token(1) == Token(2)")
	}
}
