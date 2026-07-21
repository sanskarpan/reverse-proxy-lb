# Metrics and Observability

rplb exposes metrics in Prometheus text format on the admin plane. Metrics are written by a hand-rolled Prometheus exposition engine (no `client_golang` dependency) using sharded lock-free counters to minimize false sharing at high parallelism.

---

## Scrape configuration

Admin plane binds to `127.0.0.1:9090` by default. For Prometheus to scrape it from outside the host, either:

1. Configure Prometheus to run on the same host.
2. Change `metrics.host` to `0.0.0.0` and protect it with `metrics.auth_token`.

```yaml
# prometheus.yml
scrape_configs:
  - job_name: rplb
    static_configs:
      - targets: ["localhost:9090"]
    metrics_path: /metrics
    # If auth_token is set:
    authorization:
      credentials: "my-secret-token"
```

For Kubernetes scraping via pod annotations (added automatically by the Helm chart):

```yaml
annotations:
  prometheus.io/scrape: "true"
  prometheus.io/port: "9090"
  prometheus.io/path: "/metrics"
```

---

## All `rplb_*` metrics

### Request counters

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `rplb_requests_total` | Counter | `method`, `backend`, `group` | Total HTTP requests proxied |
| `rplb_errors_total` | Counter | `backend`, `error_type` | Total errors (connect/timeout/5xx) |
| `rplb_requests_by_class_total` | Counter | `class` | Requests grouped by response class (`2xx`, `3xx`, `4xx`, `5xx`) |

### Latency

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `rplb_response_latency_seconds` | Histogram | `method`, `backend`, `group` | End-to-end request latency (time to first byte to response complete) |

Bucket boundaries (seconds): `0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10`

### Concurrency

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `rplb_inflight_requests` | Gauge | — | Current number of in-flight requests across all backends |
| `rplb_rate_limited_total` | Counter | — | Requests rejected by the rate limiter (429) |

### Backend health

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `rplb_backend_up` | Gauge | `backend` | 1 if the backend is healthy, 0 if unhealthy |
| `rplb_backend_circuit_state` | Gauge | `backend` | Circuit breaker state: 0=closed, 1=open |

---

## Example Prometheus queries

### Error rate (5-minute window)

```promql
rate(rplb_errors_total[5m])
  /
rate(rplb_requests_total[5m])
```

### Request throughput (RPS)

```promql
sum(rate(rplb_requests_total[1m]))
```

### p50, p90, p99 latency

```promql
histogram_quantile(0.50, rate(rplb_response_latency_seconds_bucket[5m]))
histogram_quantile(0.90, rate(rplb_response_latency_seconds_bucket[5m]))
histogram_quantile(0.99, rate(rplb_response_latency_seconds_bucket[5m]))
```

### Backend-specific error rate

```promql
rate(rplb_errors_total{backend="http://api-1:8000"}[5m])
```

### Backends currently down

```promql
rplb_backend_up == 0
```

### Open circuits

```promql
rplb_backend_circuit_state == 1
```

### Rate-limited requests per minute

```promql
rate(rplb_rate_limited_total[1m]) * 60
```

---

## Suggested alerts

| Alert name | Expression | For | Severity |
|-----------|-----------|-----|---------|
| High error rate | `rate(rplb_errors_total[5m]) / rate(rplb_requests_total[5m]) > 0.05` | 2m | warning |
| Critical error rate | `rate(rplb_errors_total[5m]) / rate(rplb_requests_total[5m]) > 0.25` | 1m | critical |
| Backend down | `rplb_backend_up == 0` | 30s | critical |
| Circuit open | `rplb_backend_circuit_state == 1` | 1m | warning |
| High latency p99 | `histogram_quantile(0.99, rate(rplb_response_latency_seconds_bucket[5m])) > 1` | 5m | warning |
| Readiness failing | `probe_success{job="rplb-readyz"} == 0` | 1m | critical |
| High rate limiting | `rate(rplb_rate_limited_total[5m]) > 10` | 5m | warning |

---

## Grafana dashboard

A sample Grafana dashboard JSON is provided at `deploy/grafana/rplb-dashboard.json`. It includes:

- Request rate and error rate panels
- Latency heatmap (p50/p90/p99 overlays)
- Backend health status table
- In-flight requests gauge
- Rate limit events counter

Import via Grafana UI: Dashboards → Import → Upload JSON file.

---

## JSON metrics endpoint

For non-Prometheus consumers, the admin plane also exposes metrics as JSON:

```bash
curl http://localhost:9090/metrics.json
```

Response structure:

```json
{
  "requests_total": 1234567,
  "errors_total": 123,
  "inflight_requests": 42,
  "rate_limited_total": 5,
  "backends": [
    {
      "url": "http://b1:8000",
      "healthy": true,
      "circuit_state": "closed",
      "inflight": 14,
      "requests_total": 411522,
      "errors_total": 41
    }
  ],
  "latency_p50_ms": 8.2,
  "latency_p90_ms": 23.1,
  "latency_p99_ms": 87.4
}
```

---

## Sharded counters (implementation note)

All counters (`rplb_requests_total`, `rplb_errors_total`, etc.) use `ShardedCounter` — a cache-line padded counter array with `GOMAXPROCS` shards. Each goroutine increments its shard (selected by `runtime.Gosched()` goroutine ID) to avoid false sharing on the same cache line.

At read time (Prometheus scrape), the shards are summed. The total reported value may lag by at most one scrape interval from the true atomic total, which is acceptable for a metrics use case.

This design allows the counter hot path to operate without any atomic contention between goroutines at high parallelism, at the cost of eventual consistency within a scrape window.

---

## pprof profiling

CPU, memory, goroutine, and block profiles are available on the admin plane:

```bash
# 30-second CPU profile
go tool pprof http://localhost:9090/debug/pprof/profile?seconds=30

# Heap snapshot
go tool pprof http://localhost:9090/debug/pprof/heap

# Goroutine stack trace
curl http://localhost:9090/debug/pprof/goroutine?debug=2
```

The `pprof` endpoints are exposed on the admin plane only. They require the bearer token if `metrics.auth_token` is set.
