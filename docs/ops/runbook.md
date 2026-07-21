# Operations Runbook

This runbook covers day-to-day operations: deployment, config reload, runtime control, observability, and troubleshooting.

---

## Deployment

### Binary

```bash
make build
./bin/proxy --validate --config /etc/rplb/config.yaml   # always validate first
./bin/proxy --config /etc/rplb/config.yaml
```

The systemd unit is at `deploy/systemd/rplb.service`. It includes:
- `User=rplb` and `Group=rplb` for privilege separation.
- `ExecReload=/bin/kill -HUP $MAINPID` for config reload via `systemctl reload rplb`.
- `LimitNOFILE=65536` for high-connection workloads.
- `ProtectSystem=full` and `PrivateTmp=yes` for systemd hardening.

```bash
# Install
sudo cp deploy/systemd/rplb.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now rplb

# Status
sudo systemctl status rplb

# Reload config without restart
sudo systemctl reload rplb

# View logs
journalctl -u rplb -f
```

### Docker

```bash
docker build -t rplb .
docker run -d \
  --name rplb \
  -p 8080:8080 \
  -p 127.0.0.1:9090:9090 \
  -v /etc/rplb/config.yaml:/etc/rplb/config.yaml:ro \
  rplb --config /etc/rplb/config.yaml
```

Or with Docker Compose (includes mock backends for development):

```bash
docker compose up --build
```

### Kubernetes

```bash
# Apply manifests
kubectl apply -f deploy/k8s/

# Verify
kubectl -n rplb rollout status deployment/rplb-rplb
kubectl -n rplb get pods
```

Always validate config before rollout:

```bash
kubectl -n rplb exec deploy/rplb-rplb -- \
  /bin/proxy --validate --config /etc/rplb/config.yaml
```

See [Kubernetes](kubernetes.md) for Helm chart usage.

---

## Config reload (no restart required)

The following fields take effect on reload **without a restart**:

| Field | Reload method |
|-------|--------------|
| `backends` (add/remove/reweight) | Live diff applied to the balancer |
| `rate_limiter.*` | New limits applied immediately |
| `logging.level` | Hot-swapped via `slog.LevelVar` |
| `routes` (table) | Atomic route-table swap |
| `canary.weight_percent` | Applied immediately |

Fields that **require a restart**:

| Field | Reason |
|-------|--------|
| `load_balancer.algorithm` | Algorithm change requires rebuilding the balancer state machine |
| `tls.*` | Listener must be re-bound |
| `server.port` | Listener must be re-bound |
| Route/canary topology (adding a route group) | The group balancer must be initialized |

When a reload is attempted with a topology change, rplb logs a WARN and continues with the old config — it does not partially apply changes.

### Three reload methods

**SIGHUP (recommended for systemd):**

```bash
kill -HUP $(pidof proxy)
# or
systemctl reload rplb
```

**HTTP POST (admin API):**

```bash
curl -XPOST http://localhost:9090/reload
# With bearer token:
curl -H "Authorization: Bearer $ADMIN_TOKEN" -XPOST http://localhost:9090/reload
```

**File watch (automatic):**

```yaml
server:
  watch_config: true
  watch_interval: 5s
```

The file watcher polls the config file's mtime every `watch_interval` and triggers a reload when a change is detected.

---

## Runtime control (admin API)

All admin API endpoints are on the admin plane (`127.0.0.1:9090`). Add `-H "Authorization: Bearer $ADMIN_TOKEN"` if `metrics.auth_token` is configured.

### List backends

```bash
curl http://localhost:9090/admin/backends
```

Response includes health status, in-flight requests, circuit state, and weight for each backend.

### Drain a backend (stop new traffic)

```bash
curl -XPOST 'http://localhost:9090/admin/drain?url=http://b1:8000'
```

A drained backend still handles existing in-flight requests but receives no new ones. Watch `active_conns` in the backends list:

```bash
watch -n1 "curl -s http://localhost:9090/admin/backends | python3 -m json.tool"
```

### Undrain a backend

```bash
curl -XPOST 'http://localhost:9090/admin/undrain?url=http://b1:8000'
```

### Change backend weight at runtime

```bash
curl -XPOST 'http://localhost:9090/admin/weight?url=http://b1:8000&weight=5'
```

### Force-close a circuit breaker

```bash
curl -XPOST 'http://localhost:9090/admin/circuit/reset?url=http://b1:8000'
```

### Zero-downtime backend rollout procedure

1. Add the new backend to the config with weight=1.
2. Reload config (`kill -HUP` or `POST /reload`).
3. Verify the new backend is healthy: `GET /admin/backends`.
4. Drain the old backend: `POST /admin/drain?url=<old>`.
5. Wait for `active_conns` on the old backend to reach 0 (visible in `/admin/backends`).
6. Remove the old backend from the config.
7. Reload config again.

---

## Observability

### Prometheus metrics

```bash
curl http://localhost:9090/metrics
```

Key metrics and their meanings:

| Metric | When to look at it |
|--------|--------------------|
| `rplb_requests_total` | Baseline traffic volume |
| `rplb_errors_total` | Backend failures |
| `rplb_requests_by_class_total{class="5xx"}` | Upstream application errors |
| `rplb_response_latency_seconds_bucket` | Latency distribution |
| `rplb_inflight_requests` | Current concurrency |
| `rplb_rate_limited_total` | Rate limit events |
| `rplb_backend_up{backend}` | Individual backend health |
| `rplb_backend_circuit_state{backend}` | Circuit breaker state |

See [Metrics](metrics.md) for queries and Grafana dashboard.

### Structured logs

Logs are emitted as JSON by default (`logging.format: json`). Each access log line includes:

```json
{
  "time": "2026-07-21T10:00:00Z",
  "level": "INFO",
  "msg": "request",
  "x-request-id": "req-abc-123",
  "method": "GET",
  "path": "/api/v1/users",
  "status": 200,
  "latency_ms": 12.3,
  "backend": "http://b1:8000",
  "bytes": 1024,
  "client_ip": "203.0.113.5"
}
```

Trace a request end-to-end using `X-Request-ID`:

```bash
# The client receives X-Request-ID in the response header
# Server logs can be filtered:
journalctl -u rplb | grep '"x-request-id":"req-abc-123"'
```

### pprof profiling

```bash
# 30-second CPU profile
go tool pprof http://localhost:9090/debug/pprof/profile?seconds=30

# Heap snapshot
go tool pprof http://localhost:9090/debug/pprof/heap

# Goroutine dump (useful for diagnosing stuck requests)
curl http://localhost:9090/debug/pprof/goroutine?debug=2
```

---

## Suggested alerts

| Alert | Expression | Severity |
|-------|-----------|---------|
| High error rate | `rate(rplb_errors_total[5m]) / rate(rplb_requests_total[5m]) > 0.05` | warning |
| Backend down | `rplb_backend_up == 0` | critical |
| Circuit open | `rplb_backend_circuit_state == 1` | warning |
| Latency p99 > 1s | `histogram_quantile(0.99, rate(rplb_response_latency_seconds_bucket[5m])) > 1` | warning |
| Readiness failing | HTTP probe on `/readyz` != 200 | critical |

---

## Common issues

### 503 "No available backends"

All backends in the default group are unhealthy or circuit-open.

1. Check backend health: `curl http://localhost:9090/admin/backends`
2. Check if backends are actually up: `curl http://backend-host:8000/health`
3. Check circuit state: look for `rplb_backend_circuit_state == 1`
4. Check access logs for connect errors
5. Force-reset circuits if backends recovered: `POST /admin/circuit/reset?url=...`
6. `/readyz` will also be 503 — this causes Kubernetes to remove the pod from Service endpoints

### 429 Too Many Requests

Rate limit is being triggered.

1. Check `rplb_rate_limited_total` to see the rate
2. Identify the offending client: filter access logs by `status=429`
3. Add the client to `rate_limiter.allowlist` if it is legitimate internal traffic
4. Tune `rate_limiter.requests_per_second` or `burst` for the affected path

### 502 Bad Gateway

Upstream transport failures (connection refused, reset, TLS error).

1. Filter access logs for `"status":502` to find the failing backend
2. Check per-backend error count: `rplb_errors_total{backend=...}`
3. Test direct connectivity: `curl http://backend-host:8000/`
4. Check backend application logs for panics or crashes

### TLS handshake failures

1. Verify `tls.min_version` is compatible with the client's TLS stack
2. Check cert/key paths: `openssl x509 -in /path/to/cert.crt -text`
3. For mTLS: verify `client_ca_file` contains the correct CA chain
4. For ACME: check the cache directory permissions and DNS propagation

### High latency

1. Check `rplb_response_latency_seconds_bucket` per-backend to identify slow backends
2. Check `rplb_inflight_requests` — if near `max_conns`, backends are saturated
3. Enable EWMA algorithm to automatically shift traffic to faster backends
4. Consider enabling request hedging (`retry.hedge.enabled: true`) to mask tail latency

### Memory growth

1. Collect a heap profile: `go tool pprof http://localhost:9090/debug/pprof/heap`
2. Check for goroutine leak: `curl http://localhost:9090/debug/pprof/goroutine?debug=1`
3. Common cause: response cache unbounded — check `middleware.cache.max_entries`
4. Common cause: rate-limiter LRU growing — check key diversity

---

## Graceful shutdown

On `SIGTERM` (or `Ctrl-C`):

1. The proxy closes the data-plane listener — no new connections accepted.
2. In-flight requests continue to completion.
3. After `server.shutdown_timeout`, any remaining connections are force-closed.
4. The admin plane shuts down last.

For Kubernetes, set `terminationGracePeriodSeconds` ≥ `server.shutdown_timeout + 5` in the Deployment spec to give the pod time to drain.
