# Consistent Hashing with Bounded Loads

Consistent hashing maps requests to backends such that adding or removing a backend only remaps `1/n` of keys on average — not a full reshuffling. rplb extends the classic ring with **bounded loads** to prevent hot spots and a **zero-alloc hot path** using `sync.Pool`-recycled seen-set buffers.

---

## The virtual node ring

Each backend is placed at `replicas` positions (virtual nodes) on a 32-bit hash ring. More virtual nodes produce a more uniform distribution. The default of 100 virtual nodes per backend gives good uniformity for pool sizes up to ~100 backends.

```
ring positions (0 ... 2^32-1):
  backend-A: [ 1043, 7821, 15432, ..., 4294900210 ]   ← 100 positions
  backend-B: [ 3201, 8944, 17023, ..., 4294933091 ]
  backend-C: [ 509,  6332, 14011, ..., 4294966000 ]
```

To select a backend for a key:

1. Hash the key to a uint32 ring position.
2. Walk clockwise to the first virtual node position >= key hash.
3. The backend that owns that virtual node is the candidate.

This is an O(log n) binary search on a sorted slice — fast and allocation-free.

---

## Bounded loads

Plain consistent hashing can produce hot spots when:
- The key distribution is non-uniform (e.g., a power-law user-ID distribution).
- Backends fail and their keys pile onto neighbors.

rplb adds the Karger/Mirrokni bounded-load constraint: a backend is only selected if its current load is below the bound:

```
bound = ceil(load_factor × (total_inflight + 1) / n_healthy_backends)
```

With `load_factor: 1.25`, no backend handles more than 25% above the average. When the nearest backend is at capacity, the ring walk continues to the next candidate.

```yaml
load_balancer:
  algorithm: consistent_hash
  consistent_hash:
    replicas: 100
    load_factor: 1.25
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `replicas` | int | `100` | Virtual nodes per backend — more = more uniform distribution |
| `load_factor` | float | `1.25` | Maximum multiplier above average load (1.0 = perfectly even, higher = more locality at cost of balance) |

---

## Key selection

The hash key defaults to the full request URL (`scheme://host/path?query`). Override per route:

```yaml
routes:
  - name: "user-sessions"
    path_prefix: "/api/"
    algorithm: consistent_hash
    # Key is derived from the request — customize via header rewrite if needed
```

For WebSocket sticky routing, combine consistent hash with a user-ID header:

```yaml
rewrite:
  request_headers_set:
    X-Session-Key: "${cookie:session_id}"

routes:
  - name: "ws"
    path_prefix: "/ws/"
    algorithm: consistent_hash
```

---

## Zero-alloc implementation

The bounded-load walk maintains a "seen" set to avoid revisiting the same backend twice during overflow resolution. A naive implementation would `make(map[string]struct{})` on every request — expensive at high QPS.

rplb uses a `sync.Pool` of pre-allocated seen-set structs (`seenPool`). The pool is acquired at the start of backend selection and released immediately after — zero heap allocations on the hot path.

This is the subject of [ADR-001](../adr/001-zero-alloc-hotpath.md) and [ADR-004](../adr/004-bounded-load-consistent-hash.md).

---

## Behavior on backend removal

When a backend is removed (health failure or config reload):

1. Its virtual nodes are removed from the ring slice.
2. Its keys remap to the next clockwise owner.
3. Only `1/n` of keys are remapped — other backends are unaffected.
4. In-flight reservations on the removed backend continue until completion; no new requests are assigned.

---

## When to use consistent hashing

| Scenario | Recommended? |
|----------|-------------|
| Cache-coherent routing (same user → same backend) | Yes |
| WebSocket / long-lived connections | Yes |
| gRPC streams requiring server affinity | Yes |
| Stateless REST APIs | No — round-robin or P2C is simpler |
| Highly skewed key distributions | Use with low `load_factor` (1.1–1.2) |

---

## Example: Redis cluster proxy

```yaml
backends:
  - url: "http://redis-1:6379"
    weight: 1
  - url: "http://redis-2:6379"
    weight: 1
  - url: "http://redis-3:6379"
    weight: 1

load_balancer:
  algorithm: consistent_hash
  consistent_hash:
    replicas: 150    # higher for smaller pools
    load_factor: 1.1 # tighter bound for cache consistency
```

With `replicas: 150` and 3 backends, the ring has 450 positions, which is sufficient to achieve < 5% standard deviation in key distribution.
