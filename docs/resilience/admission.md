# Admission Control

Admission control in rplb operates at two levels: **per-backend connection limits** (bulkhead) and **server-level request body limits**. Together they prevent a single slow or abusive client from exhausting memory or saturating backend connection pools.

---

## Per-backend connection limits (bulkhead)

Each backend has a `max_conns` field that caps the number of in-flight requests the proxy will send to it simultaneously. When the limit is reached, the backend is considered "saturated" and is excluded from backend selection — the balancer tries other backends instead.

```yaml
backends:
  - url: "http://api-server:8000"
    max_conns: 100        # max 100 concurrent requests to this backend
  - url: "http://db-proxy:5432"
    max_conns: 20         # database has a small connection pool
```

### Why bulkhead matters

Without a per-backend connection limit, a single slow upstream can absorb all available goroutines:

1. Upstream slows down — response time goes from 10ms to 5000ms.
2. New requests pile up, each blocked waiting for a response.
3. At 1000 RPS with 5s latency, you accumulate 5000 concurrent goroutines.
4. Memory exhausts; process OOMs.

With `max_conns: 100`, once 100 requests are in-flight to the slow backend, new requests are directed to other healthy backends or rejected with 503 if no other backend is available. The 100-connection limit caps memory usage from that backend at a predictable ceiling.

### How it interacts with load balancing

The in-flight counter is the same atomic integer used by the `least_conn`, `p2c`, and `ewma` algorithms for load scoring. When a backend reaches `max_conns`:

- It is excluded from the candidate set before algorithm selection.
- Failover logic may route to a higher-tier backend if all same-tier backends are saturated.
- The `rplb_inflight_requests` metric tracks current concurrency.

---

## Request body size limit

The middleware stack enforces a maximum request body size to prevent large bodies from buffering into memory or filling upstream buffers:

```yaml
server:
  max_request_body_bytes: 10485760    # 10 MiB default
```

When a request body exceeds this limit, the connection is aborted with 413 (Payload Too Large) before the body is read into memory. The limit is enforced by `http.MaxBytesReader` wrapping the request body in the `MaxBytes` middleware.

This protects against:
- Accidental large uploads to endpoints that do not expect them.
- Intentional large-body DoS attacks.

Set to `0` to disable the limit (not recommended for public-facing deployments).

---

## Header size limit

```yaml
server:
  max_header_bytes: 1048576    # 1 MiB (Go default)
```

Go's `net/http` enforces `max_header_bytes` on the incoming request. Requests with headers exceeding this size are rejected with 431 (Request Header Fields Too Large) before any middleware runs.

Reduce this from the default if your clients are known to send small headers and you want to reject oversized header attacks earlier.

---

## ReadHeaderTimeout (Slowloris protection)

```yaml
server:
  read_header_timeout: 10s
```

The `ReadHeaderTimeout` closes connections that take longer than the configured duration to send the complete request headers. This directly mitigates the Slowloris attack pattern, where an attacker opens many connections and sends headers one byte at a time to exhaust file descriptors.

Setting this to 5–10 seconds is appropriate for public-facing deployments. High values (or leaving it at 0/unlimited) expose the server to connection exhaustion.

---

## Idle connection timeout

```yaml
server:
  idle_timeout: 120s
```

Idle keep-alive connections are closed after `idle_timeout`. This reclaims goroutines and file descriptors from clients that open a connection and then stop sending requests.

---

## Complete hardening configuration

```yaml
server:
  host: "0.0.0.0"
  port: 8080
  read_timeout: 30s
  write_timeout: 30s
  idle_timeout: 120s
  read_header_timeout: 10s
  max_header_bytes: 1048576
  max_request_body_bytes: 10485760

backends:
  - url: "http://app:8000"
    max_conns: 200

  - url: "http://app-secondary:8000"
    max_conns: 200
```

---

## Metrics

| Metric | Description |
|--------|-------------|
| `rplb_inflight_requests` | Current number of in-flight requests across all backends |
| `rplb_backend_up{backend}` | 1 if healthy, 0 if health check failing or saturated |

Monitor `rplb_inflight_requests` approaching total `max_conns` as a signal to add backend capacity.

---

## Interaction with queuing

rplb does not implement a request queue in front of backends. When all backends are saturated (at `max_conns`) or unhealthy, the proxy immediately returns 503. This is intentional:

- Queueing adds latency and can mask capacity problems.
- A 503 signals to the client (or upstream load balancer) to apply backpressure immediately.
- Queue depth is bounded naturally by the TCP listen backlog at the OS level.

If you need queuing semantics, place rplb behind an API gateway that supports request queuing and rate limiting.
