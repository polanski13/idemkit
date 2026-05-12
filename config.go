package idemkit

import (
	"log/slog"
	"net/http"
	"time"
)

type Config struct {
	Header            string
	TTL               time.Duration
	LockTimeout       time.Duration
	Methods           []string
	MaxRequestBytes   int64
	MaxResponseBytes  int64
	CacheServerErrors bool
	ConflictMode      ConflictMode
	Hasher            func([]byte) []byte
	KeyExtractor      func(r *http.Request) (string, error)
	KeyScope          func(r *http.Request) string
	SkipFunc          func(r *http.Request) bool
	OnConflict        func(http.ResponseWriter, *http.Request, ConflictReason)
	Logger            *slog.Logger
}

const (
	defaultHeader           = "Idempotency-Key"
	defaultTTL              = 24 * time.Hour
	defaultLockTimeout      = 30 * time.Second
	defaultMaxRequestBytes  = 1 << 20
	defaultMaxResponseBytes = 1 << 20
	replayHeader            = "X-Idemkit-Replayed"
	maxBeginAttempts        = 3
)

var defaultMethods = []string{
	http.MethodPost,
	http.MethodPut,
	http.MethodPatch,
	http.MethodDelete,
}

func (c *Config) applyDefaults() {
	if c.Header == "" {
		c.Header = defaultHeader
	}
	if c.TTL <= 0 {
		c.TTL = defaultTTL
	}
	if c.LockTimeout <= 0 {
		c.LockTimeout = defaultLockTimeout
	}
	if len(c.Methods) == 0 {
		c.Methods = defaultMethods
	}
	if c.MaxRequestBytes <= 0 {
		c.MaxRequestBytes = defaultMaxRequestBytes
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = defaultMaxResponseBytes
	}
	if c.Hasher == nil {
		c.Hasher = DefaultHasher
	}
	if c.KeyExtractor == nil {
		c.KeyExtractor = headerKeyExtractor(c.Header)
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

func headerKeyExtractor(header string) func(*http.Request) (string, error) {
	return func(r *http.Request) (string, error) {
		return r.Header.Get(header), nil
	}
}
