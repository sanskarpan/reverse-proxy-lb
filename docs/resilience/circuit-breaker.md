# Circuit Breaker

The circuit breaker prevents cascading failures by stopping traffic to unhealthy backends before health checks can react. It operates at the per-backend level and supports both consecutive-failure and rolling-window modes.

---

## States

```
                        failure_threshold
                        reached
  ┌───────────┐         ─────────────────►   ┌──────────┐
  │           │                               │          │
  │  CLOSED   │                               │   OPEN   │
  │ (normal)  │◄──────────────────────────────│(no traffic)│
  │           │   success_threshold reached   │          │
  └───────────┘   in half-open                └────┬─────┘
                                                   │
                                                   │ after timeout
                                                   ▼
                                             ┌─────────────┐
                                             │  HALF-OPEN  │
                                             │ (probe only) │
                                             └─────────────┘
                                                   │
                                          ┌────────┴──────────┐
                                    success              failure
                                    ▼                         ▼
                                 CLOSED                    OPEN
```

| State | Behavior |
|-------|---------|
| **Closed** | Normal operation. Failures are counted. |
| **Open** | All requests to this backend are rejected immediately. Failover kicks in. |
| **Half-Open** | A single probe request is allowed through. Success closes the circuit; failure reopens it and resets the timeout. |

---

## Modes

### Consecutive mode (default)

Trips after `failure_threshold` consecutive failures. Resets the counter on any success.

```yaml
circuit_breaker:
  enabled: true
  mode: consecutive
  failure_threshold: 5
  success_threshold: 2
  timeout: 30s
  trip_on:
    - connect
    - timeout
```

### Rolling window mode

Trips when the error rate in a sliding time window exceeds `error_rate_threshold`, given at least `min_requests` have been made.

```yaml
circuit_breaker:
  enabled: true
  mode: rolling
  rolling_window: 10s
  error_rate_threshold: 0.5
  min_requests: 20
  success_threshold: 2
  timeout: 30s
  trip_on:
    - connect
    - timeout
    - 5xx
```

The rolling window is implemented as a ring of time-bucket counters (not a per-request deque), making it O(1) in both time and memory.

---

## Error classification (`trip_on`)

| Class | What counts |
|-------|-------------|
| `connect` | TCP connection refused, connection reset, dial timeout |
| `timeout` | Request exceeded `per_try_timeout` or `write_timeout` |
| `5xx` | Upstream returned a 5xx HTTP status code |

Default: `[connect, timeout]` — 5xx errors do not trip the circuit unless explicitly added. This is intentional: a backend returning 500 for bad inputs should not be taken out of rotation.

---

## Configuration

```yaml
circuit_breaker:
  enabled: true
  mode: consecutive
  failure_threshold: 5
  success_threshold: 2
  timeout: 30s
  rolling_window: 10s
  error_rate_threshold: 0.5
  min_requests: 20
  trip_on:
    - connect
    - timeout
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable circuit breaker |
| `mode` | string | `consecutive` | `consecutive` or `rolling` |
| `failure_threshold` | int | `5` | Consecutive failures to trip (consecutive mode) |
| `success_threshold` | int | `2` | Consecutive successes to close from half-open |
| `timeout` | duration | `30s` | Time to wait before probing (open → half-open) |
| `rolling_window` | duration | `10s` | Sliding window for rolling mode |
| `error_rate_threshold` | float | `0.5` | Error rate to trip in rolling mode (0.0–1.0) |
| `min_requests` | int | `20` | Minimum requests before the rate is evaluated |
| `trip_on` | []string | `[connect, timeout]` | Error classes that count toward the threshold |

---

## Metrics and observability

The circuit breaker publishes a state gauge that integrates with Prometheus:

```
rplb_backend_circuit_state{backend="http://b1:8000"} 0   # 0=closed, 1=open
```

State transitions are logged at WARN level with the backend URL and reason:

```json
{"level":"WARN","msg":"circuit opened","backend":"http://b1:8000","consecutive_failures":5}
{"level":"INFO","msg":"circuit half-open","backend":"http://b1:8000"}
{"level":"INFO","msg":"circuit closed","backend":"http://b1:8000"}
```

Recommended Prometheus alert:

```promql
rplb_backend_circuit_state{backend=~".*"} == 1
```

---

## Admin API

Force-close a circuit breaker (useful after a confirmed fix):

```bash
curl -XPOST 'http://localhost:9090/admin/circuit/reset?url=http://b1:8000'
```

This transitions the circuit directly from open to closed without waiting for the timeout.

---

## Distributed circuit state (Redis)

In a multi-instance deployment, each instance runs its own circuit breaker independently. If you want instances to share circuit state (e.g., one instance's failures should influence another's counters), enable the Redis sync adapter:

```yaml
circuit_breaker:
  enabled: true
  mode: consecutive
  failure_threshold: 5

  redis:
    enabled: true
    addr: "redis:6379"
    key_prefix: "rplb:circuit:"
    sync_interval: 1s
```

The local circuit is the hot path — every request decision is made locally without a Redis round-trip. The Redis sync pushes state updates asynchronously. On startup, each instance loads the persisted state from Redis to bootstrap its counters.

This is the subject of [ADR-003](../adr/003-distributed-circuit-breaker.md).

---

## Interaction with retries

The circuit breaker runs **before** the retry loop per attempt. If the circuit for backend A is open:

1. The proxy skips A and asks the balancer for the next backend in the same group (failover).
2. If all backends in the group are open, the proxy returns 503 immediately — no retries.

This prevents retry storms from amplifying pressure on an already-failing backend.

---

## Interaction with health checks

Health checks and circuit breakers serve different purposes and operate independently:

| | Health Check | Circuit Breaker |
|-|-------------|----------------|
| Detects | Proactive — periodic HTTP/TCP probe | Reactive — in-request failure counting |
| Reaction time | `interval` seconds | Milliseconds (on `failure_threshold`-th failure) |
| Recovery | `healthy_threshold` successful probes | `success_threshold` successful requests |
| Overhead | Background goroutine per backend | Zero overhead on success path |

A backend can be marked unhealthy by either mechanism. Both must agree the backend is healthy before it receives traffic.
