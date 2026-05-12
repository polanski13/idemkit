package redis

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/polanski13/idemkit"
)

const (
	defaultTTL          = 24 * time.Hour
	defaultLockTimeout  = 30 * time.Second
	defaultPollInterval = 100 * time.Millisecond
	defaultKeyPrefix    = "idemkit:"
	notifySuffix        = "notify"

	stateInFlight = "0"
	stateDone     = "1"
)

type Config struct {
	TTL          time.Duration
	LockTimeout  time.Duration
	PollInterval time.Duration
	KeyPrefix    string
	PubSub       bool
}

type Store struct {
	client    goredis.UniversalClient
	cfg       Config
	sub       *subscriber
	closeOnce sync.Once
}

func New(client goredis.UniversalClient, cfg Config) *Store {
	if cfg.TTL <= 0 {
		cfg.TTL = defaultTTL
	}
	if cfg.LockTimeout <= 0 {
		cfg.LockTimeout = defaultLockTimeout
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = defaultKeyPrefix
	}
	s := &Store{client: client, cfg: cfg}
	if cfg.PubSub {
		s.sub = newSubscriber(client, s.notifyChannel())
	}
	return s
}

func (s *Store) Close() error {
	if s.sub == nil {
		return nil
	}
	var err error
	s.closeOnce.Do(func() {
		err = s.sub.close()
	})
	return err
}

func (s *Store) notifyChannel() string {
	return s.cfg.KeyPrefix + notifySuffix
}

func (s *Store) publishArgs(key string) (string, string) {
	if !s.cfg.PubSub {
		return "", ""
	}
	return s.notifyChannel(), key
}

var beginScript = goredis.NewScript(`
local key = KEYS[1]
if redis.call('EXISTS', key) == 0 then
    redis.call('HSET', key, 'state', '0', 'token', ARGV[2], 'body_hash', ARGV[1])
    redis.call('PEXPIRE', key, ARGV[3])
    return {'fresh'}
end
local function nilSafe(v)
    if v == false or v == nil then return '' end
    return v
end
local f = redis.call('HMGET', key, 'state', 'body_hash', 'response_code', 'response_headers', 'response_body')
return {'existing', nilSafe(f[1]), nilSafe(f[2]), nilSafe(f[3]), nilSafe(f[4]), nilSafe(f[5])}
`)

var saveScript = goredis.NewScript(`
local key = KEYS[1]
local stored = redis.call('HGET', key, 'token')
if not stored or stored ~= ARGV[1] then
    return 'tokenmismatch'
end
redis.call('HSET', key,
    'state', ARGV[2],
    'response_code', ARGV[3],
    'response_headers', ARGV[4],
    'response_body', ARGV[5])
redis.call('PEXPIRE', key, ARGV[6])
if ARGV[7] ~= '' then
    redis.call('PUBLISH', ARGV[7], ARGV[8])
end
return 'ok'
`)

var releaseScript = goredis.NewScript(`
local key = KEYS[1]
local stored = redis.call('HGET', key, 'token')
if not stored or stored ~= ARGV[1] then
    return 0
end
redis.call('DEL', key)
if ARGV[2] ~= '' then
    redis.call('PUBLISH', ARGV[2], ARGV[3])
end
return 1
`)

func newToken() idemkit.Token {
	for {
		var buf [8]byte
		_, _ = rand.Read(buf[:])
		t := binary.BigEndian.Uint64(buf[:])
		if t != 0 {
			return idemkit.Token(t)
		}
	}
}

func (s *Store) Begin(ctx context.Context, key string, bodyHash []byte) (idemkit.State, *idemkit.Result, idemkit.Token, error) {
	tok := newToken()
	lockMillis := s.cfg.LockTimeout.Milliseconds()
	if lockMillis <= 0 {
		lockMillis = 1
	}
	fullKey := s.cfg.KeyPrefix + key
	raw, err := beginScript.Run(ctx, s.client, []string{fullKey},
		bodyHash,
		strconv.FormatUint(uint64(tok), 10),
		lockMillis,
	).Result()
	if err != nil {
		return 0, nil, 0, fmt.Errorf("idemkit/redis: begin: %w", err)
	}
	arr, ok := raw.([]interface{})
	if !ok || len(arr) == 0 {
		return 0, nil, 0, fmt.Errorf("idemkit/redis: begin: unexpected reply %T", raw)
	}
	tag, _ := arr[0].(string)
	switch tag {
	case "fresh":
		return idemkit.StateFresh, nil, tok, nil
	case "existing":
		if len(arr) < 6 {
			return 0, nil, 0, fmt.Errorf("idemkit/redis: begin: short existing reply (%d)", len(arr))
		}
		storedState := stringOrEmpty(arr[1])
		storedHash := stringOrEmpty(arr[2])
		var mismatch error
		if storedHash != string(bodyHash) {
			mismatch = idemkit.ErrBodyMismatch
		}
		if storedState == stateInFlight {
			return idemkit.StateInFlight, nil, 0, mismatch
		}
		result, err := decodeResult(arr[3], arr[4], arr[5])
		if err != nil {
			return 0, nil, 0, fmt.Errorf("idemkit/redis: begin decode: %w", err)
		}
		return idemkit.StateDone, result, 0, mismatch
	default:
		return 0, nil, 0, fmt.Errorf("idemkit/redis: begin: unexpected tag %q", tag)
	}
}

func (s *Store) Wait(ctx context.Context, key string) (*idemkit.Result, error) {
	fullKey := s.cfg.KeyPrefix + key
	var notify chan struct{}
	if s.sub != nil {
		notify = s.sub.register(key)
		defer s.sub.unregister(key, notify)
	}
	for {
		result, terminal, err := s.probe(ctx, fullKey)
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

func (s *Store) probe(ctx context.Context, fullKey string) (*idemkit.Result, bool, error) {
	vals, err := s.client.HMGet(ctx, fullKey, "state", "response_code", "response_headers", "response_body").Result()
	if err != nil {
		return nil, false, fmt.Errorf("idemkit/redis: probe: %w", err)
	}
	if len(vals) == 0 || vals[0] == nil {
		return nil, true, nil
	}
	state := stringOrEmpty(vals[0])
	if state == stateInFlight {
		return nil, false, nil
	}
	result, err := decodeResult(vals[1], vals[2], vals[3])
	if err != nil {
		return nil, false, fmt.Errorf("idemkit/redis: probe decode: %w", err)
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
		return fmt.Errorf("idemkit/redis: encode header: %w", err)
	}
	if headerBytes == nil {
		headerBytes = []byte{}
	}
	body := result.Body
	if body == nil {
		body = []byte{}
	}
	fullKey := s.cfg.KeyPrefix + key
	notifyChan, notifyKey := s.publishArgs(key)
	raw, err := saveScript.Run(ctx, s.client, []string{fullKey},
		strconv.FormatUint(uint64(token), 10),
		stateDone,
		strconv.Itoa(result.StatusCode),
		headerBytes,
		body,
		ttlMillis,
		notifyChan,
		notifyKey,
	).Result()
	if err != nil {
		return fmt.Errorf("idemkit/redis: save: %w", err)
	}
	reply, _ := raw.(string)
	if reply == "tokenmismatch" {
		return idemkit.ErrTokenMismatch
	}
	if reply != "ok" {
		return fmt.Errorf("idemkit/redis: save: unexpected reply %q", reply)
	}
	return nil
}

func (s *Store) Release(ctx context.Context, key string, token idemkit.Token) error {
	fullKey := s.cfg.KeyPrefix + key
	notifyChan, notifyKey := s.publishArgs(key)
	_, err := releaseScript.Run(ctx, s.client, []string{fullKey},
		strconv.FormatUint(uint64(token), 10),
		notifyChan,
		notifyKey,
	).Result()
	if err != nil {
		return fmt.Errorf("idemkit/redis: release: %w", err)
	}
	return nil
}

func stringOrEmpty(v interface{}) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
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

func decodeResult(codeRaw, headersRaw, bodyRaw interface{}) (*idemkit.Result, error) {
	r := &idemkit.Result{}
	if codeStr := stringOrEmpty(codeRaw); codeStr != "" {
		n, err := strconv.Atoi(codeStr)
		if err != nil {
			return nil, err
		}
		r.StatusCode = n
	}
	hdr, err := decodeHeader([]byte(stringOrEmpty(headersRaw)))
	if err != nil {
		return nil, err
	}
	r.Header = hdr
	if bodyStr := stringOrEmpty(bodyRaw); bodyStr != "" {
		r.Body = []byte(bodyStr)
	}
	return r, nil
}
