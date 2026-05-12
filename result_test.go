package idemkit

import (
	"net/http"
	"testing"
)

func TestResult_CloneNilReturnsNil(t *testing.T) {
	var r *Result
	if got := r.Clone(); got != nil {
		t.Fatalf("nil.Clone() = %v, want nil", got)
	}
}

func TestResult_CloneCopiesScalarFields(t *testing.T) {
	src := &Result{StatusCode: 201}
	dst := src.Clone()
	if dst == src {
		t.Fatal("Clone returned same pointer")
	}
	if dst.StatusCode != 201 {
		t.Fatalf("StatusCode: %d", dst.StatusCode)
	}
}

func TestResult_CloneCopiesHeaderDeeply(t *testing.T) {
	src := &Result{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": {"application/json"},
			"X-Multi":      {"a", "b"},
		},
	}
	dst := src.Clone()

	dst.Header.Set("Content-Type", "text/plain")
	if got := src.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("clone mutated source: src Content-Type = %q", got)
	}

	dst.Header["X-Multi"][0] = "zzz"
	if got := src.Header["X-Multi"][0]; got != "a" {
		t.Fatalf("clone shares value slice: src X-Multi[0] = %q", got)
	}
}

func TestResult_CloneCopiesBodyDeeply(t *testing.T) {
	src := &Result{
		StatusCode: 200,
		Body:       []byte("hello"),
	}
	dst := src.Clone()

	dst.Body[0] = 'X'
	if string(src.Body) != "hello" {
		t.Fatalf("clone shares body slice: src now %q", src.Body)
	}
}

func TestResult_CloneNilFieldsRemainNil(t *testing.T) {
	src := &Result{StatusCode: 204}
	dst := src.Clone()
	if dst.Header != nil {
		t.Fatalf("Header: %v, want nil", dst.Header)
	}
	if dst.Body != nil {
		t.Fatalf("Body: %v, want nil", dst.Body)
	}
}

func TestResult_CloneEmptySlicesPreserved(t *testing.T) {
	src := &Result{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       []byte{},
	}
	dst := src.Clone()
	if dst.Header == nil {
		t.Fatal("empty Header became nil after Clone")
	}
	if dst.Body == nil {
		t.Fatal("empty Body became nil after Clone")
	}
}
