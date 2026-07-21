# Load-Balancing Algorithms

rplb implements ten load-balancing algorithms. Choose based on your traffic pattern, session requirements, and backend heterogeneity. All algorithms share the same `Balancer` interface and compose cleanly with the wrapper chain: `OutlierDetection(SlowStart(ZoneAware(PriorityTiers(base))))`.

---

## Algorithm comparison

| Algorithm | Config value | Best for | Weighted? | Session affinity | Latency-aware |
|-----------|-------------|----------|-----------|-----------------|--------------|
| Round Robin | `round_robin` | Uniform stateless traffic | No | No | No |
| Smooth Weighted Round Robin | `swrr` | Heterogeneous capacity, no burst | Yes | No | No |
| Weighted | `weighted` | Static weight distribution | Yes | No | No |
| Least Connections | `least_conn` | Long-lived or variable-cost requests | No | No | No |
| Weighted Least Conn | `weighted_least_conn` | Heterogeneous backends + variable cost | Yes | No | No |
| Weighted Random | `weighted_random` | Simple weighted probabilistic distribution | Yes | No | No |
| Power of Two Choices | `p2c` | High-throughput, low-overhead least-conn | No | No | No |
| Consistent Hash | `consistent_hash` | Cache locality, session pinning | No | Yes (key-based) | No |
| EWMA | `ewma` | Latency-sensitive APIs | No | No | Yes |
| IP Hash | `ip_hash` | Client IP stickiness without cookie overhead | No | Yes (IP-based) | No |

---

## Round Robin (`round_robin`)

The simplest algorithm. Backends are selected in sequence using an atomic counter. No weights, no state, no overhead beyond a modulo operation.

```yaml
load_balancer:
  algorithm: round_robin
```

Use when: backends are homogeneous and requests are similarly priced.

Avoid when: backends differ in capacity, requests vary significantly in duration, or you need session affinity.

---

## Smooth Weighted Round Robin (`swrr`)

Nginx-style smooth weighted round-robin. Each backend accumulates its weight on every selection round; the backend with the highest current weight is chosen and has the total weight subtracted from it. This produces a smooth interleaving — a backend with weight 5 out of 7 total is not selected 5 times in a row, but distributed across the sequence.

```yaml
load_balancer:
  algorithm: swrr

backends:
  - url: "http://large:8000"
    weight: 5
  - url: "http://medium:8000"
    weight: 2
```

Use when: backends have different capacities and you want accurate weight enforcement without burst.

---

## Weighted (`weighted`)

Weighted random with deterministic seed. Simpler than SWRR but produces less smooth distributions under low concurrency. Suitable for very unequal capacity ratios where smoothness is not required.

```yaml
load_balancer:
  algorithm: weighted
```

---

## Least Connections (`least_conn`)

Always selects the backend with the fewest active (in-flight) connections. rplb tracks connections via the reserve-on-select mechanism: `Next()` atomically increments the in-flight counter; the proxy decrements it on response completion. This makes the count accurate even before the upstream ACKs the connection.

```yaml
load_balancer:
  algorithm: least_conn
```

Use when: request duration varies significantly (e.g., a mix of fast health checks and slow uploads).

---

## Weighted Least Connections (`weighted_least_conn`)

Selects the backend that minimizes `active_connections / weight`. Combines capacity weighting with connection awareness.

```yaml
load_balancer:
  algorithm: weighted_least_conn

backends:
  - url: "http://big:8000"
    weight: 4
  - url: "http://small:8000"
    weight: 1
```

---

## Weighted Random (`weighted_random`)

Selects a backend by weighted random sampling. Uses `crypto/rand`-seeded randomness to prevent correlation across replicas. Lower overhead than SWRR at the cost of distribution smoothness over small samples.

```yaml
load_balancer:
  algorithm: weighted_random
```

---

## Power of Two Choices (`p2c`)

Samples two backends at random and selects the one with fewer active connections. This achieves near-optimal load distribution (O(log log n) maximum load vs O(log n / log log n) for random) with O(1) work per selection — no global scan of backends.

```yaml
load_balancer:
  algorithm: p2c
```

Use when: you need least-conn behavior at very high throughput where scanning all backends is too expensive. P2C outperforms pure least_conn under high concurrency because it avoids the herd effect (many threads simultaneously picking the same backend with 0 connections).

See [P2C deep dive](p2c.md) for the math.

---

## Consistent Hash (`consistent_hash`)

Maps a request key to a point on a virtual node ring. The nearest clockwise backend owns that key. With bounded-load enforcement, no backend accepts more than `load_factor × (total_requests / n_backends)` requests, preventing hot spots.

```yaml
load_balancer:
  algorithm: consistent_hash
  consistent_hash:
    replicas: 100      # virtual nodes per backend
    load_factor: 1.25  # maximum imbalance factor
```

The default key is the full request URL. For session affinity you typically use the session or user ID via a custom header:

```yaml
routes:
  - name: websocket
    path_prefix: "/ws/"
    algorithm: consistent_hash
```

Use when: you need cache locality (route same session to same backend), or WebSocket/gRPC stream stickiness.

See [Consistent Hash deep dive](consistent-hash.md) for ring construction and bounded-load algorithm.

---

## Peak EWMA (`ewma`)

Tracks an exponentially weighted moving average of backend response latency and selects the backend with the lowest EWMA score, adjusted for queue depth:

```
score = latency_ewma * (active_connections + 1)
```

The "peak" variant uses the maximum of the EWMA and the current sample, which reacts quickly to latency spikes without overweighting transient noise.

```yaml
load_balancer:
  algorithm: ewma
```

Use when: backends have heterogeneous or time-varying latency and you want automatic traffic shedding toward faster nodes.

---

## IP Hash (`ip_hash`)

Hashes the client IP address (after trusted-proxy header stripping) to a consistent backend. Simpler than consistent hash — no virtual ring, no bounded loads — and suitable when the client IP distribution is known to be uniform.

```yaml
load_balancer:
  algorithm: ip_hash
```

---

## Composable wrappers

Regardless of which base algorithm you choose, the default group's request path applies this wrapper chain:

```
OutlierDetection(SlowStart(ZoneAware(PriorityTiers(base))))
```

Each wrapper narrows the candidate set passed to the next layer:

| Wrapper | Effect |
|---------|--------|
| `PriorityTiers` | Filters backends to the lowest (highest-priority) tier that has healthy members |
| `ZoneAware` | Prefers backends in `server.zone` when `load_balancer.prefer_same_zone: true` |
| `SlowStart` | Linearly ramps new backends' effective weight from 0 to 100% over `slow_start` duration |
| `OutlierDetection` | Temporarily ejects backends whose error rate exceeds `error_rate_threshold` |

All wrappers are lock-free; they reference the same atomic in-flight counters as the underlying algorithm.

---

## Sticky sessions

Any algorithm can be combined with cookie-based session stickiness:

```yaml
load_balancer:
  algorithm: round_robin
  sticky:
    enabled: true
    cookie: "rplb_affinity"
    ttl: 3600s
```

The first request is assigned by the configured algorithm; subsequent requests with the cookie are pinned to the same backend. If the pinned backend is unhealthy, the next algorithm selection is used and the cookie is reissued.
