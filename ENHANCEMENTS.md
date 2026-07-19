# Enhancements, Additions & Roadmap

An in-depth, prioritized catalog of everything this reverse proxy / load balancer
could gain to move from "solid learning project" toward production parity with
nginx / HAProxy / Envoy / Traefik. Derived from a full read of the codebase plus
comparison against production data planes.

## How to read this
- **Priority:** `P0` correctness/security gaps worth fixing first · `P1` high-value
  production features · `P2` valuable features · `P3` advanced / nice-to-have.
- **Effort:** S (hours) · M (a day or two) · L (multi-day) · XL (major subsystem).
- Items marked **(grounded)** were verified against the current source during this audit.

---

## 0. Correctness & security gaps found in the current code (P0)

> **✅ Section complete (2026-07-17).** Items 0.1–0.10 shipped via a 7-fix hardening
> pass (ISSUES 21–27); 0.11 fixed in §2; 0.12 fixed via `reloadMu`. All P0 items are
> now resolved. The two struck rows below are kept for traceability.

| # | Priority | Effort | Item |
|---|----------|--------|------|
| ~~0.11~~ | — | — | ~~Health-check `threshold` read without lock~~ — **✅ done** (§2: dropped the mutable `threshold`/`SetThreshold`; thresholds now come from immutable per-run config). |
| ~~0.12~~ | — | — | ~~Config-reload data race~~ — **✅ done** (`reloadConfig` now serialized by `reloadMu` so concurrent SIGHUP + `POST /reload` can't race on `s.cfg`; regression-tested under `-race`, confirmed RED without the lock). |

---

## 1. Load balancing algorithms & routing (P1–P2)

> **✅ Section complete (2026-07-17).** All of §1 shipped across two swarm workflows.
> The adversarial passes caught and we fixed a stacked-wrapper self-deadlock (ISSUES 28).
> Tracked in `ISSUES.md` (28, 31).

| # | Priority | Effort | Item |
|---|----------|--------|------|
| ✅ 1.6 | P2 | L | **L7 routing** — `routes:` with Host / path-prefix / method / header matching (first-match-wins) to per-route backend pools with per-route algorithm; unmatched → default group; failover stays within the matched group; per-group health checks. New `internal/routing` package. *(Per-route advanced wrappers — priority/zone/slow-start/outlier — stay on the default group; documented follow-up.)* |

**Shipped:** consistent-hash w/ bounded loads (1.1), SWRR (1.2), P2C (1.3), peak-EWMA
(1.4), weighted-least-conn (1.5), **L7 routing (1.6)**, sticky-cookie affinity (1.7),
priority tiers (1.8), slow-start (1.9), outlier ejection (1.10), zone-aware (1.11),
weighted-random (1.12).

### Architectural note — ✅ done
The request-aware selection refactor landed via optional capability interfaces
(`KeyedBalancer.NextForKey`, `LatencyObserver`, `OutcomeObserver`) that the proxy
discovers by type-assertion, so hashing/affinity/latency/outlier strategies are
first-class without breaking the base `Balancer` interface.

---

## 2. Health checking (P1–P2)

> **✅ Shipped 2026-07-17 (2.1, 2.2, 2.3, 2.5-TCP, 2.6, 2.7).** Swarm workflow: 5 e2e
> scenarios (non-flaky at `-count=5`), 4 adversarial verifiers all green; also fixed
> the 0.11 threshold data race. Tracked in `ISSUES.md`. Remaining below.

| # | Priority | Effort | Item |
|---|----------|--------|------|
| 2.4 | P2 | M | **Passive health checking** — *effectively delivered by §1 outlier detection* (live request outcomes eject/reinstate backends via `OutcomeObserver`). Left open only for a fuller unification/config surface. |
| 2.5-grpc | P2 | M | **gRPC health checks** (gRPC health-checking protocol) — *deferred* (needs grpc-go). TCP + HTTP checks shipped. |

**Shipped:** rise/fall thresholds (2.1), configurable criteria — method/expected-statuses/body-match/Host/headers (2.2), interval jitter (2.3), TCP checks (2.5), per-backend overrides (2.6), startup-grace / readiness (2.7).

---

## 3. Circuit breaking & resilience (P1–P2) — ✅ Shipped 2026-07-17

> Swarm workflow (circuit + config + proxy → integrate → 6-scenario e2e →
> adversarial verify). The verifier caught a **hedging reservation leak** (extras
> reserved but never released when the primary won before the hedge delay); fixed
> TDD with a regression test. Tracked in `ISSUES.md` (ID 29). All shipped:

| # | Priority | Effort | Item |
|---|----------|--------|------|
| ✅ 3.1 | P1 | M | **Rolling-window, rate-based tripping** — opt-in `circuit_breaker.mode: rolling` (sliding buckets, trips on error-rate over the window past `min_requests`); consecutive mode unchanged. |
| ✅ 3.2 | P1 | S | **Failure classification** — connect / timeout / 5xx / ok; `trip_on` selects which count as circuit failures (5xx counts but isn't retried). |
| ✅ 3.3 | P1 | S | **Retry budgets** — `retry.budget` caps retries as a fraction of requests (with a floor); budget-denied counter. |
| ✅ 3.4 | P1 | S | **Full-jitter backoff** — exponential with full jitter, capped; best-effort `Retry-After`. |
| ✅ 3.5 | P2 | S | **Per-try timeout** — `retry.per_try_timeout` abandons a slow attempt (classified timeout) and fails over. |
| ✅ 3.6 | P2 | M | **Hedged requests** — opt-in, idempotent-only; races the primary against extras after `hedge.delay`, single-writer response, losers cancelled, reservations released exactly once (post-fix). |
| ✅ 3.7 | P2 | S | **Circuit-state change events** — `SetOnStateChange` hook wired to logs/metrics. |
| ✅ 3.8 | P2 | M | **Bulkheads** — per-backend `MaxConns` enforced; excess → 503 with a rejection counter. |

---

## 4. Rate limiting (P1–P2) — ✅ Shipped 2026-07-17 (except 4.4)

> Swarm workflow: 6 e2e scenarios + 4 adversarial verifiers, all green, no bugs found
> this round. Tracked in `ISSUES.md`.

| # | Priority | Effort | Item |
|---|----------|--------|------|
| ✅ 4.1 | P1 | S | **Independent global vs per-key limits** — `global_rps`/`global_burst` separate from per-key `requests_per_second`/`burst`. |
| ✅ 4.2 | P1 | S | **`Retry-After` header + configurable 429 body** (`message`, `retry_after_seconds`; header derived from the limiter's suggested wait). |
| ✅ 4.3 | — | — | LRU/lastSeen eviction + max map size (former 0.4). |
| ✅ 4.4 | P2 | L | **Distributed rate limiting** — pluggable `limiter.Store` interface (shared cross-instance budget) + in-memory `MemStore`; multiple limiters sharing one Store enforce a **combined** limit (e2e-proven). A Redis-backed `Store` (GCRA Lua) is a drop-in adapter — the only piece needing `go-redis`, kept out of core `go.mod`. |
| ✅ 4.5 | P2 | M | **Per-route rules + header/API-key keying** — `rules` (path-prefix + method, first match wins) and `key: header:<Name>`. |
| ✅ 4.6 | P2 | M | **GCRA algorithm** option (`algorithm: gcra`) alongside token-bucket. |
| ✅ 4.7 | P2 | S | **Allowlist / exempt lists** — `allowlist` CIDRs bypass limiting. |

---

## 5. Timeouts, connections & protocols (P1–P2) — ✅ Shipped 2026-07-17 (except 5.4/5.5)

> Swarm workflow: 5 e2e scenarios + 4 adversarial verifiers, all green, no bugs found
> this round. Added the new `internal/tcpproxy` package and `golang.org/x/net/http2`
> (h2c) — both within the stdlib + `golang.org/x/*` policy. Tracked in `ISSUES.md`.

| # | Priority | Effort | Item |
|---|----------|--------|------|
| ✅ 5.1 | P1 | S | **Config-driven upstream timeouts** — `server.upstream.{dial,tls_handshake,response_header,expect_continue,idle_conn}_timeout`, defaulting to the §0 hardening constants. |
| ✅ 5.2 | P1 | M | **Per-backend connection pools** — each cached backend proxy owns its own `*http.Transport`, so `max_idle_conns_per_host` / `max_conns_per_host` apply per backend. |
| ✅ 5.3 | P1 | M | **HTTP/2 upstream (h2c)** — `server.upstream.http2`: https via ALPN `ForceAttemptHTTP2`, http via `x/net/http2` h2c. (Enables basic gRPC passthrough.) |
| ◑ 5.4 | P2 | L | **gRPC proxying** — **carried today**: h2c upstream (5.3) tunnels gRPC (HTTP/2 + trailers via `ReverseProxy`), and per-method routing works via L7 `path_prefix` routes (a gRPC method is `/pkg.Svc/Method`). gRPC-*native* health/reflection needs `grpc-go` (follow-up). |
| ⨯ 5.5 | P2 | L | **HTTP/3 / QUIC** — **not feasible without a dependency**: Go's stdlib has no QUIC. Requires `quic-go` (large dep) + a QUIC client to e2e; genuinely out of the stdlib+`x/*` policy. Documented as the one intentionally-unimplemented item. |
| ✅ 5.6 | P2 | M | **L4 TCP proxy** — new `internal/tcpproxy`; `server.l4.{enabled,port}` forwards raw TCP through the balancer (UDP not included). |
| ✅ 5.7 | P2 | S | **WebSocket idle timeout & max message** — `server.websocket.{idle_timeout,max_message_bytes}` enforced on the hijacked conn (idle read-deadline + cumulative byte cap). |
| ✅ 5.8 | P2 | S | **Client-disconnect cancellation** — upstream context derives from the client request; a disconnect aborts the in-flight backend call (regression-tested). |

---

## 6. TLS & security (P1–P2) — ✅ Shipped 2026-07-17 (except 6.2/6.10/6.11)

> Swarm workflow: 10 e2e scenarios + 4 adversarial verifiers (incl. a dedicated
> auth-bypass hunt) — all green, no bugs found. New `internal/tlsutil` package;
> `go.mod` stayed third-party-free. Tracked in `ISSUES.md`.

| # | Priority | Effort | Item |
|---|----------|--------|------|
| ✅ 6.1 | P1 | S | **TLS min-version & cipher policy** — `tls.min_version` (1.2/1.3), `cipher_suites`. |
| ✅ 6.2 | P1 | M | **ACME/Let's Encrypt auto-cert** — `tls.acme` via `x/crypto/acme/autocert` (HostWhitelist, shared TLS + HTTP-01 challenge handler, `directory_url` for staging). Wiring + challenge handler tested; real issuance needs a reachable domain. |
| ✅ 6.3 | P1 | S | **Certificate hot-reload** — `tls.reload_on_change`; `GetCertificate` mtime-cache re-reads rotated keypairs, no restart, no fsnotify. |
| ✅ 6.4 | P1 | S | **Security headers** — `security.headers`: HSTS, `X-Content-Type-Options`, `X-Frame-Options`, CSP, Referrer-Policy. (Hop-by-hop already stripped by ReverseProxy; XFF normalized via netutil.) |
| ✅ 6.5 | P2 | M | **mTLS** — downstream (`tls.client_auth: require_and_verify` + `client_ca_file`) and to-backends (`backend_tls.client_cert_file`/`client_key_file`). |
| ✅ 6.6 | P2 | M | **SNI multi-cert** — `tls.certificates[]` selected by ServerName (wildcard single-label supported). |
| ✅ 6.7 | P2 | M | **Auth middleware** — Basic (constant-time), API-key, JWT **HS256 + RS256** (RS256 via PEM key or **JWKS** URL w/ `kid`; alg-confusion/`none`/expired/forged rejected). OAuth2/OIDC introspection still a follow-up. |
| ✅ 6.8 | P2 | S | **IP allow/deny ACLs + method allowlist + path blocklist** (`security.acl`, trusted-proxy-aware). |
| ✅ 6.9 | P2 | S | **CORS** middleware (`security.cors`: origins/methods/headers/credentials/max-age + preflight). |
| 6.10 | P2 | S | **Secrets from env/Vault** — **deferred** (follow-up). |
| ✅ 6.11 | P3 | M | **OCSP stapling** — `tls.ocsp_stapling`: fetches + staples OCSP responses (`x/crypto/ocsp`), periodic refresh before `NextUpdate`, never staples revoked/unknown. |

---

## 7. Observability (P1) — ✅ Shipped 2026-07-17 (except 7.2/7.6)

> Swarm workflow: 7 e2e scenarios + 3 adversarial verifiers (incl. histogram-cumulative
> correctness and in-flight-gauge-balance), all green, no bugs. `go.mod` third-party-free.
> Tracked in `ISSUES.md`.

| # | Priority | Effort | Item |
|---|----------|--------|------|
| ✅ 7.1 | P1 | M | **Richer Prometheus metrics** — latency **histogram** (`rplb_response_latency_seconds_bucket/_sum/_count`), `requests_by_class_total{class}`, `inflight_requests`, `rate_limited_total`, and scrape-time `backend_up` / `backend_circuit_state` gauges (via a snapshot callback). Hand-written text exposition, no `client_golang`. |
| 7.2 | P1 | M | **OpenTelemetry tracing** — **deferred** (needs OTel deps). |
| ✅ 7.3 | P1 | S | **Request IDs** — `X-Request-ID` minted if absent, forwarded upstream + echoed on the response, in the request context + access log. |
| ✅ 7.4 | P1 | S | **Access logs** — structured JSON line per request (method/path/status/duration/bytes/client-ip/request-id), sampled (1/N). |
| ✅ 7.5 | P1 | S | **`net/http/pprof`** — mounted on the loopback admin mux, gated by admin auth. |
| 7.6 | P2 | S | **Migrate to `log/slog`** — **deferred** (low value / high churn; current logger API is stable). |
| ✅ 7.7 | P2 | S | **Health/circuit/rate-limit event metrics** — circuit-state gauge (from the breaker), rate-limit-rejection counter, backend up/down gauge; health transitions already logged. |

---

## 8. Configuration & dynamic control (P1–P2) — ✅ Shipped 2026-07-18 (8.1 default group, 8.3, 8.4; 8.5 partial)

> Swarm workflow: 5 e2e scenarios + 2 adversarial verifiers, all green. The reload-
> under-load e2e also surfaced (and fixed) a real **logging data race** (level read
> outside the mutex). Tracked in `ISSUES.md` (ID 31).

| # | Priority | Effort | Item |
|---|----------|--------|------|
| ✅ 8.1 | P1 | M | **Live backend hot-reload** — `reloadConfig` diffs backends by URL and Add/Remove/UpdateWeight on the live balancer under `reloadMu`; removed backends get circuit `Reset`. **Default group only**; algorithm and route/canary topology still need a restart (precise warnings). |
| ✅ 8.2 | P1 | S | **Env-var + CLI-flag overrides** — `RPLB_*` env vars plus `--host`/`--port`/`--log-level`/`--metrics-port` flags (applied only when explicitly set), and `--validate`. |
| ✅ 8.3 | P1 | S | **`proxy --validate`** — loads+validates the config and exits 0/non-zero without starting. |
| ✅ 8.4 | P1 | S | **File-watch auto-reload** — `server.watch_config` + `watch_interval` (mtime polling, no fsnotify dep). |
| ✅ 8.5 | P1 | S | **Admin API** — `POST /reload` plus `GET /admin/backends`, `POST /admin/drain\|undrain\|weight\|circuit/reset` (loopback, bearer-auth). |
| ✅ 8.6 | P2 | L | **DNS service discovery** — `discovery.dns` (A/SRV, periodic re-resolution, syncs into the live default group, never touches static backends). Consul/etcd/k8s-Endpoints still need their client libs (follow-up). |
| ✅ 8.7 | P2 | M | **Backend draining** — `POST /admin/drain?url=` stops new traffic; runbook covers the drain→remove flow. |
| 8.8 | P2 | S | **Per-route config live reload** — follow-up (route/canary reload still restart-required). |

---

## 9. Traffic management & data-plane features (P2) — ✅ Shipped 2026-07-17 (except 9.6)

> Swarm workflow: 10 e2e scenarios + 3 adversarial verifiers (incl. mirror-safety at
> `-race -count=5` and canary-group isolation), all green, no bugs. Tracked in `ISSUES.md`.

| # | Priority | Effort | Item |
|---|----------|--------|------|
| ✅ 9.1 | P2 | M | **Weighted traffic splitting (canary)** — `canary.weight_percent` sends a fraction of traffic to a canary pool (own algorithm/backends/health); failover + observers stay within the chosen group. |
| ✅ 9.2 | P2 | M | **Traffic mirroring / shadowing** — `mirror`: fire-and-forget shadow copy to a target for `sample_percent` of requests; body safely buffered; primary never affected by mirror errors/latency. |
| ✅ 9.3 | P2 | M | **Header & path rewriting + HTTPS redirect** — `rewrite`: set/remove request & response headers, `strip_path_prefix`, `https_redirect` (308). |
| ✅ 9.4 | P2 | M | **Fault injection** — `fault_injection`: delay/abort a configurable % (deterministic rng). |
| ✅ 9.5 | P2 | S | **Compression polish** — `compression.min_size` + `content_types` allowlist (deferred-decision buffering); brotli/zstd still a follow-up. |
| ✅ 9.6 | P3 | L | **Response caching** — in-memory LRU cache (`cache.*`): Cache-Control (max-age/s-maxage/no-store/no-cache/private/public), ETag/If-Modified-Since 304, Vary keying + purge-on-change, stale-while-revalidate, RFC 7234 §3.2 Authorization isolation, streaming/WS passthrough. |

---

## 10. Performance (P2) — partially shipped 2026-07-18

| # | Priority | Effort | Item |
|---|----------|--------|------|
| ✅ 10.1 | P2 | M | **Cached healthy-backend list** — `GetHealthy()` is now cached, invalidated by a global health-epoch (bumped only on real health changes) + per-balancer generation. Hot-path result: **350 ns/960 B/4 allocs → 11 ns/0 B/0 allocs** at 64 backends, immediately reflecting health changes. (Consistent-hash `NextForKey` allocation remains a follow-up.) |
| ✅ 10.2 | P2 | M | **Pool copy buffers** — `ReverseProxy.BufferPool` backed by a `sync.Pool` of 32 KiB buffers (per-request allocation pooling for `errCapture`/`captureWriter` still a follow-up). |
| 10.3 | P2 | M | **Sharded / lock-free metrics** — *open* (metrics are already largely atomic; sharding is a micro-opt). |
| ✅ 10.4 | P2 | S | **Benchmarks** — `internal/balancer/bench_test.go` covers RR/SWRR/P2C/least-conn/weighted-random/consistent-hash (`make bench`). |
| 10.5 | P2 | M | **Load-test harness** — *open* (follow-up). |
| 10.6 | P3 | M | **PGO** — *open* (needs production profiles). |

## 11. Testing & quality (P1) — largely shipped 2026-07-18

> From 44 tests at the start to **363 tests + 3 fuzz targets + 6 benchmarks**, all under
> `go test -race`; every subsystem has in-repo integration/e2e coverage.

| # | Priority | Effort | Item |
|---|----------|--------|------|
| ✅ 11.1 | P1 | M | **In-repo integration tests** — full server-chain e2e for failover, health, rate-limit, reload, TLS/mTLS, WS, gzip, routing, canary, mirror, observability, etc. |
| ✅ 11.2 | P1 | S | **Clock injection** — circuit, health, EWMA, slow-start, and outlier detection take injectable clocks for deterministic fast tests. |
| 11.3 | P1 | S | **Coverage threshold gate** — CI reports coverage; a hard threshold is a follow-up. |
| ✅ 11.4 | P2 | M | **Fuzz tests** — `FuzzLoad` (config), `FuzzClientIP` / `FuzzParseCIDRs` (header/CIDR parsing). |
| 11.5 | P2 | M | **Property-based distribution tests** — statistical distribution tests exist per algorithm; formal property-based is a follow-up. |
| 11.6 | P2 | M | **Chaos suite** — fault-injection *feature* + flaky-backend e2e exist; a dedicated chaos harness is a follow-up. |
| ✅ 11.7 | P2 | S | **`golangci-lint` + `gosec`** — `.golangci.yml` + CI jobs (staticcheck, gosec, vet). |

---

## 12. Build, CI/CD & operations (P1–P2) — ✅ Shipped 2026-07-17 (12.1–12.5; 12.6/12.7 follow-ups)

> Done directly (mostly files + two small code changes) rather than via a swarm. Full
> `-race` suite green; static linux build verified. Tracked in `ISSUES.md`.

| # | Priority | Effort | Item |
|---|----------|--------|------|
| ✅ 12.1 | P1 | S | **Self `/healthz` + `/readyz`** on the loopback admin listener (unauthenticated probes, separate from the proxied data plane). `/readyz` = 200 iff any backend across any group is healthy. |
| ✅ 12.2 | P1 | S | **Multi-stage Dockerfile** (distroless static) + **docker-compose** demo (proxy + 3 backends, `configs/config.docker.yaml`). |
| ✅ 12.3 | P1 | S | **GitHub Actions CI** — gofmt check, vet, build, `go test -race` + coverage, staticcheck, gosec, docker build. |
| ✅ 12.4 | P1 | S | **Configurable drain timeout** — `server.shutdown_timeout` (default 30s), used by `Stop`. |
| ✅ 12.5 | P2 | S | **Makefile targets** — test/test-race/cover/bench/lint/fmt/vet/docker/compose. (goreleaser/changelog still a follow-up.) |
| ✅ 12.6 | P2 | M | **Kubernetes manifests + Helm chart** — `deploy/k8s/` (Deployment/Service/ConfigMap with `/healthz`+`/readyz` probes, security context, drain-aware `terminationGracePeriodSeconds`/preStop) and `deploy/helm/rplb/`. |
| ✅ 12.7 | P2 | S | **systemd unit** — `deploy/systemd/rplb.service` (hardened, `ExecReload` = SIGHUP, drain-aware `TimeoutStopSec`). Ops runbook: see §13.3. |

---

## 13. Documentation (P2) — partially shipped 2026-07-18

| # | Priority | Effort | Item |
|---|----------|--------|------|
| ✅ 13.1 | P2 | S | **Full config reference** — `docs/CONFIG.md` (every section, key fields, defaults). |
| ✅ 13.2 | P2 | S | **Architecture + request-flow diagram** — `docs/ARCHITECTURE.md` (component overview + a mermaid middleware/selection/failover flow). |
| ✅ 13.3 | P2 | S | **Operations runbook** — `docs/RUNBOOK.md` (deploy, reload, admin/drain, dashboards/alerts, troubleshooting). Published benchmark numbers still a follow-up. |
| 13.4 | P2 | S | **README refresh** — ✅ done; godoc/CONTRIBUTING still a follow-up. |
| ✅ 13.5 | P2 | S | **Reconcile README with reality** — README now reflects the real algorithm list, Prometheus metrics, env vars, `/reload`, endpoints, and docker. |

---

## Suggested sequencing

1. **Harden (P0, §0):** ✅ mostly done — error-leak, idempotent retries, upstream
   timeouts, rate-limiter eviction, panic recovery, body/header limits, Prometheus
   metrics, localhost admin bind + auth, env-var config and `POST /reload` all shipped
   (see `ISSUES.md` 21–27). Remaining: self-health endpoint (§12.1), the config-reload
   race (0.12), and the health-check threshold lock (0.11).
2. **Observe (P1, §7 + §12.3):** Prometheus histograms, request IDs, access logs, pprof,
   CI with race+lint. You can't safely evolve what you can't measure.
3. **Operate (P1, §8 + §12):** full hot reload, admin API/`/reload`, Docker/compose,
   drain timeout, config validate.
4. **Route & balance (P1–P2, §1–§5):** consistent hashing, SWRR, P2C, per-backend pools,
   granular timeouts, HTTP/2.
5. **Advanced (P2–P3):** gRPC/L4, tracing depth, caching, traffic splitting/mirroring,
   service discovery, distributed rate limiting.

## Reference points
nginx (SWRR, upstream keepalive), HAProxy (health-check richness, stick tables),
Envoy (outlier detection, retry budgets, zone-aware LB, circuit breaking tiers),
Traefik (dynamic config / service discovery), Linkerd/Finagle (peak-EWMA, P2C,
retry budgets, request hedging).
