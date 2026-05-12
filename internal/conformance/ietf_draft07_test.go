package conformance

import (
	"net/http"
	"testing"

	"github.com/polanski13/idemkit"
)

func TestIETFDraft07_FirstRequestRunsHandlerAndDoesNotSetReplayHeader(t *testing.T) {
	mw := newMiddleware(t, idemkit.ConflictIETF)
	rec := doRequest(t, mw, `{"amount":100}`, "k1")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: %d, want 201", rec.Code)
	}
	if got := rec.Header().Get("X-Idemkit-Replayed"); got != "" {
		t.Fatalf("X-Idemkit-Replayed on first request: %q, want empty", got)
	}
}

func TestIETFDraft07_SameKeySameBodyReplaysCachedResponse(t *testing.T) {
	mw := newMiddleware(t, idemkit.ConflictIETF)
	first := doRequest(t, mw, `{"amount":100}`, "k1")
	second := doRequest(t, mw, `{"amount":100}`, "k1")

	if first.Code != second.Code {
		t.Fatalf("status drift across replay: first=%d second=%d", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("body drift across replay: first=%q second=%q", first.Body.String(), second.Body.String())
	}
}

func TestIETFDraft07_ReplaySetsReplayHeader(t *testing.T) {
	mw := newMiddleware(t, idemkit.ConflictIETF)
	doRequest(t, mw, `{"amount":100}`, "k1")
	rec := doRequest(t, mw, `{"amount":100}`, "k1")

	if got := rec.Header().Get("X-Idemkit-Replayed"); got != "true" {
		t.Fatalf("X-Idemkit-Replayed on replay: %q, want \"true\"", got)
	}
}

func TestIETFDraft07_SameKeyDifferentBodyReturns409(t *testing.T) {
	mw := newMiddleware(t, idemkit.ConflictIETF)
	doRequest(t, mw, `{"amount":100}`, "k1")
	rec := doRequest(t, mw, `{"amount":999}`, "k1")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status: %d, want 409 Conflict (draft-07 §2.6)", rec.Code)
	}
}

func TestIETFDraft07_DifferentKeysAreIndependent(t *testing.T) {
	mw := newMiddleware(t, idemkit.ConflictIETF)
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

func TestIETFDraft07_NoKeyPassesThroughToHandler(t *testing.T) {
	mw := newMiddleware(t, idemkit.ConflictIETF)
	rec := doRequest(t, mw, `{"amount":100}`, "")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: %d, want 201 (no key = pass through to handler)", rec.Code)
	}
}
