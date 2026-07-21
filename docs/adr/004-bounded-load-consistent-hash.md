# ADR-004: Bounded-Load Consistent Hashing over Plain Consistent Hashing

**Status:** Accepted  
**Date:** 2026-07-17  
**Deciders:** sanskarpan  

---

## Context

Consistent hashing is widely used for session affinity and cache locality. The classic algorithm (Karger et al., 1997) distributes `m` keys across `n` backends using a virtual node ring, remapping only `m/n` keys when a backend is added or removed.

However, plain consistent hashing has a well-known pathology: **hot spots**.

### Hot spot scenarios

**1. Non-uniform key distribution**

If request keys follow a power-law distribution (e.g., a small number of popular user IDs generate most traffic), the backends owning those popular key ranges receive disproportionate load. The ring structure does not account for request frequency, only key assignment.

**Example:**
- 4 backends, 100 virtual nodes each (400 ring positions).
- Key `popular-user-12345` maps to backend B.
- Backend B receives 40% of all requests because `popular-user-12345` makes 40% of the traffic.
- Other backends receive 20% each.

**2. Backend failure cascade**

When backend B fails, its keys remap to the next clockwise backend (C). If B was handling 25% of traffic, C suddenly handles 50%. If C cannot handle 2× its normal load, it also fails — cascade.

**3. Uneven virtual node distribution**

With 100 virtual nodes per backend and 4 backends, the expected arc length for each backend is 25% of the ring. But with random placement, the actual distribution has high variance for small node counts. A backend that happens to own a large arc segment gets more keys regardless of request frequency.

---

## Decision

Implement bounded-load consistent hashing (Mirrokni, Thorup, Zadimoghaddam, 2018) with a configurable `load_factor`.

### Algorithm

The bounded-load constraint adds a capacity check to the standard ring walk:

```go
func (r *ring) Next(ctx context.Context) (*Backend, error) {
    key := requestKey(ctx)
    hash := fnv32(key)

    // Compute the load bound for this selection
    totalInflight := r.totalInflight.Load()
    nHealthy := int64(len(r.healthyList))
    bound := int64(math.Ceil(loadFactor * float64(totalInflight+1) / float64(nHealthy)))

    seen := seenPool.Get()  // zero-alloc, from ADR-001
    defer seenPool.Put(seen)

    // Walk clockwise from the hash position
    pos := sort.Search(len(r.nodes), func(i int) bool {
        return r.nodes[i].hash >= hash
    })
    if pos == len(r.nodes) {
        pos = 0  // wrap around
    }

    for i := 0; i < len(r.nodes); i++ {
        node := r.nodes[(pos+i)%len(r.nodes)]
        b := node.backend

        if !b.IsHealthy() {
            continue
        }
        if seen.contains(b.URL) {
            continue  // already tried this backend via multiple virtual nodes
        }

        if b.InFlight.Load() < bound {
            return b, nil  // backend is within load bound
        }

        seen.add(b.URL)
        // Continue to next virtual node (overflow)
    }

    // All backends are at or above the bound; fallback to least-loaded
    return r.leastLoaded()
}
```

### Load factor semantics

`load_factor: 1.25` means no backend handles more than 25% above the average load:

```
average_load = total_inflight / n_healthy_backends
bound        = ceil(load_factor × (total_inflight + 1) / n_healthy_backends)
             = ceil(1.25 × average_load)
```

When all backends are below the bound, selection is identical to plain consistent hashing. The bound only activates when a backend is overloaded relative to the fleet.

### Configuration

```yaml
load_balancer:
  algorithm: consistent_hash
  consistent_hash:
    replicas: 100
    load_factor: 1.25
```

Recommended `load_factor` values:

| Use case | `load_factor` |
|---------|--------------|
| Cache locality (some imbalance ok) | 1.5–2.0 |
| Balanced (default) | 1.25 |
| Strict balance (sacrifices locality) | 1.05–1.1 |
| Exact balance (identical to least_conn under uniform keys) | 1.0 |

---

## Alternatives considered

### Alternative 1: Plain consistent hash (no bounds)

The original Karger algorithm without load checking.

Rejected as the default because:
- Hot spots are common in practice. User ID distributions are almost never uniform.
- Backend failure cascades are a correctness concern, not just a performance concern.
- The bounded-load overhead is minimal: the fast path (backend below bound) adds one atomic load comparison.

Kept as a configuration option: set `load_factor: 0` to disable bounds (falls back to plain consistent hash walk behavior).

### Alternative 2: Rendezvous hashing (highest random weight)

Each backend computes `HRW(key, backend_id)` and the backend with the highest score wins. No ring structure.

Considered but rejected because:
- O(n) computation per selection (must compute HRW for all backends).
- No natural mechanism for load bounds without global state.
- Less intuitive than ring-based CH for operators to reason about.

### Alternative 3: Jump consistent hash

A space-efficient hash function that maps keys to backends in O(ln n) time. Used in databases for shard assignment.

Rejected because:
- No bounded-load mechanism.
- Sequential backend numbering required — does not support arbitrary backend URLs as keys.
- Backend removal requires renumbering, causing `(n-1)/n` key remapping vs `1/n` for ring CH.

### Alternative 4: Maglev consistent hash

Google's Maglev table-based consistent hashing. Fast O(1) lookup via precomputed table.

Rejected because:
- Table recomputation on backend change is expensive for large tables.
- No built-in bounded-load mechanism.
- Overkill for load balancer use cases (vs Maglev's ECMP use case).

---

## Consequences

**Positive:**
- Hot spots are bounded by `load_factor`. A backend receiving a disproportionate key range cannot receive more than `load_factor × average` load.
- Backend failure cascade is damped: a failed backend's load distributes across all healthy backends up to the bound, not piling entirely onto the next node.
- Key locality is preserved for the common case (backend below bound): the same key always reaches the same backend first, and only overflows when that backend is saturated.

**Negative:**
- The seen-set overhead (mitigated by `sync.Pool` from ADR-001) adds complexity to the ring walk.
- At very high `total_inflight` counts, `bound` grows large and the bounded-load constraint rarely activates — effectively degenerating to plain CH. This is acceptable: when all backends are lightly loaded, CH locality is the priority.
- The `leastLoaded()` fallback (when all backends are at or above the bound) breaks CH locality. This only occurs under extreme overload and is preferable to returning an error.
- Property-based tests (in `internal/balancer/distribution_test.go`) verify chi-squared uniformity and bounded-load invariants under synthetic key distributions.
