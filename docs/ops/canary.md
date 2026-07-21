# Canary Deployments

rplb has first-class support for canary deployments: a configurable percentage of traffic is split to a canary backend group, with optional auto-promotion that steps the percentage up automatically and rolls back on error rate breach.

---

## How traffic splitting works

When the canary is enabled, the proxy performs a weighted random dice roll on each request:

```
if random(0, 100) < canary.weight_percent:
    route to canary backend group
else:
    route to default backend group
```

The split is stateless and per-request — there is no session stickiness between canary and default by default. The request is then handled by the selected group's own balancer and resilience stack (retries, circuit breaker).

---

## Static canary

Split 10% of traffic to a canary permanently:

```yaml
canary:
  enabled: true
  weight_percent: 10
  algorithm: round_robin
  backends:
    - url: "http://canary-v2-1:8000"
      weight: 1
    - url: "http://canary-v2-2:8000"
      weight: 1
```

The main (90%) and canary (10%) backend groups are fully independent — each has its own health checks, circuit breakers, and retry budget. A canary backend failure does not affect the default group.

---

## Auto-promote (graduated rollout)

The auto-promoter steps `weight_percent` up by `step_percent` every `step_interval`, automatically increasing the canary's traffic share until it reaches 100%.

```yaml
canary:
  enabled: true
  weight_percent: 5       # start at 5%
  algorithm: round_robin
  backends:
    - url: "http://v2:8000"

  auto_promote:
    enabled: true
    step_percent: 10      # add 10% per step
    step_interval: 5m     # evaluate every 5 minutes
    max_error_rate: 0.01  # roll back if canary error rate > 1%
    min_requests: 100     # require at least 100 requests before evaluating
```

### Promotion timeline

With the above config starting at 5%:

```
t=0m:   weight=5%   (initial)
t=5m:   weight=15%  (if error_rate < 1%)
t=10m:  weight=25%
t=15m:  weight=35%
t=20m:  weight=45%
t=25m:  weight=55%
t=30m:  weight=65%
t=35m:  weight=75%
t=40m:  weight=85%
t=45m:  weight=95%
t=50m:  weight=100% → canary becomes the default; old default backends can be drained
```

If at any step the error rate exceeds `max_error_rate`:
- `weight_percent` immediately resets to `0`.
- A WARN log is emitted with the observed error rate.
- The canary stops receiving traffic and requires manual intervention to re-enable.

---

## Error rate tracking

The auto-promoter tracks requests and errors in a sliding window per step interval:

- **Requests:** every request routed to the canary group increments the request counter.
- **Errors:** any response with a 5xx status code or a transport error increments the error counter.
- **Rate:** `error_count / request_count` evaluated at each step.

The `min_requests` guard prevents rollback due to a single error when traffic is very low. If fewer than `min_requests` have been observed at evaluation time, the step is skipped (neither promoting nor rolling back).

---

## Metrics

| Metric | Description |
|--------|-------------|
| `rplb_requests_total{group="canary"}` | Total requests routed to canary group |
| `rplb_errors_total{group="canary"}` | Total errors from canary group |
| `rplb_requests_by_class_total{class="5xx",group="canary"}` | Canary 5xx responses |

Query canary error rate in Prometheus:

```promql
rate(rplb_errors_total{group="canary"}[5m])
/
rate(rplb_requests_total{group="canary"}[5m])
```

---

## Admin API

Check the current canary status:

```bash
curl http://localhost:9090/admin/backends
```

Response includes both the default and canary groups with their current weight, backend health, and circuit state:

```json
{
  "groups": {
    "default": {
      "backends": [{"url": "http://v1:8000", "healthy": true, "inflight": 42}]
    },
    "canary": {
      "weight_percent": 25,
      "backends": [{"url": "http://v2:8000", "healthy": true, "inflight": 11}]
    }
  }
}
```

---

## Live reload

Canary `weight_percent` is live-reloadable without restart. Update the config file and either:

```bash
kill -HUP <pid>
# or
curl -XPOST http://localhost:9090/reload
```

This allows manual promotion or rollback without a process restart. Algorithm changes and backend topology changes to the canary group (adding/removing backends) also take effect on reload.

---

## Canary with consistent hash (session stickiness)

For canaries where you want the same user to always hit either the canary or default (not randomized per request), combine consistent hash with the canary:

```yaml
canary:
  enabled: true
  weight_percent: 10
  algorithm: consistent_hash
  consistent_hash:
    replicas: 100
    load_factor: 1.0    # strict — no overflow to the other group
  backends:
    - url: "http://v2:8000"
```

With consistent hash, the same request key (e.g., user ID from a header) will always be routed to the same group. This is useful for A/B testing where you want each user to have a consistent experience.
