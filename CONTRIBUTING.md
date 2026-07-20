# Contributing

## Setup

Requirements: Go 1.22 or later.

```sh
git clone https://github.com/sanskarpan/Reverse-Proxy-Load-Balancing
cd Reverse-Proxy-Load-Balancing
make build
```

## Running tests

```sh
make test        # unit + integration tests with -race
make bench       # benchmarks
```

Tests require no external services. Redis-backed store tests are skipped automatically when no Redis instance is reachable.

## Commit style

One logical change per commit. Use the following conventional commit prefixes:

- `feat:` — new feature or capability
- `fix:` — bug fix
- `refactor:` — restructuring with no behavior change
- `test:` — tests only
- `docs:` — documentation only
- `chore:` — tooling, CI, dependency updates

Example: `feat: add GCRA algorithm to rate limiter`

## Pull request conventions

- Reference the issue your PR addresses: `Closes #103` or `Part of #103`.
- Include a description of what changed and why.
- Include a test plan: a short bulleted list of scenarios you verified manually or via automated tests.
- Keep each PR focused on one concern; split unrelated changes into separate PRs.

## Label guide

| Label | When to apply |
|---|---|
| `feature` | Adds new user-visible functionality |
| `bug` | Fixes incorrect behavior |
| `performance` | Improves throughput, latency, or resource usage |
| `testing` | Adds or improves test coverage |
| `documentation` | Updates docs, comments, or examples |
| `security` | Addresses a security vulnerability or hardening |

## Code style

- Run `gofmt` before committing; CI enforces it.
- Linters: `staticcheck` and `gosec` only — no additional third-party linters.
- Add comments only when the *why* is non-obvious; skip comments that restate what the code already says.
- Exported symbols must have a godoc comment beginning with the symbol name.
