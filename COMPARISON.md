# idemkit vs prior art

A side-by-side of `idemkit` and the other Go libraries / sample projects that try to do HTTP idempotency. Last surveyed May 2026.

| Library | Body-hash fingerprint | Wait for in-progress | Postgres | Redis | In-mem | Framework-agnostic | Streaming safe-skip | License | Last release |
|---------|:--:|:--:|:--:|:--:|:--:|:--:|:--:|:--:|:--|
| **idemkit** (this repo) | ✅ length-prefixed | ✅ channel | v0.2 | v0.3 | ✅ | ✅ (`net/http`) | ✅ default | MIT | v0.1-wip |
| [velmie/idempo](https://github.com/velmie/idempo) | ✅ concat | ✅ polling (opt-in) | ❌ | ✅ | ✅ | ✅ (`net/http`) | ❌ | MIT | Dec 2025 |
| [Fiber middleware](https://docs.gofiber.io/api/middleware/idempotency) | ❌ | ❌ (spinlock) | ❌ | ❌ (in-mem by default) | ✅ | ❌ (Fiber) | ❌ | MIT | active |
| [ezraisw/idemgotent](https://github.com/ezraisw/idemgotent) | ❌ | ❌ | ❌ | via `wracha` | ✅ | partial | ❌ | MIT | 2024 |
| [furkandeveloper/idempotency-middleware](https://github.com/FurkanDeveloper/idempotency-middleware) | ❌ | ❌ | ❌ | ✅ | ❌ | ❌ (Echo) | ❌ | MIT | Jan 2025 |
| [go-mizu/idempotency](https://github.com/go-mizu/mizu) | ❌ | ❌ | ❌ | ❌ | ✅ | ❌ (Mizu) | ❌ | MIT | 2025 |
| [dhanapala-id/go-kit/idempotency](https://github.com/dhanapala-id/go-kit) | ❌ | ❌ (409 fail-fast) | ❌ | ✅ | ❌ | ✅ | ❌ | Apache 2 | internal |
| [rafael-piovesan/go-rocket-ride](https://github.com/rafael-piovesan/go-rocket-ride) | ✅ | manual | ✅ | ❌ | ❌ | n/a (sample app) | ❌ | MIT | 2022 |

## Column meaning

- **Body-hash fingerprint** — does the library include the request body in the cache-key identity? Without it, two requests with the same `Idempotency-Key` but different bodies share a cache entry, which is unsafe.
- **Wait for in-progress** — when a duplicate request arrives while the first is still running, does the second wait and replay, or fail / race? Stripe-style semantics require waiting.
- **Postgres / Redis / In-memory** — which backends are supported. `idemkit` ships in-mem in v0.1, Postgres in v0.2, Redis in v0.3.
- **Framework-agnostic** — works with `net/http` directly (so any framework that accepts `http.Handler` adapters can use it). Framework-locked libraries can't be used from outside their ecosystem.
- **Streaming safe-skip** — does the library detect `http.Flusher.Flush()` and skip caching for streaming endpoints? This is the silent foot-gun every implementation should handle but most don't.

## When to use each

**Pick [`velmie/idempo`](https://github.com/velmie/idempo) if:** you need Redis today, you don't need Postgres, and you want fewer abstractions. It's smaller, has shipped, and the body-hash + wait semantics are correct.

**Pick `idemkit` (when v0.2 lands) if:** you need a Postgres backend, want selectable conflict semantics (Stripe vs IETF), or value the streaming safe-skip and explicit threat-model documentation.

**Pick `Fiber middleware` if:** you're already using Fiber. Don't trust it for anything that requires body-hash semantics — it doesn't compare bodies.

**Pick `idemgotent`, `furkandeveloper/idempotency-middleware`, or `go-mizu/idempotency` if:** you're already using their framework AND don't need body-hash semantics. These libraries assume the idempotency key alone is enough, which is true only when clients always send the same body with the same key.

**Don't pick `dhanapala-id/go-kit`** unless you're already using their internal stack. The 409-fail-fast behavior on concurrent duplicates is wrong for most use cases — clients usually want to wait and retry.

**`go-rocket-ride` is a sample app** for Brandur's 2017 blog post, not a library. Reference it for the architecture, don't depend on it.

## Measured performance vs `velmie/idempo`

An apples-to-apples in-memory benchmark lives in [`benchmarks/`](benchmarks/) (separate Go module so the main project keeps zero non-stdlib runtime dependencies). Headline numbers (median of 3 warm runs, Apple M4):

| Scenario | idemkit | velmie | Δ timing | Δ allocations |
|---|--:|--:|---|---|
| Replay (cache hit) | ~1670 ns/op | ~1890 ns/op | idemkit ~12% faster | -5 allocs (~14% fewer) |
| Fresh (claim + Save) | ~4120 ns/op | ~7590 ns/op | idemkit ~46% faster | -21 allocs (~33% fewer) |
| Pass-through | ~2470 ns/op | ~2230 ns/op | tied (within noise) | tied |
| Replay parallel | ~2320 ns/op | ~2800 ns/op | idemkit ~17% faster | -5 allocs |

Full methodology, caveats (including velmie's polling-wait vs idemkit's channel-broadcast), and how to reproduce: see [BENCHMARKS.md](BENCHMARKS.md#comparison-vs-velmieidempo).

Both libraries are fast enough for production. idemkit's measurable advantage in the fresh path comes from channel-based wait + length-prefixed fingerprinting + the unexported response writer. velmie's strong point — Redis backend, shipped — is not yet a competition idemkit enters until v0.3.

## How this list is maintained

This document is updated when a v0.x release ships. If you spot a missing library or an outdated row, file an issue or open a PR. The intent is honest comparison, not marketing — corrections about competing libraries are welcome.
