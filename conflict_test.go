package idemkit

import "testing"

func TestConflictMode_String(t *testing.T) {
	cases := []struct {
		mode ConflictMode
		want string
	}{
		{ConflictStripe, "stripe"},
		{ConflictIETF, "ietf"},
		{ConflictMode(99), "ConflictMode(99)"},
		{ConflictMode(-1), "ConflictMode(-1)"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			if got := c.mode.String(); got != c.want {
				t.Fatalf("ConflictMode(%d).String() = %q, want %q", c.mode, got, c.want)
			}
		})
	}
}

func TestConflictMode_StripeIsZeroValue(t *testing.T) {
	var zero ConflictMode
	if zero != ConflictStripe {
		t.Fatalf("zero-value ConflictMode = %v, want ConflictStripe (default per plan)", zero)
	}
}

func TestConflictReason_String(t *testing.T) {
	cases := []struct {
		reason ConflictReason
		want   string
	}{
		{ReasonBodyMismatch, "body_mismatch"},
		{ConflictReason(99), "ConflictReason(99)"},
		{ConflictReason(-1), "ConflictReason(-1)"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			if got := c.reason.String(); got != c.want {
				t.Fatalf("ConflictReason(%d).String() = %q, want %q", c.reason, got, c.want)
			}
		})
	}
}
