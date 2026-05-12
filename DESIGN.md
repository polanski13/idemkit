# idemkit: architecture decisions

This document records why the code is shaped the way it is, deviations from the original plan, and named limitations. Treat it as the long-form companion to the README's "How it works" section.

## Public surface

```
github.com/polanski13/idemkit              # Middleware, Config, Fingerprint, Store, Result, State,
                                           # ConflictMode, ConflictReason, ErrBodyMismatch, DefaultHasher
github.com/polanski13/idemkit/store/mem    # mem.Store, mem.Config, mem.New
```

Subpackages are added when there is something to put in them. `store/pg` and `store/redis` land in v0.2 / v0.3. No `adapter/chi`, `adapter/gin`, `adapter/echo` packages — for chi/gin the middleware composes as `r.Use(idemkit.Middleware(...))` with no glue needed. Echo and Fiber have different handler shapes and could warrant small adapter packages post-v1.0 if there's demand.

## Fingerprint: length-prefixed framing

The fingerprint is SHA-256 over a deterministic byte stream:

```
[len(method)] [method] [len(path)] [path] [len(canonical query)] [canonical query]
[len(body)] [body] [len(canonical headers)] [canonical headers] [len(scope)] [scope]
```

Each component is prefixed with its byte length as `uint32` big-endian. Query and header pairs are sorted (keys lexicographically, values within each key lexicographically). Header keys are case-folded via `textproto.CanonicalMIMEHeaderKey`.

**Why length-prefix?** Concat-with-separator schemes (used by most prior implementations) collide on boundary-ambiguous inputs: `(method="POST", path="/foo", body="")` and `(method="POS", path="T/foo", body="")` produce identical bytes when concatenated. We commit to no collisions via an explicit test (`TestCanonical_BoundaryAmbiguityResolved`) so it cannot regress.

**Why `nil` and empty are equivalent.** A `nil` slice and an empty `[]byte{}` both length-prefix as `[0,0,0,0]`. Tested via `TestCanonical_NilAndEmptyEquivalent`. Distinguishing them would over-constrain the contract.

## In-memory store: sync.Mutex + map + channel-close broadcast

```go
type entry struct {
    state     idemkit.State
    bodyHash  []byte
    result    *idemkit.Result
    expiresAt time.Time
    waiters   chan struct{} // closed on state change; broadcasts wake-up
}
```

**Why channel-close, not `sync.Cond`?** `sync.Cond` does not compose with `select { case <-ctx.Done() }`. Channel close gives both broadcast semantics (one close wakes all waiters) and native `ctx` cancellation. Same pattern is used in `context.Context.Done()` and `golang.org/x/sync/singleflight`.

**Why a fresh channel per entry?** Entries are single-shot: after `Save` or `Release`, the entry is either replaced or removed. The closed channel is never reused. New entry → new channel.

**Lazy expiration via `lookupLocked(key, now)`.** TTL and `LockTimeout` are enforced on read: when `Begin` or `Wait` accesses a key, expired entries are purged in the same map lookup. No background janitor goroutine in v0.1 — keeps the store dependency-free and trivially predictable in tests. A janitor lands in v0.2+ if needed for bounded memory under adversarial load.

## Response capture: pass-through + drop on streaming

The wrapper (`responseWriter`) writes byte-for-byte to the underlying `http.ResponseWriter` AND copies to a capture buffer up to `MaxResponseBytes`. If either of these happens:

- handler calls `Flush()` (detected via the wrapper's own `Flush` method)
- handler writes more than `MaxResponseBytes`

…the capture buffer is dropped and the response is marked uncacheable. The client still receives the full pass-through stream.

**Why always implement `http.Flusher`?** Two reasons:

1. Semantically, the handler's *intent* to flush — regardless of whether the underlying writer honors it — is the signal we care about for cacheability.
2. Conditional interface assertion (the chi / gorilla pattern of returning different wrapped types based on which interfaces the base supports) adds significant complexity without changing observable behavior for the typical `net/http` server.

**Why drop the buffer entirely on oversize, not truncate?** Caching a truncated body would replay an incomplete response, which is wrong. All-or-nothing.

**`Hijacker` and friends.** The wrapper does NOT implement `http.Hijacker`, `http.Pusher`, or `io.ReaderFrom`. Handlers that need these (WebSocket upgrades, HTTP/2 push) cannot also be wrapped by `idemkit`. Either skip via `SkipFunc` or split routing so idempotency-required endpoints are separate.

## Middleware state machine

```
                   readBody (capped at MaxRequestBytes)
                                │
                                ▼
                          store.Begin
                                │
            ┌───────────────────┼───────────────────┐
            ▼                   ▼                   ▼
          Fresh             InFlight              Done
            │                   │                   │
            │              store.Wait               │
            │                   │                   │
            │             ┌─────┴──────┐            │
            │             ▼            ▼            │
            │          cached         nil ──────────┤
            │             │       (released)        │
            │             │           (retry, max 3 attempts)
            │             ▼                         │
            │           replay                      │
            ▼                                       ▼
       run handler                                replay
            │
            ├── panic         → defer fires Release
            ├── cacheable && (status < 500 || CacheServerErrors)
            │                  → store.Save
            └── otherwise     → store.Release
```

**Retry budget = 3.** `Wait → nil` happens when a held claim is released without a result (handler panicked or lock timed out and another caller reclaimed). After 3 attempts of all returning `nil`, the middleware logs and passes through to the handler without idempotency. In practice, 1 attempt suffices.

**Defer + completed flag for panic safety.** `runFresh` registers a `defer` that calls `store.Release(context.Background(), key)` only if a `completed bool` flag is still false. Normal flow sets `completed = true` at the end of `next.ServeHTTP`; panics propagate without resetting it, so the defer fires Release before unwinding. The pattern is identical to how `database/sql` handles transaction rollback on panic.

**Background context for cleanup-on-panic.** The request context may already be cancelled by the time the panic propagates. `context.Background()` ensures the Release reaches the store regardless. For normal-flow `Release` / `Save` we still use `r.Context()` — those only fail if the client is already gone, in which case losing the cache entry is acceptable (the lock timeout reclaims it eventually).

## Storage key composition

```go
storageKey = strconv.Itoa(len(scope)) + ":" + scope + key
```

Length-prefix is collision-safe regardless of scope/key contents. `(scope="6:user", key="A")` produces `"6:6:userA"`; `(scope="6", key=":userA")` produces `"1:6:userA"`. Different.

**Scope namespacing is via the storage key, not the fingerprint.** Two callers with different `KeyScope` values produce different storage keys → independent cache entries, no cross-tenant collisions to detect at the fingerprint layer. The original plan put scope in the fingerprint; that would have produced confusing `ErrBodyMismatch` responses on cross-tenant key collisions instead of clean isolation. See [Deviations](#deviations-from-the-original-plan).

## Body handling

```go
limited := io.LimitReader(r.Body, maxBytes+1)
body, err := io.ReadAll(limited)
if int64(len(body)) > maxBytes {
    _, _ = io.Copy(io.Discard, r.Body) // drain remaining for keep-alive
    return 413
}
r.Body = io.NopCloser(bytes.NewReader(body))
```

The `+1` lets us read one byte past the limit to detect oversize unambiguously. On oversize, we drain the remaining bytes so HTTP keep-alive isn't broken — `net/http`'s auto-drain stops at ~4KB and would close the connection for larger bodies.

**Why not `http.MaxBytesReader`?** It's the canonical `net/http` pattern, but requires passing `w` to mark the connection for close. Coupling the body-read helper to the response writer just to get connection-close semantics didn't feel worth it for v0.1. `io.LimitReader` + manual drain is equivalent for our needs.

**Original body's `Close()` is not called.** Standard pattern for body-rewriting middleware (`httputil.DumpRequest`, chi body-buffer, echo body-dump all do the same). `net/http` server still cleans up the connection.

## Replay

```go
hdr := w.Header()
for k, v := range result.Header {
    hdr[k] = slices.Clone(v)
}
hdr.Set("X-Idemkit-Replayed", "true")
w.WriteHeader(result.StatusCode)
w.Write(result.Body)
```

**Headers are overwritten per-key, not cleared globally.** Headers set by upstream middleware (CORS, security headers) on the same response writer survive replay. The cached response's headers take precedence for keys it owns; other keys are untouched. This is the practical middleware-stack-friendly default; strict "exact replay" semantics would require clearing `w.Header()` first, which clobbers sibling middleware contributions.

**`X-Idemkit-Replayed: true`** is always set on replays. Use it in tests to verify caching is working.

## Deviations from the original plan

The plan in `~/Downloads/idempotent-go-plan.md` is the design contract. Where the implementation diverges:

### 1. `OnlySuccess bool` (default true) → `CacheServerErrors bool` (default false)

Original plan field: `OnlySuccess bool // default: true (do not cache 5xx)`. Problem: Go's bool zero-value is `false`, so a struct-literal `Config{}` has `OnlySuccess: false` — the opposite of the documented default. The standard Go workarounds (`*bool`, hidden "applied" flag, doc-only convention) all have ergonomic costs.

Resolution: invert the name and the default. `CacheServerErrors bool // default: false`. The zero value matches the documented behavior. Same semantics.

### 2. `KeyScope` goes into the storage key, not the fingerprint

Original plan: "The scope value is folded into the body-hash so two users with the same key+body produce different fingerprints."

Resolution: prefix `KeyScope` into the storage key instead. Trade-off:

| Approach | Cross-tenant key-collision behavior |
|----------|-------------------------------------|
| Plan: scope in fingerprint | User B sees `ErrBodyMismatch` → 422 (confusing — B's body might be identical to A's, the only difference is scope) |
| Impl: scope in storage key | Independent cache entries; B's request proceeds normally |

The plan's approach offers defense-in-depth against bodyHash collisions across scopes, but with SHA-256 fingerprints, scope-in-fingerprint adds no real collision resistance over scope-in-storage-key. The implementation choice gives cleaner isolation semantics.

`Fingerprint.Scope` still exists as a public field and is honored by `Canonical()` — callers using `Fingerprint` directly can opt into scope-in-hash. The middleware does not.

### 3. No `internal/conformance/` package in v0.1

The plan promised `internal/conformance/stripe_test.go` and `internal/conformance/ietf_draft07_test.go`. The Stripe semantics are exercised by `middleware_test.go` instead; the IETF conformance suite lands in v0.2 alongside `ConflictIETF` implementation.

## Known limitations (v0.1)

These are documented because they will eventually need fixing, but are not v0.1 blockers.

### 1. Lock-timeout + reclaim race in `Save`

Sequence:
1. Caller A claims key with body hash `H_A`.
2. A's lock times out without `Save`.
3. Caller B reclaims the key with a new claim, body hash `H_B`.
4. A's handler finally completes and calls `Save` with result `R_A`.
5. The map entry now has body hash `H_B` (from B's claim) and result `R_A` (from A's `Save`). Subsequent `Begin` callers see `Done` + body mismatch even if their hash matches B's intent.

Realistically the precondition is rare (handler exceeded `LockTimeout` while another caller racing on the same key). The fix is **generation tokens** — every claim returns a token; `Save` requires the token and refuses to overwrite if it changed. Lands in v0.2 alongside the Postgres backend, where this race is more likely in practice.

### 2. `Result` is not defensively cloned

The `*Result` returned from `Begin` / `Wait` is the same pointer the store holds internally. A caller that mutates `Result.Body` or `Result.Header` after replay corrupts the cache.

The middleware itself does not mutate (it calls `slices.Clone` on each header value when writing back). Custom `OnConflict` callbacks or direct `Store` users could.

Fix: add a `(*Result).Clone()` method and have the store call it on storage and retrieval. Lands in v0.2.

### 3. Waiter without `ctx.Deadline` can theoretically block forever

A blocked `Wait` exits via:

- `Save` (channel close, returns result)
- `Release` (channel close, returns nil)
- `LockTimeout` expiration triggered by another `Begin` / `Wait` (channel close via `lookupLocked`, returns nil)
- `ctx.Done()` (returns ctx error)

If none happen — no other caller touches the key, the original claim holder is stuck, the waiter's ctx has no deadline — the waiter blocks indefinitely. Pathological but possible.

**Mitigation in v0.1:** document this and recommend `context.WithTimeout` for callers.

**Fix in v0.2:** add an optional `JanitorInterval` to `mem.Config` for proactive expiry, which would close stale entries' waiter channels regardless of access patterns.

### 4. Same idempotency-key reused across different endpoints produces 422

Path is part of the fingerprint. Two requests with the same key but different paths produce different hashes, and the second is rejected as `ErrBodyMismatch`.

**Workaround:** generate a fresh key per request (recommended anyway — UUIDv7).

**Future consideration:** include path in the storage key alongside scope, making per-endpoint key reuse safe. Currently treated as cosmetic — not blocking.

### 5. No support for `http.Hijacker`, `http.Pusher`, `io.ReaderFrom`

The response writer wrapper does not forward these interfaces. Handlers that need WebSocket upgrades or HTTP/2 server push must bypass `idemkit` via `SkipFunc`. Adding interface-conditional wrappers (the chi pattern) is ~50 LOC of glue code; not in v0.1 scope.

### 6. `Config.Logger` is the only observability hook

No Prometheus metrics, no OpenTelemetry spans. Callers wire those via the `Logger` field or their own middleware. The roadmap deliberately doesn't add observability hooks — keeping the surface tight matters more than feature completeness for a v0.x library.
