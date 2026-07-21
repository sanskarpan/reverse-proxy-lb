# Production Gap TODO

Gaps identified after the full ENHANCEMENTS.md implementation pass.
Each item has a GitHub issue with full subtask breakdown; this file is the
sequencing guide. Items are ordered by impact-to-effort ratio.

---

## Tier 1 — Quick correctness wins (S effort)

### #121 · Fix Go toolchain pin in CI
**Issue:** [#121](https://github.com/sanskarpan/reverse-proxy-lb/issues/121)

`go-version: "1.26"` is a non-existent release. Builds resolve to whatever
`setup-go` returns, making them non-reproducible.

- [ ] Pin `go-version` to `"1.24"` in `.github/workflows/ci.yml`
- [ ] Align `go.mod` `go` directive
- [ ] Verify CI passes

---

### #122 · Seed math/rand from crypto/rand
**Issue:** [#122](https://github.com/sanskarpan/reverse-proxy-lb/issues/122)

Five RNG instances seeded with `time.Now().UnixNano()` collide when replicas
start simultaneously — all send traffic to the same backend.

- [ ] Add `internal/randutil/randutil.go` → `func SecureSeed() int64`
- [ ] Replace `time.Now().UnixNano()` in `p2c.go`, `consistenthash.go`,
  `weightedrandom.go`, `wrappers.go`, `proxy.go`
- [ ] Unit test: two instances with same wall-clock produce different sequences

---

### #126 · Slowloris & oversized-header protection
**Issue:** [#126](https://github.com/sanskarpan/reverse-proxy-lb/issues/126)

No `ReadHeaderTimeout` or `MaxHeaderBytes` set — process is vulnerable to
connection exhaustion.

- [ ] Set `http.Server.ReadHeaderTimeout` from config (default 10s)
- [ ] Set `http.Server.MaxHeaderBytes` from config (default 64 KiB)
- [ ] Add optional `MaxBodyBytes` middleware (413 on exceeded)
- [ ] Config fields + validation
- [ ] E2E tests: slowloris (slow headers), oversized headers (431), oversized body (413)

---

## Tier 2 — Performance (S–M effort)

### #123 · Pool captureWriter / errCapture allocations
**Issue:** [#123](https://github.com/sanskarpan/reverse-proxy-lb/issues/123)

Per-request heap allocations for wrapper structs; major GC pressure at scale.

- [ ] `sync.Pool` for `captureWriter` in `proxy.go`
- [ ] `sync.Pool` for `errCapture` in `proxy.go`
- [ ] Benchmark before/after (`-benchmem`)
- [ ] `AllocsPerRun` assertion to lock in zero-alloc

---

### #124 · Zero-alloc ConsistentHash.NextForKey
**Issue:** [#124](https://github.com/sanskarpan/reverse-proxy-lb/issues/124)

`seen` map allocated per-call; dominant allocator in consistent-hash workloads.

- [ ] Profile to confirm (memprofile)
- [ ] Replace per-call map with pooled scratch or sorted-slice linear search
- [ ] Benchmark both; pick faster
- [ ] `AllocsPerRun` assertion for `NextForKey`

---

## Tier 3 — Production resilience (M effort)

### #125 · Global admission ceiling (max in-flight)
**Issue:** [#125](https://github.com/sanskarpan/reverse-proxy-lb/issues/125)

No global goroutine cap; proxy can OOM under extreme load before per-backend
`MaxConns` fires.

- [ ] `MaxInflightRequests`, `MaxInflightQueue`, `QueueTimeout` in `ServerConfig`
- [ ] `internal/middleware/admission.go` (semaphore-based gate)
- [ ] Wire before routing in handler chain
- [ ] `rplb_admissions_rejected_total` counter + `rplb_queue_depth` gauge
- [ ] E2E: saturate beyond limit → assert 503 + counter
- [ ] E2E: queue drains within timeout
- [ ] Goroutine leak check under sustained overload

---

### #131 · Canary auto-promote / rollback
**Issue:** [#131](https://github.com/sanskarpan/reverse-proxy-lb/issues/131)

Static canary weight requires manual operator changes; production needs
automatic promotion on healthy canary and rollback on degradation.

- [ ] `AutoPromoteConfig` in `CanaryConfig`
- [ ] `internal/canary/autopromote.go` (background step loop)
- [ ] Admin API: `GET /admin/canary/status`
- [ ] E2E: injected errors → rollback within one step interval
- [ ] E2E: healthy canary → reaches max weight
- [ ] `rplb_canary_weight` gauge + `rplb_canary_rollback_total` counter

---

### #132 · Raise coverage gate to 75%
**Issue:** [#132](https://github.com/sanskarpan/reverse-proxy-lb/issues/132)

60% gate leaves critical proxy-core paths unverified.

- [ ] Identify top 10 uncovered functions in `proxy.go`
- [ ] Fill: retry exhaustion, circuit-open mid-retry, hedge win, WS upgrade,
  canary concurrency, all security-header branches, routing fallbacks
- [ ] Raise CI gate from `60` to `75`
- [ ] Update `CONTRIBUTING.md`

---

## Tier 4 — Auth & TLS completeness (M–L effort)

### #127 · ACME end-to-end with Pebble + sanskarpan.xyz
**Issue:** [#127](https://github.com/sanskarpan/reverse-proxy-lb/issues/127)

ACME wiring exists but has never issued a real cert. Pebble provides a
CI-runnable ACME server.

- [ ] Integration test using Pebble (`//go:build integration`): issue → serve → renew
- [ ] `configs/config.acme.yaml` for sanskarpan.xyz with LE staging
- [ ] `docs/ACME.md`: DNS prerequisites, HTTP-01 port-80 requirement, staging→prod
- [ ] CI Pebble sidecar (docker-compose or test helper binary)

---

### #128 · OAuth2/OIDC token introspection
**Issue:** [#128](https://github.com/sanskarpan/reverse-proxy-lb/issues/128)

Opaque token validation against RFC 7662 introspection endpoint. Required for
enterprise deployments that don't issue JWTs.

- [ ] `OIDCIntrospectionConfig` in `AuthConfig`
- [ ] `internal/middleware/oidc_introspect.go`: POST to introspection endpoint,
  parse RFC 7662, LRU cache with TTL
- [ ] Negative-cache for bad tokens (short TTL)
- [ ] E2E: stub introspection server; valid/inactive/expired/network-error cases
- [ ] `rplb_oidc_introspection_total{result}` counter

---

## Tier 5 — Distributed systems (L effort)

### #129 · Kubernetes EndpointSlice service discovery
**Issue:** [#129](https://github.com/sanskarpan/reverse-proxy-lb/issues/129)

DNS discovery lags 10–30s in k8s; EndpointSlice informers update within
milliseconds of pod readiness changes.

- [ ] `KubernetesDiscoveryConfig` in `DiscoveryConfig`
- [ ] `internal/discovery/k8s.go` with `client-go` EndpointSlice informer
- [ ] In-cluster + kubeconfig auth
- [ ] Unit tests with `k8s.io/client-go/testing` fake client
- [ ] RBAC docs + Helm chart ServiceAccount/ClusterRole
- [ ] `docs/KUBERNETES.md`

---

### #130 · Distributed circuit-breaker state via Redis
**Issue:** [#130](https://github.com/sanskarpan/reverse-proxy-lb/issues/130)

Per-replica circuit state means only 1/N replicas protect a tripped backend.
Redis shared counters make all replicas trip and recover together.

- [ ] `SharedStateConfig` in `CircuitBreakerConfig`
- [ ] `internal/circuitbreaker/redis_state.go` with Lua atomic scripts
- [ ] Async sync (local hot path unchanged, Redis on state change + periodic)
- [ ] Fallback to local-only on Redis loss (no panic)
- [ ] Integration test: two CB instances + Redis; trip via one, assert both OPEN
- [ ] Unit test: Lua logic via miniredis
- [ ] `rplb_circuit_state{backend,replica}` gauge

---

## Status legend

| Symbol | Meaning |
|--------|---------|
| `- [ ]` | Not started |
| `- [x]` | Complete |
| ~~item~~ | Dropped / won't fix |

## Priority order for implementation

1. #121 Go version (5 min, unblocks reliable CI)
2. #122 crypto/rand seed (1 hr, correctness)
3. #126 Slowloris (2 hrs, security)
4. #123 captureWriter pool (half-day, perf)
5. #124 ConsistentHash zero-alloc (half-day, perf)
6. #125 Admission ceiling (1 day, OOM safety)
7. #131 Canary auto-promote (1 day, ops)
8. #132 Coverage 75% (2 days, quality)
9. #127 ACME + Pebble (1 day, TLS completeness)
10. #128 OIDC introspection (2 days, auth)
11. #129 k8s EndpointSlice (3 days, discovery)
12. #130 Distributed circuit breaker (3 days, resilience)
