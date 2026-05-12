package pg

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/polanski13/idemkit"
)

const (
	defaultTTL          = 24 * time.Hour
	defaultLockTimeout  = 30 * time.Second
	defaultPollInterval = 100 * time.Millisecond
	notifyChannel       = "idemkit_notify"

	stateInFlight int16 = 0
	stateDone     int16 = 1
)

//go:embed schema.sql
var schemaSQL string

type Config struct {
	TTL          time.Duration
	LockTimeout  time.Duration
	PollInterval time.Duration
	ListenConn   *pgx.Conn
}

type Store struct {
	pool      *pgxpool.Pool
	cfg       Config
	listener  *listener
	closeOnce sync.Once
}

func New(pool *pgxpool.Pool, cfg Config) *Store {
	if cfg.TTL <= 0 {
		cfg.TTL = defaultTTL
	}
	if cfg.LockTimeout <= 0 {
		cfg.LockTimeout = defaultLockTimeout
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	s := &Store{pool: pool, cfg: cfg}
	if cfg.ListenConn != nil {
		startCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		l, err := newListener(startCtx, cfg.ListenConn, notifyChannel)
		if err == nil {
			s.listener = l
		}
	}
	return s
}

func (s *Store) Close() error {
	if s.listener == nil {
		return nil
	}
	var err error
	s.closeOnce.Do(func() {
		err = s.listener.close()
	})
	return err
}

func (s *Store) notifyArg() string {
	if s.listener == nil {
		return ""
	}
	return notifyChannel
}

func ApplySchema(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("idemkit/pg: apply schema: %w", err)
	}
	return nil
}

const insertSQL = `
INSERT INTO idemkit_keys (key, body_hash, state, locked_at, expires_at, token)
VALUES ($1, $2, $3, NOW(), NOW() + ($4 * INTERVAL '1 millisecond'), nextval('idemkit_token_seq'))
ON CONFLICT DO NOTHING
RETURNING token
`

const selectForUpdateSQL = `
SELECT body_hash, state, response_code, response_headers, response_body, expires_at
FROM idemkit_keys
WHERE key = $1
FOR UPDATE
`

const reclaimSQL = `
UPDATE idemkit_keys
SET state = $1,
    body_hash = $2,
    locked_at = NOW(),
    expires_at = NOW() + ($3 * INTERVAL '1 millisecond'),
    token = nextval('idemkit_token_seq'),
    response_code = NULL,
    response_headers = NULL,
    response_body = NULL,
    completed_at = NULL
WHERE key = $4
RETURNING token
`

const saveSQL = `
WITH upd AS (
    UPDATE idemkit_keys
    SET state = $1,
        response_code = $2,
        response_headers = $3,
        response_body = $4,
        completed_at = NOW(),
        expires_at = NOW() + ($5 * INTERVAL '1 millisecond')
    WHERE key = $6 AND token = $7
    RETURNING 1
)
SELECT CASE WHEN $8 != '' THEN pg_notify($8, $6) END FROM upd
`

const releaseSQL = `
WITH del AS (
    DELETE FROM idemkit_keys WHERE key = $1 AND token = $2 RETURNING 1
)
SELECT CASE WHEN $3 != '' THEN pg_notify($3, $1) END FROM del
`

const probeSQL = `
SELECT state, response_code, response_headers, response_body
FROM idemkit_keys
WHERE key = $1 AND expires_at > NOW()
`

func (s *Store) Begin(ctx context.Context, key string, bodyHash []byte) (idemkit.State, *idemkit.Result, idemkit.Token, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, nil, 0, fmt.Errorf("idemkit/pg: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	lockMillis := s.cfg.LockTimeout.Milliseconds()
	if lockMillis <= 0 {
		lockMillis = 1
	}

	var token int64
	err = tx.QueryRow(ctx, insertSQL, key, bodyHash, stateInFlight, lockMillis).Scan(&token)
	if err == nil {
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return 0, nil, 0, fmt.Errorf("idemkit/pg: commit fresh claim: %w", commitErr)
		}
		return idemkit.StateFresh, nil, idemkit.Token(token), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, 0, fmt.Errorf("idemkit/pg: insert: %w", err)
	}

	var (
		storedHash  []byte
		state       int16
		respCode    *int32
		respHeaders []byte
		respBody    []byte
		expiresAt   time.Time
	)
	err = tx.QueryRow(ctx, selectForUpdateSQL, key).Scan(&storedHash, &state, &respCode, &respHeaders, &respBody, &expiresAt)
	if err != nil {
		return 0, nil, 0, fmt.Errorf("idemkit/pg: select existing: %w", err)
	}

	if time.Now().After(expiresAt) {
		var newToken int64
		err = tx.QueryRow(ctx, reclaimSQL, stateInFlight, bodyHash, lockMillis, key).Scan(&newToken)
		if err != nil {
			return 0, nil, 0, fmt.Errorf("idemkit/pg: reclaim: %w", err)
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return 0, nil, 0, fmt.Errorf("idemkit/pg: commit reclaim: %w", commitErr)
		}
		return idemkit.StateFresh, nil, idemkit.Token(newToken), nil
	}

	var mismatch error
	if !bytes.Equal(storedHash, bodyHash) {
		mismatch = idemkit.ErrBodyMismatch
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		return 0, nil, 0, fmt.Errorf("idemkit/pg: commit existing: %w", commitErr)
	}

	if state == stateInFlight {
		return idemkit.StateInFlight, nil, 0, mismatch
	}

	result, err := decodeResult(respCode, respHeaders, respBody)
	if err != nil {
		return 0, nil, 0, fmt.Errorf("idemkit/pg: decode result: %w", err)
	}
	return idemkit.StateDone, result, 0, mismatch
}

func (s *Store) Wait(ctx context.Context, key string) (*idemkit.Result, error) {
	var notify chan struct{}
	if s.listener != nil {
		notify = s.listener.register(key)
		defer s.listener.unregister(key, notify)
	}
	for {
		result, terminal, err := s.probe(ctx, key)
		if err != nil {
			return nil, err
		}
		if terminal {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-notify:
		case <-time.After(s.cfg.PollInterval):
		}
	}
}

func (s *Store) probe(ctx context.Context, key string) (*idemkit.Result, bool, error) {
	var (
		state       int16
		respCode    *int32
		respHeaders []byte
		respBody    []byte
	)
	err := s.pool.QueryRow(ctx, probeSQL, key).Scan(&state, &respCode, &respHeaders, &respBody)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("idemkit/pg: probe: %w", err)
	}
	if state == stateInFlight {
		return nil, false, nil
	}
	result, err := decodeResult(respCode, respHeaders, respBody)
	if err != nil {
		return nil, false, fmt.Errorf("idemkit/pg: decode result: %w", err)
	}
	return result, true, nil
}

func (s *Store) Save(ctx context.Context, key string, token idemkit.Token, result *idemkit.Result) error {
	ttlMillis := s.cfg.TTL.Milliseconds()
	if ttlMillis <= 0 {
		ttlMillis = 1
	}

	headerBytes, err := encodeHeader(result.Header)
	if err != nil {
		return fmt.Errorf("idemkit/pg: encode header: %w", err)
	}

	tag, err := s.pool.Exec(ctx, saveSQL,
		stateDone,
		int32(result.StatusCode),
		headerBytes,
		result.Body,
		ttlMillis,
		key,
		int64(token),
		s.notifyArg(),
	)
	if err != nil {
		return fmt.Errorf("idemkit/pg: save: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return idemkit.ErrTokenMismatch
	}
	return nil
}

func (s *Store) Release(ctx context.Context, key string, token idemkit.Token) error {
	_, err := s.pool.Exec(ctx, releaseSQL, key, int64(token), s.notifyArg())
	if err != nil {
		return fmt.Errorf("idemkit/pg: release: %w", err)
	}
	return nil
}

func encodeHeader(h http.Header) ([]byte, error) {
	if h == nil {
		return nil, nil
	}
	return json.Marshal(h)
}

func decodeHeader(b []byte) (http.Header, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var h http.Header
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, err
	}
	return h, nil
}

func decodeResult(respCode *int32, respHeaders, respBody []byte) (*idemkit.Result, error) {
	r := &idemkit.Result{}
	if respCode != nil {
		r.StatusCode = int(*respCode)
	}
	hdr, err := decodeHeader(respHeaders)
	if err != nil {
		return nil, err
	}
	r.Header = hdr
	if len(respBody) > 0 {
		r.Body = respBody
	}
	return r, nil
}
