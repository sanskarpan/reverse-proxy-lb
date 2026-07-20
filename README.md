# Reverse Proxy & Load Balancer

[![Coverage](https://img.shields.io/badge/coverage-60%25-brightgreen)](https://github.com/sanskarpan/reverse-proxy-lb/actions)

A production-grade HTTP(S) reverse proxy and load balancer written from scratch in Go
(standard library + `golang.org/x/*` only — no third-party framework). It provides
intelligent load balancing, health checking, circuit breaking, rate limiting, TLS,
L7 routing, rich observability, and live configuration reload.

## Features

**Load balancing** — round-robin, smooth weighted round-robin (SWRR), least-connections,
weighted-least-connections, power-of-two-choices (P2C), weighted-random, peak-EWMA
(least-latency), and consistent hashing with bounded loads. Composable wrappers:
priority tiers / backup pools, zone-aware routing, slow-start, and passive outlier
ejection. Sticky sessions via cookie.

**L7 routing** — route by Host / path-prefix / method / header to per-route backend
pools (first-match-wins), with a default fallback group.

**Health checking** — active HTTP or TCP probes, separate rise/fall thresholds,
configurable criteria (method / expected statuses / body match / Host / headers),
interval jitter, per-backend overrides, and a startup grace period.

**Resilience** — circuit breaking (consecutive or rolling-window rate-based, with
half-open probes), failure classification, retry with budgets and full-jitter backoff,
per-try timeouts, request hedging (idempotent), and per-backend bulkheads.

**Rate limiting** — token-bucket or GCRA, independent global vs per-key limits, keying
by IP or header/API-key, per-route rules, allowlists, and `Retry-After`.

**TLS & security** — TLS termination with min-version/cipher policy, SNI multi-cert,
cert hot-reload, mTLS (downstream and to backends); security-headers, CORS, IP/method/
path ACLs, and Basic / API-key / JWT (HS256) authentication.

**Traffic management** — canary/weighted splitting, request mirroring/shadowing,
header & path rewriting + HTTPS redirect, fault injection, and gzip compression
(content-type allowlist + min-size).

**Connections & protocols** — configurable upstream timeouts, per-backend connection
pools, HTTP/2 (h2c) upstream, WebSocket tunneling (with idle/max-message limits),
an optional L4 (raw TCP) proxy, and client-disconnect cancellation.

**Observability** — Prometheus text metrics (counters, gauges, latency histograms,
per-backend series), `X-Request-ID` propagation, structured access logs, and a gated
`pprof` endpoint.

**Operations** — YAML config with env-var overrides and validation; live reload of
backends (add/remove/reweight) via SIGHUP, `POST /reload`, or file-watch; `--validate`
dry-run; graceful shutdown with configurable drain; self `/healthz` + `/readyz` probes.

## Quick start

```bash
make build                 # build bin/proxy and bin/test_server
make run-backends          # start 3 demo backends on :8001-:8003
make run                   # start the proxy on :8080 (admin on :9090)

curl localhost:8080/       # proxied, load-balanced
curl localhost:9090/metrics   # Prometheus metrics (loopback admin listener)
curl localhost:9090/readyz    # readiness probe
```

### Docker

```bash
docker compose up --build     # proxy + 3 backends
curl localhost:8080/
curl localhost:9090/metrics
```

## Configuration

Config is YAML (see `configs/config.yaml`). Every advanced block is **opt-in** and
defaults to safe behavior. Environment variables (`RPLB_SERVER_PORT`,
`RPLB_RATE_LIMIT_RPS`, `RPLB_BACKENDS`, …) override file values. Validate without
starting:

```bash
./bin/proxy --validate --config configs/config.yaml
```

A full field reference is in [docs/CONFIG.md](docs/CONFIG.md).

## Endpoints

Data plane (proxy port, default `:8080`): all traffic is proxied/load-balanced.

Admin plane (loopback by default, `metrics.host:metrics.port`, default `127.0.0.1:9090`),
optionally protected by a bearer token (`metrics.auth_token`):

| Path | Purpose |
|------|---------|
| `GET /metrics` | Prometheus text exposition |
| `GET /metrics.json` | Legacy JSON metrics |
| `GET /healthz` | Liveness probe |
| `GET /readyz` | Readiness (200 iff a healthy backend exists) |
| `POST /reload` | Trigger a config reload |
| `GET /debug/pprof/` | Profiling (guarded) |

## Testing

```bash
make test-race    # go test -race ./...
make cover        # coverage summary
make bench        # balancer benchmarks
```

The suite is self-contained (no external services): 360+ unit/integration tests plus
fuzz targets for config and header parsing. CI (GitHub Actions) runs gofmt, vet, race
tests + coverage, staticcheck, gosec, and a docker build.

## Project layout

```
cmd/proxy            entry point (+ --validate)
internal/
  balancer           LB algorithms + wrappers (tiers/zone/slow-start/outlier)
  routing            L7 request routing to backend groups
  proxy              reverse proxy: retries, failover, hedging, canary, WS
  health             active health checking
  circuit            circuit breaker (consecutive + rolling)
  limiter            rate limiting (token-bucket + GCRA)
  tlsutil            server TLS config (min-version, SNI, mTLS, hot-reload)
  middleware         gzip, rate-limit, security, transforms, observability, hardening
  metrics            Prometheus + JSON metrics
  netutil            trusted-proxy-aware client IP
  tcpproxy           L4 (raw TCP) proxy
  config             YAML config, defaults, validation, env overrides
  server             wiring, admin plane, graceful shutdown, live reload
```

See [SPEC.md](SPEC.md) for the original specification, [ENHANCEMENTS.md](ENHANCEMENTS.md)
for the roadmap and shipped work, and [ISSUES.md](ISSUES.md) for the defect log.

## License

MIT
