package idemkit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
)

func Middleware(store Store, cfg Config) func(http.Handler) http.Handler {
	cfg.applyDefaults()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handle(w, r, store, &cfg, next)
		})
	}
}

func handle(w http.ResponseWriter, r *http.Request, store Store, cfg *Config, next http.Handler) {
	if cfg.SkipFunc != nil && cfg.SkipFunc(r) {
		next.ServeHTTP(w, r)
		return
	}
	if !slices.Contains(cfg.Methods, r.Method) {
		next.ServeHTTP(w, r)
		return
	}
	key, err := cfg.KeyExtractor(r)
	if err != nil {
		cfg.Logger.Warn("idemkit: key extractor error", "err", err)
		next.ServeHTTP(w, r)
		return
	}
	if key == "" {
		next.ServeHTTP(w, r)
		return
	}

	body, oversize, err := readBody(r, cfg.MaxRequestBytes)
	if err != nil {
		cfg.Logger.Warn("idemkit: read request body", "err", err)
		http.Error(w, "idemkit: failed to read request body", http.StatusBadRequest)
		return
	}
	if oversize {
		http.Error(w, "idemkit: request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	scope := ""
	if cfg.KeyScope != nil {
		scope = cfg.KeyScope(r)
	}
	storageKey := buildStorageKey(scope, key)

	fp := Fingerprint{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.Query(),
		Body:   body,
	}
	hash := cfg.Hasher(fp.Canonical())

	for attempt := 0; attempt < maxBeginAttempts; attempt++ {
		state, cached, token, err := store.Begin(r.Context(), storageKey, hash)
		if err != nil {
			if errors.Is(err, ErrBodyMismatch) {
				handleConflict(w, r, cfg, ReasonBodyMismatch)
				return
			}
			cfg.Logger.Warn("idemkit: store Begin", "err", err, "key", storageKey)
			next.ServeHTTP(w, r)
			return
		}
		switch state {
		case StateFresh:
			runFresh(w, r, store, cfg, storageKey, token, next)
			return
		case StateInFlight:
			waited, err := store.Wait(r.Context(), storageKey)
			if err != nil {
				cfg.Logger.Warn("idemkit: store Wait", "err", err, "key", storageKey)
				http.Error(w, "idemkit: wait failed", http.StatusServiceUnavailable)
				return
			}
			if waited == nil {
				continue
			}
			replay(w, waited)
			return
		case StateDone:
			replay(w, cached)
			return
		}
	}

	cfg.Logger.Warn("idemkit: retry budget exhausted", "key", storageKey)
	next.ServeHTTP(w, r)
}

func runFresh(w http.ResponseWriter, r *http.Request, store Store, cfg *Config, storageKey string, token Token, next http.Handler) {
	cw := newResponseWriter(w, cfg.MaxResponseBytes)
	completed := false
	defer func() {
		if !completed {
			_ = store.Release(context.Background(), storageKey, token)
		}
	}()
	next.ServeHTTP(cw, r)
	completed = true

	if !cw.cacheable() {
		if err := store.Release(r.Context(), storageKey, token); err != nil {
			cfg.Logger.Warn("idemkit: store Release", "err", err, "key", storageKey)
		}
		return
	}

	result := cw.snapshot()
	if result.StatusCode >= 500 && !cfg.CacheServerErrors {
		if err := store.Release(r.Context(), storageKey, token); err != nil {
			cfg.Logger.Warn("idemkit: store Release", "err", err, "key", storageKey)
		}
		return
	}

	if err := store.Save(r.Context(), storageKey, token, result); err != nil {
		cfg.Logger.Warn("idemkit: store Save", "err", err, "key", storageKey)
		_ = store.Release(r.Context(), storageKey, token)
	}
}

func handleConflict(w http.ResponseWriter, r *http.Request, cfg *Config, reason ConflictReason) {
	if cfg.OnConflict != nil {
		cfg.OnConflict(w, r, reason)
		return
	}
	http.Error(w, "idemkit: idempotency-key conflict ("+reason.String()+")", http.StatusUnprocessableEntity)
}

func replay(w http.ResponseWriter, result *Result) {
	hdr := w.Header()
	for k, v := range result.Header {
		hdr[k] = slices.Clone(v)
	}
	hdr.Set(replayHeader, "true")
	w.WriteHeader(result.StatusCode)
	if len(result.Body) > 0 {
		_, _ = w.Write(result.Body)
	}
}

func readBody(r *http.Request, maxBytes int64) ([]byte, bool, error) {
	if r.Body == nil {
		return nil, false, nil
	}
	limited := io.LimitReader(r.Body, maxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > maxBytes {
		_, _ = io.Copy(io.Discard, r.Body)
		return nil, true, nil
	}
	return body, false, nil
}

func buildStorageKey(scope, key string) string {
	if scope == "" {
		return key
	}
	return strconv.Itoa(len(scope)) + ":" + scope + key
}
