## Summary

<!-- One or two sentences describing what this PR does and why. -->

## Type

<!-- Mark with [x] -->

- [ ] Bug fix
- [ ] New feature
- [ ] Documentation
- [ ] Refactor / cleanup
- [ ] Performance
- [ ] Build / CI

## Linked issue

<!-- e.g. closes #42 -->

## Checklist

- [ ] `gofmt -l .` produces no output
- [ ] `go vet ./...` clean
- [ ] `go test -count=1 -race ./...` passes locally
- [ ] No new non-stdlib runtime dependencies (or, if added, justification in the PR description)
- [ ] Public API changes documented in README / DESIGN.md
- [ ] If performance-relevant: `BENCHMARKS.md` updated with measurements from the `benchmarks/` submodule
- [ ] If new `Store` backend: implements `idemkit.Store` with a passing race-tested test suite under `store/<name>/`
- [ ] No code comments (project convention — test names and clear identifiers carry intent)
