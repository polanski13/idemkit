package idemkit

import (
	"context"
	"testing"
)

type stubStore struct {
	beginState  State
	beginResult *Result
	beginToken  Token
	beginErr    error
	waitResult  *Result
	waitErr     error
	saveErr     error
	releaseErr  error
}

func (s stubStore) Begin(context.Context, string, []byte) (State, *Result, Token, error) {
	return s.beginState, s.beginResult, s.beginToken, s.beginErr
}

func (s stubStore) Wait(context.Context, string) (*Result, error) {
	return s.waitResult, s.waitErr
}

func (s stubStore) Save(context.Context, string, Token, *Result) error {
	return s.saveErr
}

func (s stubStore) Release(context.Context, string, Token) error {
	return s.releaseErr
}

var _ Store = stubStore{}

func TestStore_StubExercisesAllMethods(t *testing.T) {
	ctx := context.Background()
	cached := &Result{StatusCode: 200, Body: []byte("ok")}
	s := stubStore{
		beginState:  StateDone,
		beginResult: cached,
		waitResult:  cached,
	}
	var iface Store = s

	state, res, _, err := iface.Begin(ctx, "k", []byte{1, 2, 3})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if state != StateDone {
		t.Fatalf("Begin state: got %v want StateDone", state)
	}
	if res != cached {
		t.Fatalf("Begin result: got %v want cached", res)
	}

	got, err := iface.Wait(ctx, "k")
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got != cached {
		t.Fatalf("Wait result: got %v want cached", got)
	}

	if err := iface.Save(ctx, "k", Token(1), cached); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := iface.Release(ctx, "k", Token(1)); err != nil {
		t.Fatalf("Release: %v", err)
	}
}
