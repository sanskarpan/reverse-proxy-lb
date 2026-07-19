# Known Issues / Defect Tracker

Status legend: `OPEN` · `FIXED` · `WONTFIX`

This file was produced by an adversarial audit of the reverse-proxy load balancer.
Each entry lists severity, location, the defect, how to reproduce, and the fix.

---

## ID 1 — Backend failures recorded as SUCCESS; retry & circuit breaker dead
- **Severity:** Critical · **Category:** Logic/Contract · **Status:** FIXED
- **Location:** `internal/proxy/proxy.go` (`doRequest`, `proxyRequest`)
- **Defect:** `doRequest` returned `nil` unconditionally. Transport errors were handled
  by `ReverseProxy.ErrorHandler` (which wrote a 502) but never propagated out, so
  `lastErr` was always nil. Result: retry never fired, `RecordFailure` was never
  called, and a 502'd request was recorded as `RecordSuccess` with `TotalErrors=0`.
- **Reproduce:** point a backend at a closed port, send one request → 502 to client,
  `TotalRetries=0`, `TotalErrors=0`, circuit stays Closed.
- **Fix:** Capture the upstream error via request-context + a custom `ErrorHandler`
  that does not write the response, and a `captureWriter` that tracks whether bytes
  were written. `doRequest` now returns `(error, written)`; the retry loop and
  circuit breaker act on the real error. Failover only when nothing was written yet.

## ID 2 — Connection pool / `httpClient` / `MaxConns` unused
- **Severity:** High · **Category:** Contract/Data · **Status:** FIXED
- **Location:** `internal/proxy/proxy.go`
- **Defect:** The pooled `http.Client` was built but never used; a fresh
  `ReverseProxy` was allocated per request on `http.DefaultTransport`. `MaxConns`
  was stored but never enforced.
- **Fix:** One cached `ReverseProxy` per backend URL sharing a single pooled
  `http.Transport`. `MaxConns` enforced during selection/failover.

## ID 3 — Per-IP rate limiting bypassable via client-supplied `X-Forwarded-For`
- **Severity:** Critical · **Category:** Security · **Status:** FIXED
- **Location:** `internal/middleware/middleware.go` (`getClientIP`)
- **Defect:** Rate-limit key was the raw `X-Forwarded-For` header, so an attacker
  rotates the header to get unlimited per-IP buckets and to spoof any IP.
- **Fix:** New `internal/netutil.ClientIP` uses the direct peer (`RemoteAddr`) and
  only honors forwarding headers when the peer is in a configured
  `server.trusted_proxies` CIDR list. Default (no trusted proxies) ignores headers.

## ID 4 — Data race on `backend.Healthy` (plain bool)
- **Severity:** High · **Category:** Race · **Status:** FIXED
- **Location:** `internal/balancer/balancer.go`, `internal/health`, `internal/circuit`
- **Defect:** `Healthy bool` written by health checker and circuit breaker without
  holding the balancer mutex, read by `GetHealthy` — unsynchronized.
- **Fix:** `Healthy` is now an `atomic.Bool` behind `IsHealthy()`/`SetHealthy()`.

## ID 5 — Circuit breaker: data race, TOCTOU, half-open floods
- **Severity:** High · **Category:** Race · **Status:** FIXED
- **Location:** `internal/circuit/breaker.go` (`Allow`)
- **Defect:** `Allow` read `state.state` with no lock held (race with Record*), used
  a separate lock region for the Open→HalfOpen transition (TOCTOU), and admitted
  every request while HalfOpen (thundering herd on a recovering backend). Also, a
  single `RecordFailure` on an unknown backend opened the circuit immediately.
- **Fix:** `Allow` does read-decide-transition under one lock; HalfOpen admits a
  bounded number of probes (`successThreshold`); unknown backends initialize Closed.

## ID 6 — Configured log level/format ignored
- **Severity:** Medium · **Category:** Contract · **Status:** FIXED
- **Location:** `internal/logging`, `internal/server/server.go`
- **Defect:** `logging.SetLevel` never called; `cfg.Logging.Level/Format` ignored,
  so `Debug` logging never emitted.
- **Fix:** `logging.Configure(level, format)` invoked in `server.New`.

## ID 7 — Hot reload reads the wrong file; reloads almost nothing
- **Severity:** Medium · **Category:** Logic/Contract · **Status:** FIXED
- **Location:** `internal/server/server.go` (`reloadConfig`)
- **Defect:** Hard-coded `config.Load("configs/config.yaml")` ignored the `--config`
  flag path.
- **Fix:** The actual config path is stored on the server and used on reload; the
  rate limiter (see ID 8) and log level are re-applied. Topology changes
  (backends/algorithm) still require restart — logged explicitly, not silently faked.

## ID 8 — `RateLimiter.UpdateRate` never updates existing per-IP limiters
- **Severity:** Medium · **Category:** Logic · **Status:** FIXED
- **Location:** `internal/limiter/limiter.go` (`UpdateRate`)
- **Defect:** The per-key update loop was a no-op (`_ = limiter`); existing buckets
  kept the old rate forever.
- **Fix:** Rebuild each existing limiter at the new rate/burst.

## ID 9 — Metrics double-count requests; 502s not counted as errors
- **Severity:** Medium · **Category:** Data · **Status:** FIXED
- **Location:** `internal/proxy/proxy.go`, `internal/middleware/middleware.go`
- **Defect:** Both the Metrics middleware and `Proxy.ServeHTTP` called `IncrRequest`.
- **Fix:** Requests counted once (middleware). Errors counted in the proxy on final
  failure (ID 1 makes them real).

## ID 10 — Least-connections selection races (thundering herd)
- **Severity:** Medium · **Category:** Race/Logic · **Status:** FIXED
- **Location:** `internal/balancer/leastconn.go` + proxy
- **Defect:** Selection picked the min-conn backend but the increment happened later
  in the proxy, so concurrent selects all picked the same backend.
- **Fix:** Selection reserves the connection atomically (increments under the same
  lock/atomic as selection). The proxy releases the reservation when done.

## ID 11 — `X-Forwarded-For` overwritten (chain lost) and set to `host:port`
- **Severity:** Medium · **Category:** Contract · **Status:** FIXED
- **Location:** `internal/proxy/proxy.go` (custom `Director`)
- **Defect:** Director overwrote XFF with `RemoteAddr` (`ip:port`), destroying the
  chain.
- **Fix:** Use the default `ReverseProxy` Director, which *appends* the peer IP to
  the XFF chain; set `X-Real-IP` to the trusted-aware client IP.

## ID 12 — Latent infinite recursion / double WriteHeader in failover
- **Severity:** Medium (latent) · **Category:** Logic · **Status:** FIXED
- **Location:** `internal/proxy/proxy.go` (`tryNextBackend`)
- **Defect:** `tryNextBackend`→`proxyRequest`→`tryNextBackend` could recurse until
  stack overflow and write the response header twice.
- **Fix:** Rewritten as an iterative failover loop with a `tried` set, a single
  response write, and a "response already started → stop" guard.

## ID 13 — `WeightedRoundRobin.sequences` dead & mutated without a lock
- **Severity:** Low · **Category:** Race/Dead-code · **Status:** FIXED
- **Location:** `internal/balancer/weighted.go`
- **Defect:** `sequences` map written by `Add`/`Remove` (outside the mutex), never
  read.
- **Fix:** Removed the field and the overridden `Add`/`Remove`.

## ID 14 — No config validation
- **Severity:** Medium · **Category:** Contract · **Status:** FIXED
- **Location:** `internal/config/config.go`
- **Defect:** Empty backends, unknown algorithm, bad ports, `rps<=0`, unparseable
  URLs all silently accepted.
- **Fix:** `Config.validate()` runs in `Load` and rejects these with clear errors;
  empty algorithm defaults to `round_robin`.

## ID 15 — Metrics server errors swallowed; no timeouts
- **Severity:** Low/Medium · **Category:** Data/Reliability · **Status:** FIXED
- **Location:** `internal/server/server.go`
- **Defect:** `http.ListenAndServe` return value discarded; default server (no
  timeouts).
- **Fix:** Dedicated `http.Server` with timeouts; errors logged; shut down on stop.

## ID 16 — `Server.wg` dead; startup failure leaves a zombie
- **Severity:** Low · **Category:** Logic · **Status:** FIXED
- **Location:** `internal/server/server.go`
- **Defect:** Unused wait group; `Start` errors only logged in a goroutine while
  `Run` blocked forever.
- **Fix:** Removed `wg`; `Run` propagates startup errors via a channel and returns.

---

## ID 17 — (Regression, found during e2e) captureWriter broke streaming/WebSockets
- **Severity:** High · **Category:** Logic · **Status:** FIXED
- **Location:** `internal/proxy/proxy.go` (`captureWriter`)
- **Defect:** The `captureWriter` introduced for ID 1 wrapped the `ResponseWriter` but
  did not forward `http.Flusher`/`http.Hijacker`, so SSE/streaming and WebSocket
  upgrades would have broken (the original code passed the raw writer). Normal
  buffered responses were unaffected, so it was invisible until streaming was tested.
- **Fix:** `captureWriter` now implements `Flush()` and `Hijack()`, delegating to the
  underlying writer and marking the response as started. Covered by a compile-time
  interface assertion, a hijack-delegation test, and an end-to-end streaming test.

## ID 18 — Gzip compression (SPEC §8) was never implemented
- **Severity:** Medium · **Category:** Contract · **Status:** FIXED
- **Location:** `internal/middleware/gzip.go`, `internal/server/server.go`, config
- **Defect:** SPEC/README advertised gzip compression; it did not exist.
- **Fix:** Added a `Gzip` middleware (opt-in via `compression.enabled`) that compresses
  responses when the client sends `Accept-Encoding: gzip`, skips WebSocket upgrades,
  never double-compresses, and forwards `Flush`/`Hijack`. Verified e2e over HTTPS.

## ID 19 — Metrics middleware wrapper broke WebSocket upgrades in the full chain
- **Severity:** High · **Category:** Logic · **Status:** FIXED
- **Location:** `internal/middleware/middleware.go` (`responseWriter`)
- **Defect:** The Metrics middleware's `responseWriter` did not forward `Hijack`/`Flush`,
  so even with the ID 17 fix a WebSocket/SSE request through the real chain would fail
  at the metrics layer.
- **Fix:** `responseWriter` now forwards `Flush` and `Hijack`. Verified with a real
  WebSocket handshake driven through the compiled binary (`101` + echo round-trip).

## ID 20 — Backend (upstream) TLS was not configurable (SPEC §10)
- **Severity:** Medium · **Category:** Contract/Security · **Status:** FIXED
- **Location:** `internal/config/config.go` (`BackendTLSConfig`), `internal/proxy`,
  `internal/health`, `internal/server`
- **Defect:** The proxy could not talk to `https://` backends whose certs weren't in
  the system trust store, and there was no way to pin a CA — SPEC §10 "Backend TLS
  support" was unmet. The proxy/health transports had no `TLSClientConfig`.
- **Fix:** Added `backend_tls` config (`ca_file`, `insecure_skip_verify`) built into a
  `*tls.Config` and applied to both the proxy transport and the health-check client.
  Verified: default verification fails closed on a self-signed backend (502), a pinned
  `ca_file` verifies successfully (200), and `insecure_skip_verify` bypasses (200).
  Unit test asserts all three paths including fail-closed.

## End-to-end verification performed
- **Backend TLS** — proxy → self-signed https backend: `ca_file` trust → 200,
  default verification → 502 (fail closed), `insecure_skip_verify` → 200.
- **Real RFC6455 WebSocket** — `x/net/websocket` client dialed *through* the proxy
  (real handshake + masked framing), echo round-trip succeeded across the full chain.
- **Failover / retry / circuit** — dead backend, 500 concurrent requests → all 200.
- **Health-check eviction + recovery**, **rate-limit 429s**, **live SIGHUP reload**,
  **IP-hash stickiness**, **graceful shutdown drain** (slow backend, SIGTERM mid-flight).
- **TLS termination** (self-signed cert) → HTTPS 200.
- **Gzip over HTTPS** → `Content-Encoding: gzip`, `1f8b` magic, correct decompression.
- **WebSocket** upgrade tunneled through the full middleware chain → `101` + echo.

## P0 hardening pass (2026-07-17) — ISSUES 21–27

Implemented via a parallel agent workflow (per-file agents → integration → adversarial
verification). All seven verified correct; full suite `go test -race` green (63 tests).

## ID 21 — Client-facing error leak
- **Severity:** High · **Category:** Security · **Status:** FIXED
- **Location:** `internal/proxy/proxy.go` (`proxyRequest`)
- **Defect:** The terminal failure path wrote `"Backend error: "+lastErr.Error()` to the
  client, leaking dial/TLS/backend-URL detail.
- **Fix:** Returns a generic `http.StatusText(502)` body; the detailed error stays in
  server-side logs only.

## ID 22 — Retries not idempotency-aware
- **Severity:** High · **Category:** Data/Correctness · **Status:** FIXED
- **Location:** `internal/proxy/proxy.go` (`attemptBackend`, `proxyRequest`)
- **Defect:** Every method (incl. `POST`/`PATCH`) was retried/failed-over, risking
  double-apply.
- **Fix:** `isIdempotent(r)` (GET/HEAD/PUT/DELETE/OPTIONS/TRACE or `Idempotency-Key`)
  gates same-backend retries; cross-backend failover for non-idempotent requests is
  allowed only on a connection-establishment error (`isConnectError`). Verified: POST to
  a dead backend fails over (safe); POST does 0 same-backend retries while GET retries.

## ID 23 — Missing granular upstream timeouts
- **Severity:** Medium · **Category:** Reliability · **Status:** FIXED
- **Location:** `internal/proxy/proxy.go` (transport in `New`)
- **Fix:** Transport now sets `DialContext` (5s connect / 30s keep-alive),
  `TLSHandshakeTimeout` 5s, `ResponseHeaderTimeout` 30s, `ExpectContinueTimeout` 1s,
  preserving the pool settings and `TLSClientConfig`.

## ID 24 — Rate-limiter wipe-all cleanup + unbounded growth
- **Severity:** Medium · **Category:** Security/Reliability · **Status:** FIXED
- **Location:** `internal/limiter/limiter.go`
- **Fix:** Per-key entries track `lastSeen`; `cleanup` evicts only idle-past-TTL entries
  (active buckets survive, so no burst reset); `maxEntries` cap with stale-then-oldest
  eviction bounds memory. `UpdateRate` still rebuilds existing keys.

## ID 25 — No panic recovery / body / header limits
- **Severity:** Medium · **Category:** Reliability/Security · **Status:** FIXED
- **Location:** `internal/middleware/hardening.go`, `internal/server/server.go`
- **Fix:** `middleware.Recover` (logs + 500, re-panics `ErrAbortHandler`, doesn't wrap
  the writer so WS/streaming survive) and `middleware.MaxBytes` (caps request bodies)
  wired into the chain; `httpServer` sets `ReadHeaderTimeout` and `MaxHeaderBytes`.

## ID 26 — Metrics: JSON→Prometheus + hardened admin plane
- **Severity:** Medium · **Category:** Observability/Security · **Status:** FIXED
- **Location:** `internal/metrics/metrics.go`, `internal/server/server.go`
- **Fix:** `PrometheusHandler` emits real text exposition (`# TYPE`, labelled per-backend
  series, `text/plain; version=0.0.4`) at `/metrics`; JSON kept at `/metrics.json`. The
  admin server binds to `Metrics.Host` (default `127.0.0.1`) with optional bearer-token
  auth (`Metrics.AuthToken`, constant-time compare).

## ID 27 — SPEC drift: env-var config, POST /reload, dead fields
- **Severity:** Medium · **Category:** Contract · **Status:** FIXED
- **Location:** `internal/config/config.go`, `internal/server/server.go`
- **Fix:** `applyEnvOverrides` (runs in `Load` before `validate`) honors `RPLB_*` incl.
  `RPLB_BACKENDS`; `POST /reload` endpoint (405 on other methods) triggers reload; dead
  `BackendConfig.Healthy`/`ActiveConns` fields removed.
- **Note (follow-up):** `reloadConfig` mutates shared `s.cfg` read concurrently by
  handlers — a pre-existing reload data race tracked as ENHANCEMENTS 0.12.

## ENHANCEMENTS §1 (load balancing) — 2026-07-17

Shipped consistent-hash (bounded-load), SWRR, P2C, peak-EWMA, weighted-least-conn,
weighted-random, sticky-cookie affinity, priority tiers, slow-start, outlier ejection,
zone-aware — plus request-aware capability interfaces (`KeyedBalancer`,
`LatencyObserver`, `OutcomeObserver`). 10-scenario e2e + adversarial verification.

## ID 28 — Stacked-wrapper self-deadlock (found by adversarial verify)
- **Severity:** Critical · **Category:** Race/Logic · **Status:** FIXED
- **Location:** `internal/balancer/wrappers.go`
- **Defect:** Wrappers restricted the backend set by toggling excluded backends'
  health under a single package-level non-reentrant mutex held across `inner.Next()`.
  When two restricting wrappers stacked — `ZoneAware(PriorityTiers(...))`, the exact
  server composition when `prefer_same_zone` + tiers are set — the inner wrapper
  re-locked the same mutex and the selection goroutine **self-deadlocked** (request
  hangs forever). The health-toggle also raced the health checker. The e2e missed it
  because it only tested the wrappers separately.
- **Fix:** Replaced the health-mutation/global-lock scheme with lock-free **subset
  composition**: each wrapper narrows the candidate set and delegates via an internal
  `subsetPicker` (`pickFrom`/`pickFromKey`); the common unrestricted case still uses
  full inner-algorithm fidelity, restricted subsets compose through nested wrappers,
  and base algorithms fall back to a stateless least-conn / hash pick. No shared lock,
  no health mutation.
- **Regression tests:** `TestStackedWrappersNoDeadlock` and
  `TestStackedWrappersKeyedNoDeadlock` (5s-timeout guarded); verified end-to-end
  against the real binary with a `prefer_same_zone` + tiered config.

## ENHANCEMENTS §2 (health checking) — 2026-07-17

Shipped separate rise/fall thresholds, configurable HTTP criteria (method / expected
statuses / body match / Host / headers), interval jitter, TCP checks, per-backend
overrides, and startup-grace/readiness. Also fixed the **0.11 threshold data race**
(dropped the mutable `threshold`/`SetThreshold`; thresholds now come from immutable
per-run config). 5 e2e scenarios (non-flaky at `-count=5`) + 4 adversarial verifiers,
all green. Passive health (2.4) is covered by §1 outlier detection. gRPC health
(2.5-grpc) deferred (needs grpc-go).

## ENHANCEMENTS §3 (resilience) — 2026-07-17

Shipped rolling-window rate-based circuit tripping (+ state-change hook), failure
classification, retry budgets, full-jitter backoff, per-try timeout, hedged requests,
and per-backend bulkheads. All opt-in, defaulting to prior behavior. 6 e2e scenarios +
5 adversarial verifiers.

## ID 29 — Hedged-request reservation leak (found by adversarial verify)
- **Severity:** High · **Category:** Race/Data · **Status:** FIXED
- **Location:** `internal/proxy/proxy.go` (`proxyRequestHedged`)
- **Defect:** Extra hedge backends were reserved (`IncrConn`) up front, but when the
  primary produced a response **before** the hedge delay elapsed, the code `break`s out
  without launching them — and each extra's `DecrConn` lived only inside the
  never-started goroutine. So every such hedged request leaked one reservation per
  configured extra; leaked slots accumulate until `MaxConns` is falsely hit, causing
  spurious bulkhead 503s and excluding the backend from selection. The e2e missed it
  because it didn't assert `ActiveConns` returned to 0.
- **Fix:** After the collect/drain loops, release any extras reserved but never
  launched (`if !extrasLaunched { for _, b := range extras { b.DecrConn() } }`).
- **Regression test:** `TestHedgePrimaryWinsNoReservationLeak` (20 alternating-role
  hedged GETs; asserts every backend's `ActiveConns` returns to 0); green at `-count=3`,
  proxy package `-race` clean.

## ENHANCEMENTS §4 (rate limiting) — 2026-07-17

Shipped independent global/per-key limits, `Retry-After` + configurable 429 body,
per-route rules + header/API-key keying, GCRA algorithm option, and allowlist. All
opt-in, defaulting to prior per-IP+global behavior. 6 e2e scenarios + 4 adversarial
verifiers, all green (no defects found this round). Distributed rate limiting (4.4)
deferred (needs Redis).

## ENHANCEMENTS §5 (connections & protocols) — 2026-07-17

Shipped config-driven upstream timeouts, per-backend connection pools, HTTP/2 (h2c)
upstream, an L4 TCP proxy (`internal/tcpproxy`), WebSocket idle-timeout/max-message,
and client-disconnect cancellation. All opt-in. 5 e2e scenarios + 4 adversarial
verifiers, all green (no defects found this round). Added `golang.org/x/net` (h2c) —
within the stdlib + `golang.org/x/*` policy. Deferred: gRPC-specific routing (5.4;
h2c already carries gRPC) and HTTP/3 (5.5).

## ID 30 — Config-reload data race (ENHANCEMENTS 0.12)
- **Severity:** Medium · **Category:** Race · **Status:** FIXED
- **Location:** `internal/server/server.go` (`reloadConfig`)
- **Defect:** `reloadConfig` wrote `s.cfg.Logging`/`s.cfg.RateLimiter` and read
  `s.cfg.Backends` with no synchronization, so a SIGHUP and a `POST /reload` (or two
  concurrent `/reload` requests) could race on `s.cfg`.
- **Fix:** Added `Server.reloadMu` and serialized `reloadConfig` under it.
- **Regression test:** `TestConcurrentReloadNoRace` (8 goroutines × 25 reloads);
  green with the lock, confirmed **DATA RACE** without it.

## ENHANCEMENTS 1.6 (L7 routing) — 2026-07-17

Shipped L7 routing: `routes:` match by Host / path-prefix / method / header
(first-match-wins) to per-route backend pools, each with its own algorithm; unmatched
requests use the default group. New `internal/routing` package (`Router` + `BuildGroup`).
The proxy pins the routed balancer on the request context so selection, in-group
failover, sticky affinity, and latency/outcome observers all stay within the matched
group (no cross-group leakage). Per-group health checks; single shared circuit breaker
(distinct backend pointers). Opt-in: no `routes` => unchanged single-balancer behavior.
6 e2e scenarios + 3 adversarial verifiers, all green (no defects found this round).
Per-route advanced wrappers deferred to a follow-up.

## ENHANCEMENTS §6 (TLS & security) — 2026-07-17

Shipped TLS min-version/cipher policy, cert hot-reload (mtime cache), SNI multi-cert,
mTLS (downstream + to-backends), security-headers middleware, CORS, IP ACL + method +
path filtering, and Basic/API-key/JWT-HS256 auth. New `internal/tlsutil` package.
All opt-in. 10 e2e scenarios + 4 adversarial verifiers (incl. a dedicated auth-bypass
hunt — alg-confusion/`none`/expired/tampered JWTs all rejected, constant-time Basic
compare), all green (no defects found this round). `go.mod` stayed third-party-free
(stdlib + `golang.org/x/*`). Deferred: ACME auto-cert (6.2, needs real domain), Vault
secrets (6.10), OCSP stapling (6.11).

## ENHANCEMENTS §7 (observability) — 2026-07-17

Shipped richer Prometheus metrics (latency histogram, status-class counters, in-flight /
rate-limited / backend-up / circuit-state gauges), `X-Request-ID` propagation, structured
access logs (sampled), and `net/http/pprof` on the loopback admin mux gated by admin
auth. Additive to the existing exposition; `go.mod` third-party-free. 7 e2e scenarios +
3 adversarial verifiers (histogram cumulative correctness + in-flight-gauge balance +
pprof gating), all green (no defects this round). Deferred: OpenTelemetry tracing (7.2,
needs deps), `log/slog` migration (7.6, low value).

## ENHANCEMENTS §9 (traffic management) — 2026-07-17

Shipped canary/weighted traffic splitting, request mirroring/shadowing (body-buffered,
primary-isolated), header & path rewriting + HTTPS redirect, fault injection
(delay/abort), and compression polish (min-size + content-type allowlist). All opt-in.
10 e2e scenarios + 3 adversarial verifiers (mirror-safety `-race -count=5`,
canary-group isolation, wiring), all green (no defects this round). Deferred: response
caching (9.6, large subsystem). Note: the new blocks apply at startup — live reload of
them is a follow-up (same as backends/algorithm).

## ENHANCEMENTS §12 (CI/CD & ops) — 2026-07-17

Shipped self `/healthz` + `/readyz` probes (loopback admin listener, unauthenticated;
readyz = 200 iff any backend healthy), a configurable graceful-shutdown drain timeout
(`server.shutdown_timeout`, default 30s), a multi-stage distroless Dockerfile +
docker-compose demo (+ `configs/config.docker.yaml`), a GitHub Actions CI pipeline
(gofmt/vet/build/`test -race`/coverage/staticcheck/gosec/docker), and expanded Makefile
targets. Done directly (not a swarm). `TestHealthzAndReadyz` added; static linux build
verified; full `-race` suite green. Follow-ups: k8s/Helm (12.6), systemd/runbook (12.7).

## ENHANCEMENTS §8 (dynamic config) — 2026-07-18

Shipped live default-group backend hot-reload (add/remove/reweight by URL diff, circuit
`Reset` for removed backends, under `reloadMu`), a `--validate` config dry-run flag, and
opt-in file-watch auto-reload (`watch_config`/`watch_interval`, mtime polling). 5 e2e
scenarios + 2 adversarial verifiers, all green. Algorithm/route/canary reload remain
restart-required (precise warnings). Service discovery deferred (external deps).

## ID 31 — Logging data race (found by the §8 reload-under-load e2e)
- **Severity:** Medium · **Category:** Race · **Status:** FIXED
- **Location:** `internal/logging/logger.go` (`Logger.log`)
- **Defect:** `log()` called `shouldLog(level)` — which reads `l.level` — **before**
  acquiring `l.mu`, while `SetLevel`/`SetFormat` (invoked by live config reload) write
  `l.level`/`l.format` under the lock. Concurrent request logging + a reload raced.
- **Fix:** `log()` now holds `l.mu` across the level check and the write. Verified under
  `-race` by the reload-under-load e2e.

## ENHANCEMENTS §10/§11/§13 (perf, testing, docs) — 2026-07-18

Done directly (no swarm): `ReverseProxy.BufferPool` (pooled 32 KiB copy buffers, 10.2);
balancer benchmarks (10.4); fuzz targets for config + client-IP/CIDR parsing (11.4);
`.golangci.yml` + gosec/staticcheck CI jobs (11.7); a comprehensive README refresh
(13.4/13.5) and a full `docs/CONFIG.md` reference (13.1). Benchmarks flagged a perf
follow-up: `GetHealthy()` and consistent-hash `NextForKey` allocate on the hot path
(10.1). Full `-race` suite green (363 tests, 3 fuzz, 6 benchmarks).

## Deferred-items pass — response caching, DNS discovery, admin API (2026-07-18)

Shipped response caching (9.6), DNS service discovery (8.6), and the admin/drain API
(8.5/8.7). Adversarial verifiers caught **3 real bugs**, all fixed with regression tests:

## ID 32 — Admin weight mutation data race
- **Severity:** High · **Category:** Race · **Status:** FIXED
- **Location:** `internal/balancer/balancer.go` + weighted algorithms
- **Defect:** `Backend.Weight` was a plain `int`; `POST /admin/weight` (UpdateWeight)
  wrote it under the balancer mutex while SWRR/weighted-random/WRR/weighted-least-conn
  read it unlocked on the hot path → confirmed data race under concurrent traffic.
- **Fix:** `Weight` is now `atomic.Int32` via `GetWeight()`/`SetWeight()`; all readers
  updated. Regression: `TestConcurrentUpdateWeightAndSelect` (`-race`).

## ID 33 — Cache served `no-cache` responses stale
- **Severity:** Medium · **Category:** Correctness · **Status:** FIXED
- **Location:** `internal/middleware/cache.go`
- **Defect:** A response with `Cache-Control: no-cache` was stored and served as a fresh
  `HIT` without revalidation → stale content.
- **Fix:** `no-cache` responses are no longer stored. Regression:
  `TestCacheNoCacheResponseNotServedStale`.

## ID 34 — Cache Vary poisoning / cross-user reuse
- **Severity:** High · **Category:** Correctness/Security · **Status:** FIXED
- **Location:** `internal/middleware/cache.go`
- **Defect:** (a) a resource whose `Vary` set changed could serve a mis-keyed variant;
  (b) a shared cache could serve one authenticated user's cached response to another.
- **Fix:** (a) a `Vary`-set change purges the resource's prior variants; (b) RFC 7234
  §3.2 — responses to `Authorization` requests are neither stored nor reused unless
  explicitly `public`/`s-maxage`. Regressions: `TestCacheVaryKeyedOnceObserved`,
  `TestCacheAuthorizationNotSharedAcrossUsers`, `TestCachePublicSharedForAuthorized`.

## Deferred-items pass (cont.) — TLS extras, auth, flags, perf (2026-07-18)

Shipped ACME auto-cert (6.2), OCSP stapling (6.11), JWT RS256+JWKS (6.7) — swarm, all
verifiers green incl. the JWT-bypass hunt (`go.mod` gained only `golang.org/x/crypto`).
Plus, done directly: CLI flag overrides (8.2); and the **cached healthy-backend list**
(10.1) — `GetHealthy()` cached via a global health-epoch + per-balancer generation,
taking the hot path from 350 ns/960 B/4 allocs to 11 ns/0 B/0 allocs at 64 backends
while still reflecting health changes immediately (`TestGetHealthyCacheReflectsChanges`,
`-race` clean).

## Deferred-items pass (final) — distributed rate limiting (2026-07-18)

Shipped a pluggable `limiter.Store` abstraction for **distributed rate limiting** (4.4):
multiple `RateLimiter` instances sharing one `Store` enforce a combined cross-instance
limit (`SetStore`); `MemStore` is the in-process GCRA backend, e2e-proven with two
"instances" (`TestSharedStoreCombinedLimitAcrossInstances`). A Redis-backed `Store` is a
drop-in adapter — the only piece needing `go-redis`, deliberately kept out of core
`go.mod`. Remaining genuinely-dep-bound items: **HTTP/3** (5.5, no stdlib QUIC — needs
`quic-go`) is the one intentionally-unimplemented feature; **gRPC** (5.4) is already
carried by h2c upstream + L7 path routing (gRPC-native health/reflection would need
`grpc-go`).

## Test-suite gaps addressed
- Added tests for `proxy` (dead backend → retry + circuit failure + failover + single
  metric count), `netutil` (trusted-proxy IP resolution, spoof rejection), `circuit`
  (half-open probe limit), `limiter` (UpdateRate updates existing keys), and `config`
  (validation).
</content>
</invoke>
