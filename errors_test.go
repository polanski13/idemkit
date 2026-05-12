package idemkit

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestErrBodyMismatch_IdentityUnderIs(t *testing.T) {
	if !errors.Is(ErrBodyMismatch, ErrBodyMismatch) {
		t.Fatal("sentinel does not match itself via errors.Is")
	}
}

func TestErrBodyMismatch_MatchesWhenWrapped(t *testing.T) {
	wrapped := fmt.Errorf("store layer: %w", ErrBodyMismatch)
	if !errors.Is(wrapped, ErrBodyMismatch) {
		t.Fatal("wrapped error does not match ErrBodyMismatch via errors.Is")
	}
}

func TestErrBodyMismatch_DoesNotMatchUnrelated(t *testing.T) {
	other := errors.New("different error")
	if errors.Is(other, ErrBodyMismatch) {
		t.Fatal("unrelated error spuriously matches ErrBodyMismatch")
	}
}

func TestErrBodyMismatch_MessageIsInformative(t *testing.T) {
	msg := ErrBodyMismatch.Error()
	if msg == "" {
		t.Fatal("error message is empty")
	}
	if !strings.Contains(msg, "idemkit") {
		t.Fatalf("error message %q does not identify the package", msg)
	}
}

func TestErrTokenMismatch_IdentityUnderIs(t *testing.T) {
	if !errors.Is(ErrTokenMismatch, ErrTokenMismatch) {
		t.Fatal("sentinel does not match itself via errors.Is")
	}
}

func TestErrTokenMismatch_MatchesWhenWrapped(t *testing.T) {
	wrapped := fmt.Errorf("store layer: %w", ErrTokenMismatch)
	if !errors.Is(wrapped, ErrTokenMismatch) {
		t.Fatal("wrapped error does not match ErrTokenMismatch via errors.Is")
	}
}

func TestErrTokenMismatch_DistinctFromBodyMismatch(t *testing.T) {
	if errors.Is(ErrTokenMismatch, ErrBodyMismatch) {
		t.Fatal("ErrTokenMismatch should not match ErrBodyMismatch")
	}
	if errors.Is(ErrBodyMismatch, ErrTokenMismatch) {
		t.Fatal("ErrBodyMismatch should not match ErrTokenMismatch")
	}
}

func TestErrTokenMismatch_MessageIsInformative(t *testing.T) {
	msg := ErrTokenMismatch.Error()
	if msg == "" {
		t.Fatal("error message is empty")
	}
	if !strings.Contains(msg, "idemkit") {
		t.Fatalf("error message %q does not identify the package", msg)
	}
}
