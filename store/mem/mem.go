package mem

import (
	"bytes"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/polanski13/idemkit"
)

const (
	defaultTTL         = 24 * time.Hour
	defaultLockTimeout = 30 * time.Second
)

type Config struct {
	TTL         time.Duration
	LockTimeout time.Duration
	Clock       func() time.Time
}

type Store struct {
	cfg     Config
	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	state     idemkit.State
	bodyHash  []byte
	result    *idemkit.Result
	expiresAt time.Time
	waiters   chan struct{}
}

func New(cfg Config) *Store {
	if cfg.TTL <= 0 {
		cfg.TTL = defaultTTL
	}
	if cfg.LockTimeout <= 0 {
		cfg.LockTimeout = defaultLockTimeout
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &Store{cfg: cfg, entries: make(map[string]*entry)}
}

func (s *Store) Begin(ctx context.Context, key string, bodyHash []byte) (idemkit.State, *idemkit.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.cfg.Clock()
	e, ok := s.lookupLocked(key, now)
	if !ok {
		s.entries[key] = &entry{
			state:     idemkit.StateInFlight,
			bodyHash:  slices.Clone(bodyHash),
			expiresAt: now.Add(s.cfg.LockTimeout),
			waiters:   make(chan struct{}),
		}
		return idemkit.StateFresh, nil, nil
	}

	var mismatch error
	if !bytes.Equal(e.bodyHash, bodyHash) {
		mismatch = idemkit.ErrBodyMismatch
	}

	if e.state == idemkit.StateInFlight {
		return idemkit.StateInFlight, nil, mismatch
	}
	return idemkit.StateDone, e.result, mismatch
}

func (s *Store) Wait(ctx context.Context, key string) (*idemkit.Result, error) {
	s.mu.Lock()
	e, ok := s.lookupLocked(key, s.cfg.Clock())
	if !ok {
		s.mu.Unlock()
		return nil, nil
	}
	if e.state == idemkit.StateDone {
		res := e.result
		s.mu.Unlock()
		return res, nil
	}
	notify := e.waiters
	s.mu.Unlock()

	select {
	case <-notify:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok = s.lookupLocked(key, s.cfg.Clock())
	if !ok {
		return nil, nil
	}
	if e.state == idemkit.StateDone {
		return e.result, nil
	}
	return nil, nil
}

func (s *Store) Save(ctx context.Context, key string, result *idemkit.Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok {
		return nil
	}
	e.state = idemkit.StateDone
	e.result = result
	e.expiresAt = s.cfg.Clock().Add(s.cfg.TTL)
	if e.waiters != nil {
		close(e.waiters)
		e.waiters = nil
	}
	return nil
}

func (s *Store) Release(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok {
		return nil
	}
	if e.waiters != nil {
		close(e.waiters)
	}
	delete(s.entries, key)
	return nil
}

func (s *Store) lookupLocked(key string, now time.Time) (*entry, bool) {
	e, ok := s.entries[key]
	if !ok {
		return nil, false
	}
	if now.After(e.expiresAt) {
		if e.waiters != nil {
			close(e.waiters)
		}
		delete(s.entries, key)
		return nil, false
	}
	return e, true
}
