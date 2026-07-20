# Profile-Guided Optimization

How to collect a profile and build a PGO-optimized binary.

## Quick start

1. Run the proxy in a staging environment
2. `make pgo-collect` — collects a 10s CPU profile via pprof
3. `make pgo-build` — builds with `-pgo=cmd/proxy/default.pgo`

## Expected gains

Go's PGO typically yields 2-7% CPU reduction by inlining and devirtualizing hot paths.
Hottest paths in rplb: balancer selection, middleware chain, response copy.

## CI

The regular `make build` does NOT use PGO (profile not committed). PGO build is opt-in via `make pgo-build`.
