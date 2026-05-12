# idemkit

[![CI](https://github.com/polanski13/idemkit/actions/workflows/ci.yml/badge.svg)](https://github.com/polanski13/idemkit/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/polanski13/idemkit.svg)](https://pkg.go.dev/github.com/polanski13/idemkit)

> HTTP idempotency middleware for Go: length-prefixed body fingerprinting, true wait-for-in-progress, and pluggable storage (in-memory + Postgres).

> **Released as v0.2.0.** 0.x semver — API may shift before v1.0; pin to a specific version in production. See [CHANGELOG.md](CHANGELOG.md) for what changed since v0.1.0 (notably: generation tokens broke the `Store` interface; existing v0.1 custom backends need updating).

## What it does

`idemkit` is a `net/http` middleware that gives any Go service idempotent `POST` / `PUT` / `PATCH` / `DELETE`. Same `Idempotency-Key` + same body → cached replay. Same key + different body → 422 conflict (Stripe-style, configurable to IETF 409). Concurrent duplicates → second waits for the first.

Shipping in v0.2:

- `net/http` middleware
- **In-memory `Store`** (single-instance, race-safe, zero non-stdlib deps) with optional proactive expiry janitor
- **Postgres `Store`** (`store/pg`, pgx/v5 — cross-instance coordination via `INSERT ... ON CONFLICT` + row-based reclaim, polling `Wait`)
- Length-prefixed request fingerprinting (method + path + query + body)
- Streaming safe-skip on `http.Flusher` — the silent foot-gun every prior library misses
- `MaxRequestBytes` / `MaxResponseBytes` caps
- `KeyScope` for tenant isolation
- **Generation tokens** for safe `Save` under lock-timeout-reclaim race
- **`Result.Clone()`** — defensive copying at store boundaries
- Stripe-style 422 (`ConflictStripe`, default) or IETF draft-07 §2.6 409 (`ConflictIETF`) on body mismatch, both configurable via `OnConflict`
- Conformance test suite documenting each mode's contract

Out of scope for v0.2, see [Roadmap](#roadmap): Redis store, opt-in LISTEN/NOTIFY for Postgres.

## Install

```bash
go get github.com/polanski13/idemkit
```

Requires Go 1.25 or later. The core `idemkit` package and `store/mem` have zero non-stdlib runtime dependencies. Backend subpackages (`store/pg`) bring their own driver as a direct dep (`github.com/jackc/pgx/v5`); users who only need in-memory storage don't link it.

## Quickstart

```go
package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/polanski13/idemkit"
	"github.com/polanski13/idemkit/store/mem"
)

func main() {
	store := mem.New(mem.Config{
		TTL:         time.Hour,
		LockTimeout: 30 * time.Second,
	})

	mw := idemkit.Middleware(store, idemkit.Config{})

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":"ch_%d","status":"succeeded"}`, time.Now().UnixNano())
	})

	http.Handle("/v1/charges", mw(h))
	log.Fatal(http.ListenAndServe(":8080", nil))
}
```

Test it:

```bash
curl -X POST -i -H "Idempotency-Key: ch_001" http://localhost:8080/v1/charges
# 201 Created
# {"id":"ch_1715533101...","status":"succeeded"}

curl -X POST -i -H "Idempotency-Key: ch_001" http://localhost:8080/v1/charges
# Same ID. Response includes header:  X-Idemkit-Replayed: true
```

Full runnable example: [examples/nethttp/main.go](examples/nethttp/main.go).

## Quickstart (chi)

A runnable chi example lives in [examples/chi](examples/chi). It uses a separate Go module so chi stays out of the core `idemkit` dependency graph.

```bash
cd examples/chi
go run .
```

In another terminal, send the same idempotency key and body twice:

```bash
curl -i -X POST http://localhost:8080/v1/charges \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: ch_001" \
  -H "X-Tenant-ID: tenant_a" \
  -d '{"amount":1000}'
# 201 Created
# {"id":"ch_1715533101...","status":"succeeded"}

curl -i -X POST http://localhost:8080/v1/charges \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: ch_001" \
  -H "X-Tenant-ID: tenant_a" \
  -d '{"amount":1000}'
# Same ID. Response includes header:  X-Idemkit-Replayed: true
```

A replay with the same key but a different body returns the default Stripe-style conflict:

```bash
curl -i -X POST http://localhost:8080/v1/charges \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: ch_001" \
  -H "X-Tenant-ID: tenant_a" \
  -d '{"amount":2000}'
# 422 Unprocessable Entity
# idemkit: idempotency-key conflict (body_mismatch)
```

`X-Tenant-ID` is read by the example's fake auth middleware and passed to `Config.KeyScope`, isolating identical keys per tenant.

## Quickstart (Postgres)

```go
package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/polanski13/idemkit"
	"github.com/polanski13/idemkit/store/pg"
)

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://user:pass@localhost:5432/app?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	if err := pg.ApplySchema(ctx, pool); err != nil {
		log.Fatal(err)
	}

	store := pg.New(pool, pg.Config{
		TTL:          24 * time.Hour,
		LockTimeout:  30 * time.Second,
		PollInterval: 100 * time.Millisecond,
	})
	mw := idemkit.Middleware(store, idemkit.Config{})

	http.Handle("/v1/charges", mw(handler()))
	log.Fatal(http.ListenAndServe(":8080", nil))
}
```

The schema is in [store/pg/schema.sql](store/pg/schema.sql). `ApplySchema` is idempotent (uses `IF NOT EXISTS`); production deployments typically run it via a migration tool (goose, atlas, sqlx-migrate) instead of at startup.

`Wait` is polling-based in v0.2 (default 100 ms). `LISTEN/NOTIFY` for instant wakeup lands in v0.3 as opt-in.

## Security and threat model

**Idempotency keys arrive in a client-supplied header. Treat them as untrusted.**

### 1. Storage DoS

A malicious or buggy client can flood the store with distinct keys, growing memory until TTL expires entries. Mitigations the library exposes:

- `MaxRequestBytes` caps per-request memory (1 MiB default).
- `TTL` bounds entry lifetime (24h default).
- **Rate-limit upstream** of this middleware by source IP or authenticated principal — `idemkit` does not.

### 2. Cross-tenant key replay

Two users submitting the same key + same body to the same endpoint would otherwise share a cache entry — user B could see user A's response. To prevent:

```go
cfg := idemkit.Config{
	KeyScope: func(r *http.Request) string {
		return userIDFromContext(r.Context())
	},
}
```

`KeyScope` is folded into the storage key, namespacing cache entries per principal. Without it, the cache namespace is global per endpoint.

### 3. Key length and content

`idemkit` does not validate keys in v0.1. Recommended (not enforced): UUIDv7 or similar opaque identifier, max 255 bytes. Wire validation via a custom `KeyExtractor` if you need stricter rules.

## How it works

```
   Begin(key, hash) ─► Fresh    ─► run handler ─► Save / Release
                  ├─► InFlight  ─► Wait        ─► replay (or retry on release)
                  └─► Done      ─► replay
```

- **First request**: middleware claims the key (`Begin` returns `Fresh`), runs your handler, captures the response, calls `Save`.
- **Duplicate, same body**: middleware sees `Done` and replays the cached response with `X-Idemkit-Replayed: true`.
- **Concurrent duplicate**: middleware sees `InFlight`, blocks in `Wait`, then replays the result.
- **Same key, different body**: 422 Unprocessable Entity (Stripe default; override via `OnConflict`).

### Fingerprint composition

SHA-256 over **length-prefixed framing** of:

1. Method
2. Path
3. Canonicalised query (keys sorted, values sorted within each key)
4. Body bytes
5. Optional selected headers (default: none)
6. Optional `KeyScope` value (when used directly, not via middleware)

Length-prefixing prevents boundary-ambiguity collisions like `(method="POST", path="/foo")` vs `(method="POS", path="T/foo")` that affect concat-with-separator schemes used by other libraries.

### Streaming endpoints

If a handler exercises `http.Flusher.Flush()` or writes more than `MaxResponseBytes`, the response is marked uncacheable and passed through to the client unchanged. SSE, long-polling, and large file downloads work transparently — they just aren't cached.

For known-streaming endpoints, opt out explicitly:

```go
cfg.SkipFunc = func(r *http.Request) bool {
	return r.URL.Path == "/v1/events" // SSE stream
}
```

## Configuration

```go
type Config struct {
	Header            string                                          // default: "Idempotency-Key"
	TTL               time.Duration                                   // default: 24h
	LockTimeout       time.Duration                                   // default: 30s
	Methods           []string                                        // default: POST, PUT, PATCH, DELETE
	MaxRequestBytes   int64                                           // default: 1 MiB
	MaxResponseBytes  int64                                           // default: 1 MiB
	CacheServerErrors bool                                            // default: false (skip 5xx)
	ConflictMode      ConflictMode                                    // default: ConflictStripe
	Hasher            func([]byte) []byte                             // default: SHA-256
	KeyExtractor      func(r *http.Request) (string, error)           // default: reads Header
	KeyScope          func(r *http.Request) string                    // optional: tenant prefix
	SkipFunc          func(r *http.Request) bool                      // optional: opt-out predicate
	OnConflict        func(http.ResponseWriter, *http.Request, ConflictReason)
	Logger            *slog.Logger                                    // default: slog.Default()
}
```

Zero values are replaced with defaults at `Middleware` construction time. To opt into caching 5xx responses, set `CacheServerErrors: true`.

## Backends

| Backend | Status | Package | Use case |
|---------|--------|---------|----------|
| In-memory | v0.1 | `github.com/polanski13/idemkit/store/mem` | Tests, single-instance deployments |
| Postgres | ✅ v0.2 | `github.com/polanski13/idemkit/store/pg` | Production, cross-instance coordination |
| Redis | planned v0.3 | `…/store/redis` | Production, high-throughput caching |
| Custom | always | implement `idemkit.Store` | Anything else |

## Conformance

- **Stripe semantics** (default, `ConflictMode: ConflictStripe`) — 422 Unprocessable Entity on body-hash mismatch, replay on match, wait on concurrent duplicate. Tested in [`internal/conformance/stripe_test.go`](internal/conformance/stripe_test.go).
- **IETF `draft-ietf-httpapi-idempotency-key-header-07`** (`ConflictMode: ConflictIETF`) — 409 Conflict on body-hash mismatch per §2.6, otherwise identical replay/wait semantics. Tested in [`internal/conformance/ietf_draft07_test.go`](internal/conformance/ietf_draft07_test.go). What's not (yet) implemented from the draft: RFC 7807 Problem Details response bodies, per-method validation. Status-code conformance is the substantive difference; the rest of the middleware (Methods filter, KeyScope, replay headers) is mode-independent and equally compliant under either mode.

## FAQ

**Q: Why not just use a Postgres unique constraint?**
A: That covers "second request fails", but not "second request waits for the first" or "second request replays the cached response". `idemkit` does all three.

**Q: What happens on server crash mid-request?**
A: The in-flight claim is held until `LockTimeout` (30s default) expires, after which the entry is reclaimable by the next caller. For the in-memory store, the entire entry is lost on process restart — appropriate for in-mem (no stale claim survives).

**Q: Can the same key be reused across endpoints?**
A: Not in v0.1 — path is part of the request fingerprint, so reusing a key with a different path produces a 422 conflict. Best practice: generate a unique key per request (UUIDv7 is recommended).

**Q: Can I cache 5xx responses?**
A: Off by default (`CacheServerErrors: false`). 5xx replays can mask transient infrastructure errors; safer to let clients retry. Set `CacheServerErrors: true` to opt in.

**Q: How big is the perf overhead?**
A: Replay path adds ~1.4 μs marginal overhead vs no middleware; fresh path (claim + handler + Save) adds ~3.6 μs. Pass-through routes (wrong method or no key) cost essentially nothing. On a 10K rps service that's about 1.4% of a CPU core for replays and 3.6% for fresh claims — typically dwarfed by actual handler work. Numbers are Apple M4 steady-state; expect tighter variance on dedicated server hardware. See [BENCHMARKS.md](BENCHMARKS.md) for the full breakdown, methodology, and reproduction commands.

**Q: What about SSE / streaming endpoints?**
A: `idemkit` detects `http.Flusher.Flush()` and silently skips caching. Pass-through to the client is unaffected. For endpoints you know upfront should bypass caching (file downloads, WebSocket upgrades), use `SkipFunc`.

**Q: How is this different from `velmie/idempo`?**
A: See [COMPARISON.md](COMPARISON.md). Short version: `idemkit` adds Postgres-first design (v0.2), pub/sub-coordinated wait (v0.3), selectable conflict semantics, streaming safe-skip in the v0.1 default, and an explicit threat-model section. `velmie/idempo` is Redis-only with polling wait, but is also smaller, simpler, and has shipped.

## Roadmap

| Version | Adds | Status |
|---------|------|--------|
| v0.1.0 | `net/http` middleware, in-mem store, fingerprinting, Stripe conflict, streaming safe-skip, threat model | ✅ released |
| v0.2.0 | Postgres store, IETF conflict mode, chi example, conformance test suite, generation tokens, `Result.Clone()`, optional janitor for in-mem | ✅ released |
| v0.3 | Redis store, opt-in LISTEN/NOTIFY (Postgres), opt-in Redis pub/sub | planned |
| v1.0 | Stable API, semver guarantees | planned |

Out of scope for v1.0: Prometheus / OpenTelemetry hooks (use the `Logger` field and your own observability stack), request-body streaming, custom codecs.

## Design and architecture

See [DESIGN.md](DESIGN.md) for architectural decisions, deviations from the original plan, and named limitations.

## License

MIT — see [LICENSE](LICENSE).

## Acknowledgments

- **[Brandur Leach](https://brandur.org/idempotency-keys)** — the original 2017 blueprint that everyone re-implements.
- **[Stripe API docs](https://stripe.com/docs/api/idempotent_requests)** — for the 422-on-mismatch convention that `idemkit` follows by default.
- **[IETF httpapi WG](https://datatracker.ietf.org/doc/draft-ietf-httpapi-idempotency-key-header/)** — for finally drafting a spec.
- **[velmie/idempo](https://github.com/velmie/idempo)** — prior art whose existence made `idemkit`'s differentiation crisp.
