# idemkit benchmarks

Reproduce locally:

```bash
go test -run=^$ -bench=. -benchmem -benchtime=2s ./...
```

## Setup

| Field | Value |
|-------|-------|
| Hardware | Apple M4 (10-core; 4 P-cores + 6 E-cores) |
| OS / arch | darwin/arm64 |
| Go toolchain | 1.25.6 |
| Mode | `-race` disabled, default GOMAXPROCS, `-benchtime=2s` |

Numbers below are the median of three consecutive runs on a warm system (M4 P-cores in steady-state, post-thermal-rampup). First-run-on-cold-boot numbers are ~1.5–2× faster; sustained-load numbers track these medians. **Expect 30–50% variance run-to-run on this hardware** — see Methodology below.

## Pure-function hot paths

| Benchmark | ns/op | B/op | allocs/op | What it measures |
|---|--:|--:|--:|---|
| `Fingerprint.Canonical` (full payload — query + headers + scope) | ~1100 | 800 | 12 | Length-prefixed canonicalisation with 5-key map sort + 2-header map sort |
| `Fingerprint.Canonical` (minimal — method + path + body) | ~100 | 64 | 1 | The common path: no query, no headers, just method/path/body |
| `DefaultHasher` (small payload) | ~90 | 0 | 0 | SHA-256 over ~50 bytes |
| `DefaultHasher` (1 KiB payload) | ~675 | 0 | 0 | SHA-256 over 1024 bytes |

`DefaultHasher` is zero-alloc because `sha256.Sum256` returns a fixed-size `[32]byte` array; we slice it (`sum[:]`) without copying.

`Canonical` allocates because each map (`Query`, `Headers`) needs a sorted-keys slice, plus the output `bytes.Buffer`. The minimal path skips both maps and allocates only the buffer.

## In-memory store

| Benchmark | ns/op | B/op | allocs/op | What it measures |
|---|--:|--:|--:|---|
| `Begin` (Fresh, unique keys) | ~700 | ~320 | 4 | Mutex acquire + map insert + entry alloc + channel alloc + bodyHash clone |
| `Begin` (Done, single thread) | ~90 | 0 | 0 | Mutex acquire + map lookup + body-hash compare |
| `Begin` (Done, `RunParallel` × 10 cores) | ~225 | 0 | 0 | Same as above under mutex contention |
| `Begin + Save` roundtrip (unique keys) | ~820 | ~320 | 4 | Full Fresh-then-Save cycle |

Notes:
- Cache-hit path (`Begin` Done) is **zero-alloc** — important for high-replay traffic.
- Parallel contention on a single hot key adds ~2.5× latency (90 → 225 ns) but does not allocate. Mutex contention scales gracefully under M4's 10 cores; a busier mutex on x86_64 server hardware may show steeper or flatter degradation.
- Fresh path's allocations: 1 entry struct, 1 `chan struct{}`, 1 bodyHash copy, 1 map-keys growth amortised. Hard to drop further without `sync.Pool`, which would complicate the simple model. Deferred.

## End-to-end middleware (via `httptest`)

These include the cost of `httptest.NewRequest`, `httptest.NewRecorder`, and the handler — they're real-world request shapes, not isolated middleware overhead.

| Benchmark | ns/op | B/op | allocs/op | What it measures |
|---|--:|--:|--:|---|
| `Baseline` (no middleware, handler only) | ~2200 | 6194 | 21 | Reference: just request setup + handler |
| `Replay` (warm cache hit) | ~3600 | 7323 | 30 | Full middleware path → store hit → replay |
| `FreshPerKey` (unique key per iter) | ~5800 | ~8275 | 42 | Full middleware path → claim → handler → Save |
| `PassThrough` no key (POST without `Idempotency-Key`) | ~2400 | 6194 | 21 | Middleware filters out, no-key short-circuit |
| `PassThrough` GET (key set but method not in `Methods`) | ~2300 | 6506 | 21 | Middleware filters out, method short-circuit |
| `Replay` `RunParallel` × 10 cores | ~2850 | 7325 | 30 | Replay under realistic concurrent load |

### Per-request marginal overhead (middleware logic only)

Subtracting the baseline gives the cost of idemkit:

| Scenario | Marginal ns/op | Marginal allocs/op |
|---|--:|--:|
| Replay (cache hit) | **~1400 ns** | +9 |
| Fresh (claim + Save) | **~3600 ns** | +21 |
| Pass-through (no key) | ~200 ns | ~0 |
| Pass-through (GET) | ~100 ns | ~0 |
| Replay parallel | **~650 ns** per goroutine | +9 |

## What this means in practice

- **Replay path is ~1.4 μs marginal overhead.** A service serving 10K rps would spend ~14 ms/sec on idemkit's replay logic — about 1.4% of a CPU core. Negligible.
- **Fresh path is ~3.6 μs marginal overhead.** Same 10K rps service spends ~36 ms/sec — about 3.6% of a core. Cheap relative to the actual handler work (DB calls, serialisation, business logic) which typically dominates.
- **Pass-through is essentially free.** Routes that don't need idempotency don't pay for it. The method-filter and no-key short-circuit are first checks in the pipeline.
- **Parallel replay benefits from concurrency.** Per-goroutine marginal cost on a single hot key drops to ~650 ns under `RunParallel`, suggesting mutex contention is not the bottleneck at this scale; goroutine scheduling and per-request setup dominate.

## Methodology and variance

Apple M4's heterogeneous core architecture (4 P + 6 E cores) and aggressive thermal management produce non-trivial benchmark variance:

- **Cold boot**: numbers ~40–50% lower than steady-state (Replay ~1.5 μs, Baseline ~1 μs).
- **Warm steady-state** (after one full benchmark run): numbers above.
- **Heavy load** (other apps active): numbers up to 2× steady-state.

x86_64 server hardware with proper cooling typically shows tighter variance but slower individual numbers due to lower per-core clock. Re-run on your production hardware for representative figures.

The **relative ratios** between benchmarks (e.g., "Replay is ~1.6× Baseline", "Replay marginal is ~40% of full path") are stable across runs and hardware. The absolute numbers are not.

## Caveats

1. **`httptest` overhead is real.** `BenchmarkMiddleware_Baseline` shows the request-setup cost dominates (~2 μs). Real `net/http` server requests have similar per-request overhead from connection management, header parsing, etc.
2. **Single-instance, in-memory store.** Cross-instance coordination (Postgres in v0.2, Redis in v0.3) will add network round-trip latency to `Begin` / `Save`. The mem-store numbers are the floor.
3. **Map growth.** `BenchmarkBegin_Fresh` and `BenchmarkMiddleware_FreshPerKey` accumulate entries in the store (no TTL eviction during a benchmark). Go's map grows in amortised O(1); very long benchmark runs may show slight ns/op drift as the map resizes. Not material at the iteration counts above.
4. **Allocations include `httptest` machinery.** ~17 of the ~21 allocs/op in the baseline are from `httptest.NewRequest` constructing a `*http.Request` with all its sub-structures (URL, Header map, Body reader, etc.). The remaining ~4 are from the handler. idemkit's own marginal allocs (+9 for replay, +21 for fresh) are layered on top.

## Comparison vs `velmie/idempo`

`velmie/idempo` is the closest comparable Go library — same `net/http` integration model, similar body-hash + wait-for-in-progress semantics, also offers an in-memory store. The `benchmarks/` directory contains an apples-to-apples comparison: identical handler, identical request payload, both libraries with their respective in-memory backends and equivalent configuration (idemkit defaults + velmie with `WithWaitForInProgress(true)` + `WithAllowedResponseHeaders("Content-Type")`).

Reproduce locally:

```bash
cd benchmarks
go test -run=^$ -bench=. -benchmem -benchtime=2s .
```

Median of 3 warm runs on the same hardware:

| Scenario | idemkit ns/op | velmie ns/op | idemkit allocs/op | velmie allocs/op |
|---|--:|--:|--:|--:|
| Replay (cache hit) | ~1670 | ~1890 | **30** | **35** |
| Fresh (claim + handler + Save) | ~4120 | ~7590 | **42** | **63** |
| Pass-through (no `Idempotency-Key`) | ~2470 | ~2230 | 21 | 21 |
| Replay `RunParallel` × 10 cores | ~2320 | ~2800 | 30 | 35 |

| Scenario | Relative timing | Relative allocations |
|---|---|---|
| Replay | idemkit ~12% faster | idemkit -5 allocs (~14% fewer) |
| Fresh | idemkit **~46% faster** | idemkit **-21 allocs (~33% fewer)** |
| Pass-through | velmie ~10% faster | tied |
| Replay parallel | idemkit ~17% faster | idemkit -5 allocs |

### What this means

- **Both libraries are fast enough for production.** The differences are sub-microsecond per request on warm M4. On a 10K rps service, idemkit's advantage in the fresh path saves ~35 ms/sec of CPU — about 3.5% of a core. Real, but not transformative.
- **idemkit's edge is biggest in the fresh path.** This is where length-prefixed fingerprinting + channel-broadcast wait + the unexported `responseWriter` design pay off compared to velmie's polling-based wait and concat fingerprint.
- **Pass-through is essentially tied.** Both libraries short-circuit on the no-key path with minimal overhead. The ~240 ns gap is within run-to-run noise (see Methodology).
- **Allocations are stable across runs.** While ns/op fluctuates 1.5–2× with M4 thermal state, the alloc counts (30 vs 35 replay, 42 vs 63 fresh) don't drift. They are the most reliable metric.

### Caveats specific to this comparison

- **Default configurations are not perfectly equivalent.** velmie defaults to no body-hash fingerprinting; the comparison uses `WithWaitForInProgress(true)` to match idemkit's always-on wait. Other knobs (max body bytes, allowed headers) are configured to match idemkit's defaults as closely as possible.
- **velmie's polling wait** is opt-in (the default is fail-fast on duplicate); with polling enabled, the replay path goes through their poll loop even on warm cache hits. idemkit's channel-based wait short-circuits earlier.
- **No Redis comparison.** velmie's strong point is Redis support, which idemkit will only ship in v0.3. A Redis-vs-Redis comparison will be a v0.3 benchmark.
- **Single-instance only.** Cross-instance coordination changes everything. These numbers are for the in-mem case.

## Not benchmarked yet (deferred to v0.2+)

- `mem.Store` under high lock contention (1000+ concurrent goroutines on one key) — the parallel bench tops out at GOMAXPROCS. A true contention benchmark would use synchronised goroutine fan-out.
- Postgres / Redis stores — they don't exist yet.
- The `Wait` blocking path under concurrent waiters — Go's testing framework doesn't directly support benchmarking blocking calls cleanly. Documented in tests (`TestConcurrentWaiters_AllReceiveSavedResult`) instead.
