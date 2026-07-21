# Retry and Hedged Requests

rplb provides two mechanisms for tolerating transient backend failures: **retries** (sequential attempts after failure) and **hedging** (parallel speculative requests raced against each other). They serve different failure modes and can be used together.

---

## Retries

When a request fails for a retryable reason, the proxy attempts the request again on a (potentially different) backend, subject to a retry budget.

### Retry-eligible conditions

| Condition | Default? | Notes |
|-----------|---------|-------|
| `connect` | Yes | TCP connection failure |
| `timeout` | Yes | `per_try_timeout` exceeded |
| `5xx` | No | Opt-in via `retry_on: [5xx]` — only safe for idempotent methods |

Non-idempotent methods (`POST`, `PATCH`, `DELETE`) are only retried on pure connection errors, never on 5xx responses. The proxy does not buffer the request body for non-idempotent methods.

### Backoff

Retries use **full jitter exponential backoff**:

```
wait = random(0, min(max_backoff, base * 2^attempt))
```

Full jitter prevents thundering herds when many clients simultaneously retry after a failure event.

### Retry budget

The budget (`retry.budget`) caps the total number of concurrent retries across all in-flight requests. Setting `budget: 10` means at most 10 request slots are consumed by retries at any time. This prevents a sudden backend failure from causing all traffic to double-retry, amplifying load on surviving backends.

```yaml
retry:
  max_attempts: 3
  backoff: exponential
  max_backoff: 10s
  budget: 10
  per_try_timeout: 5s
  honor_retry_after: true
  retry_on:
    - connect
    - timeout
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `max_attempts` | int | `3` | Maximum total attempts (1 = no retries) |
| `backoff` | string | `exponential` | `exponential` or `linear` |
| `max_backoff` | duration | `10s` | Backoff ceiling |
| `budget` | int | `0` | Maximum concurrent retry slots (0 = unlimited) |
| `per_try_timeout` | duration | — | Timeout per individual attempt, independent of the request-level timeout |
| `honor_retry_after` | bool | `true` | Respect the `Retry-After` response header when present |
| `retry_on` | []string | `[connect, timeout]` | Error classes that trigger retry |

---

## Hedged Requests

Hedging issues additional copies of the request to different backends after a configurable delay, then races them. The first response that arrives wins; all losers are cancelled. This eliminates long-tail latency caused by slow backends without increasing error rates.

### How hedging works

```
t=0ms    → primary request sent to backend-A
t=50ms   → hedge.delay elapsed; backend-A has not responded
           → hedge request #1 sent to backend-B
t=80ms   → backend-B responds with 200
           → backend-A's request is cancelled; reservation released
           → 200 response returned to client

Total client latency: 80ms
Without hedging: would have waited until backend-A responded or timed out
```

The hedge delay (`hedge.delay`) should be set to approximately the p95 latency of the slowest tolerable response. Requests that respond faster than the delay never generate a hedge — overhead is zero on the fast path.

### Idempotency constraint

Hedged requests are only issued for **idempotent** methods: `GET`, `HEAD`, `OPTIONS`, `PUT`, `DELETE`. `POST` and `PATCH` are never hedged to avoid duplicate side effects.

### Reservation management

Each hedged request atomically increments the chosen backend's in-flight counter when issued and decrements it when cancelled or completed. The winning request holds its reservation until the response body is fully copied to the client. The implementation guarantees exactly one decrement per backend per hedge cycle — the reservation leak that can occur if the cancel path decrements without a paired increment was identified and fixed during development (see ISSUES.md, defect #29).

### Configuration

```yaml
retry:
  max_attempts: 3

  hedge:
    enabled: true
    delay: 50ms
    max_extra: 2
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `hedge.enabled` | bool | `false` | Enable hedged requests |
| `hedge.delay` | duration | — | Wait time before issuing the first hedge |
| `hedge.max_extra` | int | `1` | Maximum additional hedged requests beyond the primary |

### Hedging vs retries

| Property | Retries | Hedging |
|----------|---------|---------|
| Trigger | Failure (after it completes) | Slowness (after a delay, before completion) |
| Latency impact | Adds delay after failure | Reduces tail latency |
| Upstream load | Only on failure | Always doubles load after hedge.delay |
| Safe for POST? | No (except on connect error) | No |
| Best for | Transient errors | Long-tail latency (e.g., GC pauses, cold starts) |

They compose naturally: a primary that fails immediately triggers a retry; a primary that stalls past `hedge.delay` triggers a hedge. If the hedge also stalls, the retry budget kicks in.

---

## `per_try_timeout` vs overall timeout

The overall request timeout is set by `server.write_timeout`. `per_try_timeout` adds a tighter per-attempt bound:

```
Overall budget:   server.write_timeout = 30s
Per attempt:      retry.per_try_timeout = 5s
Max attempts:     retry.max_attempts = 3

Worst case:   3 × 5s backoff = 15s  (well within 30s)
```

Without `per_try_timeout`, a single slow backend could consume the entire overall budget, leaving no time for retries.
