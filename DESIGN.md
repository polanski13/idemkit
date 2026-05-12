# idemkit: architecture decisions

This document records why the code is shaped the way it is, deviations from the original plan, and named limitations. Treat it as the long-form companion to the README's "How it works" section.

## Public surface

```
github.com/polanski13/idemkit              # Middleware, Config, Fingerprint, Store, Result, State, Token,
                                           # ConflictMode, ConflictReason, ErrBodyMismatch, ErrTokenMismatch,
                                           # DefaultHasher
github.com/polanski13/idemkit/store/mem    # mem.Store, mem.Config, mem.New
github.com/polanski13/idemkit/store/pg     # pg.Store, pg.Config, pg.New, pg.ApplySchema (v0.2)
github.com/polanski13/idemkit/store/redis  # redis.Store, redis.Config, redis.New (v0.3)
```

Subpackages are added when there is something to put in them. No `adapter/chi`, `adapter/gin`, `adapter/echo` packages — for chi/gin the middleware composes as `r.Use(idemkit.Middleware(...))` with no glue needed. Echo and Fiber have different handler shapes and could warrant small adapter packages post-v1.0 if there's demand.

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

## Postgres store (v0.2)

`store/pg` implements `idemkit.Store` on top of `pgx/v5`. Same behavioural contract as `mem.Store`, with serialised state durably persisted across processes.

### Schema

```sql
CREATE SEQUENCE idemkit_token_seq AS BIGINT INCREMENT BY 1 START WITH 1 NO CYCLE;

CREATE TABLE idemkit_keys (
    key              TEXT        PRIMARY KEY,
    body_hash        BYTEA       NOT NULL,
    state            SMALLINT    NOT NULL,
    response_code    INT,
    response_headers JSONB,
    response_body    BYTEA,
    locked_at        TIMESTAMPTZ NOT NULL,
    completed_at     TIMESTAMPTZ,
    expires_at       TIMESTAMPTZ NOT NULL,
    token            BIGINT      NOT NULL
);

CREATE INDEX idemkit_keys_expires_at ON idemkit_keys (expires_at);
```

`response_headers` is JSONB; `http.Header` (`map[string][]string`) round-trips through `encoding/json` cleanly, preserving the nil-vs-empty distinction (`null` JSON for nil, `{}` for empty). `response_body` is `BYTEA`.

### Begin

Wrapped in a transaction for atomicity between insert-attempt and follow-up SELECT/UPDATE:

1. `INSERT ... ON CONFLICT DO NOTHING RETURNING token` with `nextval('idemkit_token_seq')`. If a row is returned → `Fresh`, commit, return token.
2. Otherwise the key already exists. `SELECT ... FOR UPDATE` locks the row for the rest of the transaction.
3. If `expires_at < NOW()` (lock-timeout or TTL elapsed), `UPDATE` the row in place — new state, new bodyHash, new locked_at, new expires_at, new token from the sequence, response_* cleared. Commit. Return `Fresh` with new token.
4. Otherwise return `InFlight` / `Done` per state, with `ErrBodyMismatch` if hashes differ. Commit (releases the FOR UPDATE lock).

3 round trips in the worst case (Begin → Insert miss → Select → maybe Update). Can be collapsed into a single CTE later if benchmarks show the latency matters.

### Wait

Polling-based in v0.2 (default 100 ms). On each tick, query the row's state filtered by `expires_at > NOW()`:

- `pgx.ErrNoRows` → entry absent or expired → return `(nil, nil)` (caller retries `Begin`)
- state=done → return cached result
- state=in_flight → tick again

Tight loop on `ctx.Done()` exits cleanly with `ctx.Err()`.

LISTEN/NOTIFY overlay is opt-in via `Config.ListenConn` — see "LISTEN/NOTIFY overlay" below. Polling at 100 ms is good enough for the latencies idempotency targets on its own; LISTEN is a latency hint, not a correctness requirement.

### Save

```sql
WITH upd AS (
    UPDATE idemkit_keys
    SET state = $1, response_code = $2, response_headers = $3, response_body = $4,
        completed_at = NOW(), expires_at = NOW() + ($5 * INTERVAL '1 millisecond')
    WHERE key = $6 AND token = $7
    RETURNING 1
)
SELECT CASE WHEN $8 != '' THEN pg_notify($8, $6) END FROM upd
```

If the inner UPDATE matches 0 rows, the outer SELECT returns 0 rows and `RowsAffected() == 0` → `ErrTokenMismatch`. Same semantics as `mem.Store`: another caller reclaimed the key, our token is stale.

`$8` is the LISTEN channel name (passed as `""` when the overlay is disabled). The `CASE WHEN` short-circuit means `pg_notify` is not called when the channel is empty — same set of SQL constants covers both modes.

### Release

```sql
WITH del AS (
    DELETE FROM idemkit_keys WHERE key = $1 AND token = $2 RETURNING 1
)
SELECT CASE WHEN $3 != '' THEN pg_notify($3, $1) END FROM del
```

`RowsAffected()` is not checked — Release is idempotent. Absent or mismatched-token both succeed silently. The conditional `pg_notify` follows the same pattern as Save.

### LISTEN/NOTIFY overlay

When `Config.ListenConn` is a non-nil dedicated `*pgx.Conn`, `New` issues `LISTEN idemkit_notify` on it under a 5 s timeout and spawns a listener goroutine that calls `WaitForNotification(ctx)` in a loop, dispatching each notification's payload (the bare key) to in-process waiters registered by `Wait`. If the initial `LISTEN` fails, the store silently degrades to polling-only — same as runtime listener-conn errors.

**Why a dedicated `*pgx.Conn`.** `LISTEN` is connection-state in PostgreSQL: a pooled connection returned to `pgxpool` and handed to another query mid-LISTEN would lose the subscription. The store therefore requires the caller to supply a separately-dialed `*pgx.Conn` it doesn't multiplex with anything else.

**Why polling stays.** The listener can drop (network blip, server restart, conn pool churn) and lose any notifications emitted during the gap — `pg_notify` is delivered at commit time and not persisted thereafter. Polling at `PollInterval` remains the source of truth; LISTEN is a latency optimization that short-circuits the polling tick when a relevant event arrives. The architecture cannot become silently incorrect by losing notifications.

**Wait integration.** Same nil-channel idiom as `store/redis`: a `var notify chan struct{}` is registered with the listener when the overlay is enabled and stays nil otherwise. The probe loop's `select` includes a `case <-notify` branch that simply never fires when notify is nil. One select block covers both modes.

**Lifecycle.** `Store.Close()` cancels the listener's run context (`WaitForNotification` returns with the cancellation error) and waits for the goroutine `done` chan. Idempotent via `sync.Once`; no-op when `ListenConn` is unset. The store does **not** close the caller's `*pgx.Conn` — that lifecycle stays with the caller. Recommended pattern: `defer store.Close()` followed by `defer listenConn.Close(ctx)`.

**Channel and payload.** Channel name is fixed (`idemkit_notify`); pg has no `KeyPrefix` analogue because keys live in the `idemkit_keys` table, not the channel namespace. Payload is the bare key, well under PostgreSQL's 8000-byte NOTIFY payload limit for any realistic idempotency-key value. The store side issues `LISTEN` (not `PSUBSCRIBE`-style pattern matching), so SQL identifier escaping is irrelevant — the channel name is a constant.

### Token generation

A single Postgres `SEQUENCE` (`idemkit_token_seq`) hands out globally unique BIGINT tokens. Sequences in Postgres are MVCC-aware and lock-free; no contention even at high claim rates. Wrap-around at 2^63: 100K claims/sec for 2.9 million years. Effectively infinite.

### Why no advisory locks

The plan considered `pg_try_advisory_xact_lock` as a secondary fence. `hashtext(key)` collides under high cardinality (int4 keyspace, 32-bit hash). The two-int4 form (`pg_try_advisory_xact_lock(int4, int4)`) fed with the first and last 32 bits of SHA-256(key) gives 64-bit collision resistance — but the row-based claim with `FOR UPDATE` already serialises operations correctly. Advisory locks add overhead for no correctness gain. Documented; not implemented.

## Redis store (v0.3)

`store/redis` implements `idemkit.Store` on top of `github.com/redis/go-redis/v9`. Same behavioural contract as `mem.Store` and `pg.Store`, with state durably persisted in Redis and cross-instance coordination via Redis's atomic operations.

### Storage layout

Each idempotency key maps to a single Redis hash at `<KeyPrefix><key>` (`KeyPrefix` defaults to `"idemkit:"`). Fields:

| Field | Meaning |
|-------|---------|
| `state` | `"0"` (in-flight) or `"1"` (done) |
| `token` | Decimal-string-encoded `uint64` |
| `body_hash` | Raw bytes of the request fingerprint |
| `response_code` | Decimal-string-encoded HTTP status |
| `response_headers` | JSON-encoded `http.Header` |
| `response_body` | Raw bytes of the captured response body |

The whole hash is governed by a single TTL via `PEXPIRE` (millisecond precision). All four operations (Begin, Save, Release, Wait) touch only this one key — every Lua script in this store is single-key, which keeps the implementation Redis Cluster–compatible without hash tags.

### Begin

Single Lua script:

1. `EXISTS key` → 0: `HSET` state="0", token=NEW, body_hash=BODY; `PEXPIRE` with `LockTimeout`; return `{'fresh'}`.
2. Otherwise `HMGET` state, body_hash, response_code, response_headers, response_body and return `{'existing', ...}`.

A `nilSafe` helper coerces Redis `nil` returns to empty strings before tabling, because Lua's `{a, nil, c}` truncates at the first nil and the Go decoder needs a fixed-arity reply. The Go side then decodes the `existing` reply into the correct `State` + cached `Result` (if Done) + `ErrBodyMismatch` (if hashes differ).

Atomicity matters: under concurrent `Begin` calls, exactly one will see `EXISTS==0` and create the entry. Lua scripts in Redis are single-threaded relative to other commands on that node — `TestConcurrentClaim_ExactlyOneSeesFresh` verifies this with 20 goroutines.

### Token generation

Tokens are 64-bit values from `crypto/rand` (re-rolled on the 1-in-2^64 chance of zero, which is the "no claim" sentinel). No counter key, no sequence to maintain.

This deviates from `store/pg`, which uses a Postgres `SEQUENCE`. Postgres has cheap, MVCC-aware sequences; Redis does not, and a counter key would introduce a second slot to coordinate in Cluster mode (forcing hash tags or accepting cross-slot script limitations). Random `uint64` tokens sidestep both — collision probability over realistic claim volumes is negligible.

### Wait

Polling-based, default 100 ms (same default as `store/pg`). Each tick: `HMGET` state, response_code, response_headers, response_body. Branches:

- All-nil reply (the key doesn't exist — expired by TTL or removed by `Release`) → return `(nil, nil)`. Caller retries `Begin`.
- `state="0"` (in-flight) → sleep `PollInterval`, repeat.
- `state="1"` (done) → decode `response_*` fields into `Result`, return.

`ctx.Done()` interrupts the sleep cleanly with `ctx.Err()`. Pub/sub overlay is opt-in via `Config.PubSub`; see "Pub/sub overlay" below.

### Pub/sub overlay

When `Config.PubSub` is `true`, `New` spawns a subscriber goroutine that `SUBSCRIBE`s to a single notify channel derived from `KeyPrefix` (default channel: `idemkit:notify`). `Save` and `Release` extend their Lua scripts to `PUBLISH <channel> <bareKey>` after the state mutation; when `PubSub` is disabled, the channel ARGV is passed as `""` and the script's `if ARGV[n] ~= ''` guard skips the publish entirely. One set of scripts, both modes.

`Wait` registers a per-call buffered (size 1) notify chan with the subscriber, then `select`s over `notify-chan`, `polling-tick`, and `ctx.Done()` inside the probe loop. The notify chan is unregistered via `defer` when `Wait` returns. Multiple goroutines waiting on the same key each register their own chan; the subscriber's dispatcher non-blocking-sends to all of them on each published payload.

**Why polling stays.** Redis pub/sub has no persistence — a subscriber that drops or reconnects loses any messages emitted during the gap. Polling remains the source of truth; pub/sub is purely a latency hint that short-circuits the polling tick when a relevant event arrives. The architecture cannot become silently incorrect by losing notifications.

**Channel isolation.** The notify channel is derived from `KeyPrefix`, so two stores configured with different prefixes on the same Redis broadcast on different channels and don't observe each other's events. Verified by `TestPubSub_KeyPrefixIsolatesChannels`.

**Lifecycle.** `Store.Close()` stops the subscriber goroutine: close a stop-chan, `pubsub.Close()` on the go-redis subscription (releases the dedicated connection), then `<-done` for orderly shutdown. Idempotent via `sync.Once`; a no-op when `PubSub` is disabled. Recommended pattern: `defer store.Close()` at process shutdown, or `t.Cleanup` in tests.

### Save

Single Lua script:

1. `HGET token` → if nil or mismatches `ARGV[1]`, return `'tokenmismatch'`.
2. `HSET` state="1", response_*; `PEXPIRE` with `TTL` (not `LockTimeout` — the entry is now Done).
3. Return `'ok'`.

Go translates `'tokenmismatch'` to `idemkit.ErrTokenMismatch`. Same contract as `pg.Save` and `mem.Save`.

### Release

Single Lua script: `HGET token`, verify, `DEL` on match. Mismatched or missing token is a silent no-op (per `idemkit.Store` contract).

### TTL handling and lock-timeout reclaim

Postgres tracks `expires_at` as a column and enforces it via `WHERE expires_at > NOW()` filters; reclaiming an expired-in-flight row needs `SELECT FOR UPDATE` to overwrite it in place. Redis handles this natively: the hash carries the `LockTimeout`-derived TTL while in-flight, and the `TTL`-derived TTL after Save. When a stuck-in-flight key passes its lock timeout, Redis evicts it on its own clock; the next `Begin` sees `EXISTS==0` and claims fresh. No reclaim path, no `SELECT FOR UPDATE` equivalent. `TestLockTimeout_ExpiredInFlightIsReclaimable` and `TestTTL_ExpiredDoneEntryIsReclaimable` cover both flows.

### Why no `WATCH`/`MULTI`/`EXEC`

Optimistic concurrency via `WATCH` is the alternative to Lua. Trade-offs:

- `WATCH` retries on contention; Lua does not (scripts are atomic).
- `WATCH` spreads logic across multiple Go-side round trips; Lua inlines it in one.
- For 4 operations × small payload, Lua's single-RTT path is both simpler and faster.

Lua scripts are loaded by go-redis on first call and reused by SHA on subsequent calls (`EVALSHA`).

### Cluster compatibility

Every Lua script in `store/redis` takes exactly one key. Redis Cluster routes the script to the right slot by that key — no multi-key scripts, no hash tags needed. `Sentinel`, `Ring`, and `ClusterClient` are supported transparently via `redis.UniversalClient` in the constructor signature.

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

## Conflict semantics

| Mode | Body-hash mismatch | Concurrent same-key+same-body | Reference |
|------|--------------------|-------------------------------|-----------|
| `ConflictStripe` (default) | **422 Unprocessable Entity** | wait + replay | [Stripe API docs](https://stripe.com/docs/api/idempotent_requests) |
| `ConflictIETF` | **409 Conflict** | wait + replay | [draft-ietf-httpapi-idempotency-key-header-07 §2.6](https://datatracker.ietf.org/doc/draft-ietf-httpapi-idempotency-key-header/) |

Both modes set `X-Idemkit-Replayed: true` on replays. `OnConflict` callback overrides both — if the caller wires their own conflict response, mode is irrelevant.

**What's not in v0.2 from the IETF draft:**

- RFC 7807 Problem Details response bodies. We emit `text/plain` errors via `http.Error`; callers wanting structured Problem Details should wire `OnConflict`.
- Per-method validation of the key format (the draft is permissive about format; we don't enforce UUID-ness either).

The substantive observable difference between modes is the conflict status code. Replay semantics, header handling, method filtering, and threat-model considerations are mode-independent.

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

### 3. ~~No `internal/conformance/` package in v0.1~~ — closed in v0.2

**Resolved** alongside `ConflictIETF` (v0.2). Both spec-conformance suites live in `internal/conformance/`:

- `stripe_test.go` — Stripe semantics (422 on body-hash mismatch; replay on match; wait on concurrent duplicate; replay header present; cross-key isolation).
- `ietf_draft07_test.go` — IETF draft-07 §2.6 (409 on body-hash mismatch; otherwise identical to Stripe replay/wait semantics; no-key pass-through for permitted methods).

Tests share fixtures via `helpers_test.go`. The suite isolates "what each mode actually promises" from `middleware_test.go`'s integration coverage. If draft-08 lands and changes semantics, a new `ietf_draft08_test.go` file is added; the older draft file is preserved for regression tracking until that draft sunsets.

## Known limitations

These are documented because they will eventually need fixing. v0.2 closed two of them (#1, #2); the rest carry into later releases.

### 1. ~~Lock-timeout + reclaim race in `Save`~~ — closed in v0.2

**Resolved by generation tokens** (issue #3, v0.2). Each `Begin` returns a `Token`; `Save` and `Release` require it and refuse to mutate if the entry's current generation doesn't match. The race scenario is now caught and verified by `TestSave_AfterReclaimByOtherCallerReturnsErrTokenMismatch`.

Interface change:

```go
type Store interface {
    Begin(ctx context.Context, key string, bodyHash []byte) (State, *Result, Token, error)
    Save(ctx context.Context, key string, token Token, result *Result) error
    Release(ctx context.Context, key string, token Token) error
}
```

`Token` is `uint64` — zero is "no claim". `Save` with a missing/wrong token returns `ErrTokenMismatch`. `Release` with a missing/wrong token is a noop (idempotent by design).

### 2. ~~`Result` is not defensively cloned~~ — closed in v0.2

**Resolved by `Result.Clone()`** (issue #4, v0.2). `mem.Store` now clones on both input (`Save`) and output (`Begin` / `Wait` of cached results). Caller mutation of returned `Result` or post-`Save` mutation of the input cannot corrupt the cache. Verified by `TestSave_InputClonedSoCallerMutationDoesNotCorruptCache` and `TestBegin_OutputClonedSoCallerMutationDoesNotCorruptCache`.

Cost: two header-clone + body-copy operations per cache miss-and-fill cycle. Re-benchmark in v0.2; expected to be within the existing per-request budget.

### 3. ~~Waiter without `ctx.Deadline` can theoretically block forever~~ — closed in v0.2

**Resolved** by optional `JanitorInterval` in `mem.Config` (v0.2). When set to a positive duration, `New` spawns a background goroutine that periodically calls `purgeExpired` regardless of access patterns — closing waiter channels on expired entries and deleting them. Verified by `TestJanitor_WakesWaiterOnExpiredInFlightWithoutOtherTraffic`.

`Store.Close() error` stops the goroutine cleanly (idempotent via `sync.Once`; noop when the janitor was never started). Recommended pattern: `defer store.Close()` at process shutdown, or `t.Cleanup` in tests.

Default remains `JanitorInterval: 0` (disabled) — preserves the v0.1 zero-goroutine semantics for callers who don't need proactive expiry. The pathology (waiter with no `ctx.Deadline`, no other traffic, janitor disabled) is still possible in that mode; the documented mitigation remains "always pass a bounded `ctx` to `Wait`".

### 4. Same idempotency-key reused across different endpoints produces 422

Path is part of the fingerprint. Two requests with the same key but different paths produce different hashes, and the second is rejected as `ErrBodyMismatch`.

**Workaround:** generate a fresh key per request (recommended anyway — UUIDv7).

**Future consideration:** include path in the storage key alongside scope, making per-endpoint key reuse safe. Currently treated as cosmetic — not blocking.

### 5. No support for `http.Hijacker`, `http.Pusher`, `io.ReaderFrom`

The response writer wrapper does not forward these interfaces. Handlers that need WebSocket upgrades or HTTP/2 server push must bypass `idemkit` via `SkipFunc`. Adding interface-conditional wrappers (the chi pattern) is ~50 LOC of glue code; not in v0.1 scope.

### 6. `Config.Logger` is the only observability hook

No Prometheus metrics, no OpenTelemetry spans. Callers wire those via the `Logger` field or their own middleware. The roadmap deliberately doesn't add observability hooks — keeping the surface tight matters more than feature completeness for a v0.x library.
