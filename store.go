package idemkit

import "context"

type Store interface {
	Begin(ctx context.Context, key string, bodyHash []byte) (State, *Result, error)
	Wait(ctx context.Context, key string) (*Result, error)
	Save(ctx context.Context, key string, result *Result) error
	Release(ctx context.Context, key string) error
}
