# Getting Started

This page walks you from zero to a running reverse proxy in under five minutes.

---

## Prerequisites

| Requirement | Minimum version | Notes |
|-------------|----------------|-------|
| Go | 1.21 | Build from source; `go install` |
| Docker | any recent | Optional — for container deployment |
| Redis | 7.x | Optional — only needed for distributed rate limiting or distributed circuit-breaker sync |

---

## Install

### Option 1 — Build from source (recommended)

```bash
git clone https://github.com/sanskarpan/Reverse-Proxy-Load-Balancing
cd Reverse-Proxy-Load-Balancing
make build          # produces ./bin/proxy
./bin/proxy --help
```

The binary is statically linked and has no runtime dependencies.

### Option 2 — `go install`

```bash
go install reverse-proxy-lb/cmd/proxy@latest
```

### Option 3 — Docker

```bash
docker build -t rplb .
```

Or pull and run with Docker Compose (demo with three echo-server backends):

```bash
docker compose up --build
```

---

## Minimal configuration

Create `config.yaml`:

```yaml
backends:
  - url: "http://localhost:8001"
  - url: "http://localhost:8002"
  - url: "http://localhost:8003"

load_balancer:
  algorithm: round_robin
  health_check:
    enabled: true
    interval: 10s
    path: "/health"

logging:
  level: info
  format: json
```

That is all you need to start proxying traffic. Every other block is opt-in.

---

## Run

```bash
./bin/proxy --config config.yaml
```

On startup you will see structured JSON log lines:

```json
{"time":"2026-07-21T10:00:00Z","level":"INFO","msg":"starting proxy","addr":"0.0.0.0:8080"}
{"time":"2026-07-21T10:00:00Z","level":"INFO","msg":"admin plane","addr":"127.0.0.1:9090"}
{"time":"2026-07-21T10:00:00Z","level":"INFO","msg":"health check started","backends":3}
```

### Validate config without starting

```bash
./bin/proxy --validate --config config.yaml
```

Exit code 0 means the config is valid. Use this in CI or before a rolling restart.

---

## Verify

```bash
# Proxy a request
curl http://localhost:8080/

# Check which backends are healthy
curl http://localhost:9090/admin/backends

# Prometheus metrics
curl http://localhost:9090/metrics

# Readiness (returns 200 when at least one backend is healthy)
curl http://localhost:9090/readyz
```

---

## Environment variable overrides

You can override any critical field without editing the config file:

```bash
RPLB_SERVER_PORT=9000 \
RPLB_LOG_LEVEL=debug \
RPLB_BACKENDS=http://b1:8000,http://b2:8000 \
  ./bin/proxy --config config.yaml
```

All `RPLB_*` overrides are documented in the [configuration reference](configuration.md#environment-overrides).

---

## Graceful shutdown

Send `SIGTERM` (or `Ctrl-C`). The proxy stops accepting new connections, waits up to `server.shutdown_timeout` (default `30s`) for in-flight requests to finish, then exits.

---

## Next steps

- [Configuration reference](configuration.md) — every field with types and defaults
- [Algorithms](algorithms/index.md) — choose the right balancing strategy
- [Resilience](resilience/circuit-breaker.md) — add circuit breaking and retries
- [Security](security/tls-acme.md) — enable TLS with auto-cert
- [Operations runbook](ops/runbook.md) — live reload, drain, alerts
