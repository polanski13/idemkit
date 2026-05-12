# Changelog

All notable changes to `idemkit` are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Under 0.x the API is considered unstable; minor releases may include breaking changes that are called out below.

## [0.2.0] — 2026-05-12

### Added

- **Postgres backend** (`github.com/polanski13/idemkit/store/pg`) — implements `idemkit.Store` via `pgx/v5`. Atomic claim through `INSERT ... ON CONFLICT DO NOTHING RETURNING token`; row-based reclaim on lock-timeout or TTL expiry via `SELECT ... FOR UPDATE`; polling-based `Wait` (`LISTEN/NOTIFY` deferred to v0.3 as opt-in). Schema in [`store/pg/schema.sql`](store/pg/schema.sql), embedded via `//go:embed`. `ApplySchema` is idempotent (`IF NOT EXISTS`).
- **Generation tokens** (`idemkit.Token`, `idemkit.ErrTokenMismatch`) — `Begin` now returns a non-zero `Token` on `StateFresh`; `Save` and `Release` require the token. `Save` with a stale token returns `ErrTokenMismatch`; `Release` with a stale token is a noop. Closes the lock-timeout-reclaim race documented in DESIGN.md.
- **`Result.Clone()`** — deep copy of a `Result` (StatusCode + cloned Header + copied Body). `mem.Store` now clones on both input (`Save`) and output (`Begin` / `Wait` of cached results); caller mutation cannot corrupt the cache.
- **`ConflictMode: ConflictIETF`** — returns 409 Conflict on body-hash mismatch per `draft-ietf-httpapi-idempotency-key-header-07 §2.6`. `ConflictStripe` remains the default (422 Unprocessable Entity).
- **`internal/conformance/`** — separate test files per spec (`stripe_test.go`, `ietf_draft07_test.go`) documenting each mode's contract as a standalone spec. Shared fixtures in `helpers_test.go`.
- **`examples/chi/`** — runnable chi router example with `KeyScope`-based tenant isolation via `X-Tenant-ID` header. Separate Go module keeps chi out of the core dep graph.
- **`mem.Config.JanitorInterval`** + **`mem.Store.Close()`** — optional background goroutine for proactive expiry. Closes waiter channels on expired entries regardless of access patterns. `Close()` stops the goroutine cleanly (idempotent via `sync.Once`). Default `JanitorInterval: 0` preserves v0.1's zero-goroutine semantics.
- **Postgres benchmarks** (`BenchmarkPG_*`) and a new "Postgres store" section in BENCHMARKS.md with measured round-trip costs.

### Changed (breaking)

- **`idemkit.Store` interface signature**: `Begin` now returns `(State, *Result, Token, error)` (added `Token`). `Save` and `Release` take a `Token` parameter. Anyone implementing a custom `Store` will need to update method signatures. Within 0.x semver, breaking changes are explicit.
- **Min Go version bumped to 1.25** (was 1.22 in v0.1.0). Go 1.22 is N-4 and EOL; the project uses no 1.22-specific features.

### Documented

- DESIGN.md "Known limitations" #1, #2, #3 marked closed in v0.2:
  - #1 Lock-timeout + reclaim race — resolved by tokens
  - #2 `Result` not defensively cloned — resolved by `Result.Clone()`
  - #3 Waiter without `ctx.Deadline` blocks forever — resolved by optional janitor
- New "Postgres store" architecture section explaining transactional Begin flow, sequence-based tokens, polling-Wait rationale, and why no advisory locks.
- New "Conflict semantics" section enumerating mode-specific status codes and what's not yet covered from the IETF draft (RFC 7807 Problem Details, per-method validation).
- README quickstarts for Postgres and chi.

### Closed issues

- #1 Implement Postgres store
- #2 Implement `ConflictMode: ConflictIETF`
- #3 Generation tokens for safe `Save` under lock-timeout race
- #4 Add `Result.Clone()` for defensive copying
- #5 Add chi router example (via [PR #8](https://github.com/polanski13/idemkit/pull/8) from `@nightcityblade`)
- #6 Add `internal/conformance/` test suite (Stripe + IETF)
- #7 Optional `JanitorInterval` for proactive expiry in `mem.Store`

### Dependencies

- Added `github.com/jackc/pgx/v5` as a direct dependency. Only users importing `store/pg` link it; `idemkit` core and `store/mem` remain stdlib-only.

## [0.1.0] — 2026-05-12

### Added

Initial release.

- `net/http` middleware via `idemkit.Middleware(store, cfg)`.
- In-memory `Store` (`store/mem`) — single-instance, race-safe, zero non-stdlib deps.
- Length-prefixed body-hash fingerprinting (method + path + query + body).
- Streaming safe-skip on `http.Flusher.Flush()`.
- Stripe-style 422 on body-hash mismatch (configurable via `OnConflict`).
- `KeyScope` for tenant isolation via storage-key prefix.
- `MaxRequestBytes` / `MaxResponseBytes` caps (1 MiB defaults).
- 84+ tests under `-race`; 2M+ fuzz executions clean.
- GitHub Actions CI (gofmt + vet + build + race tests on Go 1.25 and stable; 15s fuzz smoke).
- README with quickstart, threat model, FAQ, roadmap.
- DESIGN.md with architecture decisions, plan deviations, named limitations.
- COMPARISON.md vs eight prior-art Go libraries.
- BENCHMARKS.md with measured per-request overhead and methodology.
- Apples-to-apples benchmark vs `velmie/idempo` in `benchmarks/`.
