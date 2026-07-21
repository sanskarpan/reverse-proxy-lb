# Power of Two Choices (P2C)

Power of Two Choices (P2C) is a randomized load-balancing algorithm that achieves near-optimal distribution with O(1) work per selection — making it the preferred replacement for `least_conn` under high concurrency.

---

## How it works

Instead of scanning all backends to find the minimum, P2C samples exactly two backends at random and selects the one with fewer active connections:

```
1. Choose backend A at random from the healthy pool.
2. Choose backend B at random (B ≠ A).
3. Select whichever has fewer active (in-flight) connections.
   Ties broken by index order.
```

That is the entire algorithm. No global minimum scan. No lock on the backend list beyond the in-flight counter atomics.

---

## The math behind it

Under pure random (one choice), the maximum load on any backend follows:

```
E[max load] ≈ Θ(log n / log log n)
```

where `n` is the number of backends. With **two** independent random choices (P2C), this collapses dramatically:

```
E[max load] ≈ Θ(log log n)
```

This is the "power of two choices" result from the balls-into-bins problem (Azar, Broder, Karlin, Upfal 1994). The key insight: you do not need to find the global minimum — just breaking symmetry with one comparison yields exponentially better balance.

For 64 backends:
- Random: max load ≈ 4.1× average
- P2C: max load ≈ 1.7× average
- Scan all (least_conn): max load ≈ 1.0× average (at the cost of contention)

P2C is the sweet spot between accuracy and overhead.

---

## Why P2C beats `least_conn` at high concurrency

Pure `least_conn` requires reading the in-flight counter of every healthy backend to find the minimum. Under high concurrency this creates a herd effect: multiple goroutines simultaneously read counters, all see backend X at 0, all select backend X, and X is immediately overloaded.

P2C avoids the herd effect because:

1. No goroutine examines all backends — only two.
2. Two goroutines choosing the same pair `(A, B)` independently still select the less-loaded one, which may differ by the time the second goroutine checks.
3. The atomic in-flight counter per backend is the only shared state.

---

## Configuration

```yaml
load_balancer:
  algorithm: p2c
```

P2C has no algorithm-specific tuning knobs. It respects all the standard wrapper options:

```yaml
load_balancer:
  algorithm: p2c
  prefer_same_zone: true          # zone-aware wrapper
  slow_start: 30s                 # ramp up new backends
  outlier_detection:
    enabled: true
    error_rate_threshold: 0.3
```

---

## Comparison with `least_conn`

| Property | `least_conn` | `p2c` |
|----------|-------------|-------|
| Selection work | O(n) scan | O(1) — two samples |
| Maximum load imbalance | Minimal (near-optimal) | log log n (very good) |
| Herd effect under burst | Yes — thundering herd on "idle" backends | No — random sampling avoids coordinated picks |
| Suited for | < 20 backends, moderate concurrency | Any pool size, high concurrency |
| Weighted backends | No (use `weighted_least_conn`) | No |

---

## When to use P2C

Use P2C when:
- You have a large backend pool (20+ backends).
- Requests have variable cost (e.g., mixed fast + slow endpoints).
- You experience thundering herd with `least_conn` (all goroutines pick the same backend on a burst).

Avoid P2C when:
- Backends have significantly different weights — use `weighted_least_conn` instead.
- You need session affinity — use `consistent_hash` with `load_factor` tuning.
- Pool size is 2 — P2C degenerates to always selecting the less-loaded of two, which is optimal but has a tiny overhead vs simply alternating.

---

## Implementation note

rplb's P2C implementation samples from the **healthy-backend-list cache** rather than the full backend list. This cache is invalidated only when a backend's health state changes (via a global epoch counter). Under stable conditions the hot path is:

1. Load the cached healthy list — 0 allocations, 11 ns at 64 backends.
2. Pick two random indices.
3. Compare two atomic int64 loads.
4. Return the winner.

The random source is `crypto/rand`-seeded at startup (see [ADR-002](../adr/002-crypto-rand-seed.md)) to prevent all replicas from sampling the same sequence.
