# idemkit benchmarks

Apples-to-apples performance comparison between `idemkit` and [`velmie/idempo`](https://github.com/velmie/idempo) — the closest Go-ecosystem comparable.

This directory is a **separate Go module** so the main project's `go.mod` does not pull in `velmie/idempo` as a dependency.

## Run

```bash
cd benchmarks
go test -run=^$ -bench=. -benchmem -benchtime=2s .
```

For a longer, more stable measurement:

```bash
go test -run=^$ -bench=. -benchmem -benchtime=10s -count=3 . | tee results.txt
```

## What's compared

Same `net/http` handler wrapped by each library with their respective in-memory stores:

- **idemkit**: `idemkit.Middleware(mem.New(mem.Config{}), idemkit.Config{})`
- **velmie**: `middleware.Middleware(WithEngine(idempo.NewEngine(memory.New(), idempo.WithWaitForInProgress(true))), WithAllowedResponseHeaders("Content-Type"))`

Benchmarked scenarios:

- **Replay** — cache hit path (same key, same body, repeated)
- **Fresh** — unique key per iteration (full claim + handler + Save cycle)
- **Pass-through** — no `Idempotency-Key` header (middleware should short-circuit)
- **Replay parallel** — replay path under `RunParallel` × GOMAXPROCS

## Results

See the [Comparison vs velmie/idempo](../BENCHMARKS.md#comparison-vs-velmieidempo) section in the main `BENCHMARKS.md`.

## Why a separate module

Pulling `velmie/idempo` into the main `go.mod` would force every `idemkit` user to download a competing library at `go get` time. Keeping the comparison in `benchmarks/` with its own `go.mod` keeps `idemkit`'s public dependency surface zero-stdlib-deps, while still letting maintainers verify the comparison locally.

The module uses a `replace` directive to point at the parent `idemkit` package, so benchmark numbers always reflect the local working copy:

```go
replace github.com/polanski13/idemkit => ..
```
