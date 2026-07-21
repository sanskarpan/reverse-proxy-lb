# Reverse Proxy Load Balancer

A production-grade reverse proxy and load balancer written in Go — no frameworks, no bloat, stdlib `net/http` all the way down.

---

## Why rplb?

rplb is designed for teams that need the operational depth of HAProxy or Envoy without the C++ build chain, the YAML sprawl of Kubernetes ingress controllers, or the runtime footprint of a JVM-based solution. It is a single static binary with zero runtime dependencies that fits equally well as a sidecar, a Kubernetes ingress replacement, or a standalone edge proxy.

---

## Feature grid

| Feature | Details |
|---------|---------|
| 10 LB algorithms | Round-robin, weighted, SWRR, least-conn, P2C, EWMA, consistent-hash, IP-hash, WLC, weighted-random |
| Full resilience stack | Circuit breaker (consecutive + rolling window), retries with budget, hedged requests, outlier ejection |
| L7 routing | Match on Host, path prefix, HTTP method, and arbitrary headers — per-route algorithm + backends |
| Production TLS | ACME/Let's Encrypt auto-cert, SNI multi-cert, mTLS (client + upstream), OCSP stapling, HTTP/3 QUIC |
| Distributed rate limiting | Token-bucket and GCRA, per-IP or per-header key, Redis-backed cross-instance budget |
| Security middleware | JWT + OIDC introspection, API-key, Basic auth, CORS, ACL (IP/method/path), security headers |
| Observability | Hand-written Prometheus exposition, structured JSON access logs, OpenTelemetry tracing, pprof |
| Zero-downtime operations | Live config reload (SIGHUP / HTTP / file-watch), drain API, canary auto-promote with rollback |

---

## Quick start

### Minimal config

```yaml
backends:
  - url: "http://localhost:8001"
  - url: "http://localhost:8002"
  - url: "http://localhost:8003"

load_balancer:
  algorithm: round_robin

logging:
  level: info
  format: json
```

### Run

```bash
# Build
make build

# Start
./bin/proxy --config config.yaml

# Verify
curl http://localhost:8080/
```

### Docker

```bash
docker build -t rplb .
docker run -p 8080:8080 -p 9090:9090 \
  -v $(pwd)/configs/config.yaml:/etc/rplb/config.yaml \
  rplb --config /etc/rplb/config.yaml
```

---

## Architecture

```
                   ┌──────── admin plane :9090 (loopback) ─────────┐
                   │  /metrics  /healthz  /readyz  /reload           │
                   │  /admin/backends  /debug/pprof                  │
                   └────────────────────────────────────────────────┘

  ┌────────┐       ┌──────────────────────────────────────────┐       ┌──────────┐
  │        │       │          middleware chain                 │       │backend-1 │
  │ client │──────►│ Recover → Auth → RateLimit → Cache → Gzip│──────►│backend-2 │
  │        │       │         → Proxy (retry + hedge)          │       │backend-3 │
  └────────┘       └──────────────────────────────────────────┘       └──────────┘
                                    │                 ▲
                          balancer selection    health checker
                          circuit breaker       discovery (DNS/k8s)
```

The full middleware order is:

```
Recover → SecurityHeaders → CORS → ACL → Auth → RequestID → AccessLog
→ Rewrite → FaultInjection → Mirror → MaxBytes → Logging → Metrics
→ RateLimit → Cache → Gzip → Proxy
```

Each middleware is conditional on its config block being enabled, so disabled features add zero overhead.

---

## Data plane and admin plane

| Plane | Default address | Purpose |
|-------|----------------|---------|
| Data | `0.0.0.0:8080` | Proxied traffic |
| Admin | `127.0.0.1:9090` | Metrics, health, reload, drain, pprof |

The admin plane binds to loopback by default and optionally requires a bearer token (`metrics.auth_token`).

---

## Key links

- [Getting started](getting-started.md) — install, minimal config, first request
- [Configuration reference](configuration.md) — every field documented
- [Load-balancing algorithms](algorithms/index.md) — comparison table and deep dives
- [Resilience](resilience/circuit-breaker.md) — circuit breaker, retries, hedging, rate limiting
- [Security](security/tls-acme.md) — TLS/ACME, JWT/OIDC, hardening
- [Operations](ops/runbook.md) — runbook, metrics, tracing, canary, Kubernetes
- [Cookbooks](cookbook/blue-green.md) — blue/green, canary rollout, WebSocket, k8s sidecar
- [Architecture decisions](adr/001-zero-alloc-hotpath.md) — ADRs explaining key design choices
