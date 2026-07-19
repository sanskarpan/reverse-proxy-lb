# Operations runbook

## Deploy

- **Binary:** `make build` → `./bin/proxy --config /etc/rplb/config.yaml`. systemd unit
  in `deploy/systemd/rplb.service`.
- **Docker:** `docker build -t rplb .` or `docker compose up --build` (demo).
- **Kubernetes:** `kubectl apply -f deploy/k8s/`. Liveness → `/healthz`, readiness →
  `/readyz` on the admin port (9090). Set `terminationGracePeriodSeconds` ≥
  `server.shutdown_timeout`.

Always validate config before rollout: `./bin/proxy --validate --config <file>`.

## Config reload (no restart)

Backends (add/remove/reweight), rate limits, and log level reload live. Algorithm and
route/canary topology still need a restart.

- **SIGHUP:** `kill -HUP <pid>` (or `systemctl reload rplb`).
- **HTTP:** `curl -XPOST localhost:9090/reload` (bearer token if `metrics.auth_token` set).
- **File-watch:** set `server.watch_config: true` to auto-reload on file change.

## Runtime control (admin API, loopback, bearer-auth)

```bash
curl localhost:9090/admin/backends                        # list backends + health
curl -XPOST 'localhost:9090/admin/drain?url=http://b:8001'    # stop new traffic (drain)
curl -XPOST 'localhost:9090/admin/undrain?url=http://b:8001'
curl -XPOST 'localhost:9090/admin/weight?url=http://b:8001&weight=3'
curl -XPOST 'localhost:9090/admin/circuit/reset?url=http://b:8001'
```

**Zero-downtime backend rollout:** drain the old backend, wait for its `active_conns`
to reach 0 (visible in `/admin/backends`), then remove it from config and reload.

## Observability

- **Metrics:** `GET /metrics` (Prometheus). Key series:
  `rplb_requests_total`, `rplb_errors_total`, `rplb_requests_by_class_total{class}`,
  `rplb_response_latency_seconds_bucket` (histogram → p50/p90/p99),
  `rplb_inflight_requests`, `rplb_rate_limited_total`,
  `rplb_backend_up{backend}`, `rplb_backend_circuit_state{backend}`.
- **Logs:** structured JSON; `X-Request-ID` is minted/propagated and included in access
  logs — grep by request id to trace a request end-to-end.
- **Profiling:** `go tool pprof http://localhost:9090/debug/pprof/profile`.

### Suggested alerts

| Alert | Expression (idea) |
|-------|-------------------|
| High error rate | `rate(rplb_errors_total[5m]) / rate(rplb_requests_total[5m]) > 0.05` |
| Backend down | `rplb_backend_up == 0` |
| Circuit open | `rplb_backend_circuit_state == 1` |
| Latency p99 | `histogram_quantile(0.99, rate(rplb_response_latency_seconds_bucket[5m])) > 1` |
| Readiness | `probe /readyz != 200` |

## Common issues

- **All requests 503 "No available backends":** every backend is unhealthy — check
  `/admin/backends`, backend `/health`, and circuit state. `/readyz` will also be 503.
- **429s:** rate limit hit — check `rplb_rate_limited_total`; tune `rate_limiter` or add
  an `allowlist`.
- **502s:** upstream transport failures — check per-backend `errors` and logs (detailed
  errors are logged server-side; clients get a generic body).
- **TLS handshake failures:** verify `tls.min_version` vs client, cert/key paths, and
  (for mTLS) `client_ca_file`.
- **Slowloris / big bodies:** bounded by `ReadHeaderTimeout`, `MaxHeaderBytes`, and the
  10 MiB request-body cap.
