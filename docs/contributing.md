# Contributing

Thank you for considering a contribution to rplb. This document covers setup, testing requirements, commit conventions, and PR process.

---

## Setup

Requirements: Go 1.21 or later. No other runtime dependencies for the core build.

```bash
git clone https://github.com/sanskarpan/Reverse-Proxy-Load-Balancing
cd Reverse-Proxy-Load-Balancing
make build     # produces ./bin/proxy
make test      # unit + integration tests with -race
```

Optional — only needed for Redis-backed tests:

```bash
docker run -d --name redis -p 6379:6379 redis:7-alpine
# Tests detect Redis automatically and enable Redis-backed store tests
```

---

## Running tests

### Unit and integration tests

```bash
make test          # go test -race ./...
make test-race     # explicit alias for the above
```

All tests must pass with `-race` enabled. The race detector catches real bugs — do not suppress races with workarounds.

Tests that require external services (Redis, Pebble ACME server) detect their availability automatically and skip gracefully when the service is not running. CI runs without external services; these tests are run manually before merging features that touch Redis or ACME.

### Coverage gate

```bash
make cover         # runs tests and opens coverage report
```

The project maintains a **75% coverage gate**. New code should maintain or improve coverage. The CI pipeline reports coverage as a check; PRs that drop coverage below 75% require justification.

Generated code, adapter glue, and `main.go` are excluded from the coverage calculation.

### Benchmarks

```bash
make bench         # go test -bench=. -benchmem ./...
```

Run benchmarks before and after changes that touch the hot path (balancer selection, middleware chain, response copy). Report results in the PR description if the benchmark result changes significantly.

Reference benchmark (14-core M3 Max, Go 1.26):

```
BenchmarkConsistentHash/64-backends-14     105,247,301    11.33 ns/op    0 B/op    0 allocs/op
BenchmarkP2C/64-backends-14                198,432,001     5.91 ns/op    0 B/op    0 allocs/op
BenchmarkRoundRobin/64-backends-14         632,110,450     1.89 ns/op    0 B/op    0 allocs/op
```

### Load testing

```bash
make loadtest      # runs test/loadtest/ against a local binary
```

Asserts: ≥ 99% success rate and p99 latency < 500ms under the load test profile.

### Chaos testing

```bash
make chaos         # 5 chaos scenarios: slow/flapping/total-failure/50%-error/connection-reset
```

---

## Fuzz targets

The project includes fuzz targets for security-sensitive parsing paths:

```bash
# Run fuzzing for 60 seconds on the config parser
go test -fuzz=FuzzConfigLoad -fuzztime=60s ./internal/config/

# Run fuzzing on the client IP extractor (security-critical)
go test -fuzz=FuzzExtractClientIP -fuzztime=60s ./internal/netutil/
```

---

## Linting

```bash
make lint          # runs staticcheck and gosec
make fmt           # gofmt -w ./...
make vet           # go vet ./...
```

Only two linters are used:
- `staticcheck` — catches real bugs, deprecated API usage, and style issues that matter.
- `gosec` — catches security anti-patterns (SQL injection, use of weak crypto, etc.).

No additional linters. The project explicitly does not use exhaustive `golangci-lint` configurations — linter noise reduces the signal-to-noise ratio of CI feedback.

### Lint suppressions

Use `//nolint:` directives only for known false positives. Each suppression must include a comment explaining why it is a false positive:

```go
// gosec G402: TLS InsecureSkipVerify is only set when config.InsecureSkipVerify is true,
// which is validated at config load to require an explicit opt-in.
//#nosec G402
tlsConfig.InsecureSkipVerify = cfg.InsecureSkipVerify
```

---

## Commit style

One logical change per commit. Use conventional commit prefixes:

| Prefix | When to use |
|--------|------------|
| `feat:` | New user-visible feature or capability |
| `fix:` | Bug fix |
| `refactor:` | Restructuring with no behavior change |
| `test:` | Tests only (no production code change) |
| `docs:` | Documentation only |
| `chore:` | Tooling, CI, dependency updates, non-functional changes |
| `perf:` | Performance improvement (no behavior change) |

Examples:

```
feat: add GCRA algorithm to rate limiter
fix: prevent reservation leak in hedged requests
perf: replace per-request map alloc with sync.Pool in consistent hash
test: add chi-squared uniformity test for weighted_random
docs: add ADR for bounded-load consistent hashing
```

Commit messages should explain the **why**, not just the what. The diff already shows what changed.

---

## Pull request process

### Before opening a PR

1. Run `make test-race` — all tests must pass.
2. Run `make lint` — no new lint issues.
3. Run `make bench -benchmem` if your change touches the hot path — report results.
4. Validate config examples: `./bin/proxy --validate --config configs/config.yaml`.

### PR description

Include:
- **What changed:** brief summary.
- **Why:** the motivation or bug being fixed.
- **Test plan:** a bulleted list of scenarios you verified, either via automated tests or manually.
- **Benchmark delta:** if the change affects performance.

Reference the issue your PR addresses: `Closes #103` or `Part of #103`.

### One concern per PR

Keep each PR focused on one concern. Split unrelated changes into separate PRs. This makes review faster and keeps the git history readable.

### PR size guideline

| Category | Guideline |
|----------|-----------|
| Bug fix | < 100 lines changed |
| Feature | < 500 lines changed |
| Large feature | Split into multiple PRs with a tracking issue |

Very large PRs slow down review and increase the risk of introducing regressions. If a feature is large, open a tracking issue and implement it in phases.

---

## Label guide

| Label | When to apply |
|-------|--------------|
| `feature` | Adds new user-visible functionality |
| `bug` | Fixes incorrect behavior |
| `performance` | Improves throughput, latency, or resource usage |
| `testing` | Adds or improves test coverage |
| `documentation` | Updates docs, comments, or examples |
| `security` | Addresses a security vulnerability or hardening |

---

## Code style

- Run `gofmt` before committing; CI enforces it.
- Exported symbols must have a godoc comment beginning with the symbol name.
- Add comments only when the *why* is non-obvious; skip comments that restate what the code already says.
- Prefer table-driven tests for functions with multiple input/output cases.
- Use `t.Helper()` in test helper functions so failure lines point to the test, not the helper.
- Avoid `t.Fatal` inside goroutines — use channels or `sync/errgroup` to propagate errors back to the test goroutine.

---

## Good first issues

Look for issues labeled `good-first-issue` in the GitHub issue tracker. Typical good first issues:

- Adding a missing field to the configuration reference.
- Writing a benchmark for an existing function.
- Adding a test case to an existing table-driven test.
- Improving an error message to be more actionable.

For larger contributions, open an issue to discuss the design before writing code. This avoids wasted effort if the approach does not align with the project direction.
