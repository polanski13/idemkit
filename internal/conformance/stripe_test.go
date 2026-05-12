package conformance

import (
	"net/http"
	"testing"

	"github.com/polanski13/idemkit"
)

func TestStripe_FirstRequestRunsHandlerAndDoesNotSetReplayHeader(t *testing.T) {
	mw := newMiddleware(t, idemkit.ConflictStripe)
	rec := doRequest(t, mw, `{"amount":100}`, "k1")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: %d, want 201", rec.Code)
	}
	if got := rec.Header().Get("X-Idemkit-Replayed"); got != "" {
		t.Fatalf("X-Idemkit-Replayed on first request: %q, want empty", got)
	}
}

func TestStripe_SameKeySameBodyReplaysCachedResponse(t *testing.T) {
	mw := newMiddleware(t, idemkit.ConflictStripe)
	first := doRequest(t, mw, `{"amount":100}`, "k1")
	second := doRequest(t, mw, `{"amount":100}`, "k1")

	if first.Code != second.Code {
		t.Fatalf("status drift across replay: first=%d second=%d", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("body drift across replay: first=%q second=%q", first.Body.String(), second.Body.String())
	}
	if first.Header().Get("Content-Type") != second.Header().Get("Content-Type") {
		t.Fatal("Content-Type drift across replay")
	}
}

func TestStripe_ReplaySetsReplayHeader(t *testing.T) {
	mw := newMiddleware(t, idemkit.ConflictStripe)
	doRequest(t, mw, `{"amount":100}`, "k1")
	rec := doRequest(t, mw, `{"amount":100}`, "k1")

	if got := rec.Header().Get("X-Idemkit-Replayed"); got != "true" {
		t.Fatalf("X-Idemkit-Replayed on replay: %q, want \"true\"", got)
	}
}

func TestStripe_SameKeyDifferentBodyReturns422(t *testing.T) {
	mw := newMiddleware(t, idemkit.ConflictStripe)
	doRequest(t, mw, `{"amount":100}`, "k1")
	rec := doRequest(t, mw, `{"amount":999}`, "k1")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: %d, want 422 Unprocessable Entity (Stripe convention)", rec.Code)
	}
}

func TestStripe_DifferentKeysAreIndependent(t *testing.T) {
	mw := newMiddleware(t, idemkit.ConflictStripe)
	a := doRequest(t, mw, `{"amount":100}`, "k1")
	b := doRequest(t, mw, `{"amount":200}`, "k2")

	if a.Code != http.StatusCreated || b.Code != http.StatusCreated {
		t.Fatalf("both should be 201; got a=%d b=%d", a.Code, b.Code)
	}
	if a.Header().Get("X-Idemkit-Replayed") != "" {
		t.Fatal("a should not be replayed")
	}
	if b.Header().Get("X-Idemkit-Replayed") != "" {
		t.Fatal("b should not be replayed")
	}
}
