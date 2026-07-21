# ADR-002: Seeding math/rand from crypto/rand

**Status:** Accepted  
**Date:** 2026-07-17  
**Deciders:** sanskarpan  

---

## Context

Several components in rplb use pseudo-random number generation:

- `weighted_random`: randomly samples backends proportional to weight.
- `p2c`: randomly samples two backend indices.
- `fault_injection`: determines whether to inject a delay or abort.
- `canary`: determines whether to route to the canary group.
- `mirror`: determines whether to mirror a request.
- Retry backoff: full-jitter exponential backoff.

The default seed for `math/rand` in Go versions before 1.20 is `1` — a constant. This means all goroutines and all replicas start with identical random sequences.

### The replica correlation problem

Consider a weighted_random deployment with 10 proxy replicas all seeded at `1`. At startup:
- All 10 replicas generate identical random sequences.
- Replica 1 selects backend A, replica 2 selects backend A, ..., replica 10 selects backend A.
- Instead of distributing load across backends, all replicas simultaneously agree on the same backend.

Under high load, this produces correlated traffic spikes: all replicas pile onto the same backend at the same time, violating the intended weight distribution.

The same problem affects `p2c` (all replicas sample the same pair), `fault_injection` (all replicas inject faults at the same requests), and `canary` (all replicas agree on the same dice roll, making the canary weight effectively binary at the replica level rather than distributional).

### Go 1.20+ global rand

Go 1.20 introduced automatic random seeding of the global `math/rand` source. However:
1. rplb targets Go 1.21+, so the global source is auto-seeded.
2. Components using `math/rand.New(math/rand.NewSource(...))` with a fixed seed bypass the global source.
3. Any code path that explicitly passes a seed for reproducibility in tests must not use that seed in production.

---

## Decision

All production random sources in rplb are seeded from `crypto/rand.Read`:

```go
package randutil

import (
    "crypto/rand"
    "encoding/binary"
    "math/rand"
)

// NewRand returns a new math/rand.Rand seeded from crypto/rand.
// Each call returns an independent source — use this to create per-component
// Rand instances rather than sharing the global source.
func NewRand() *rand.Rand {
    var seed int64
    if err := binary.Read(rand.Reader, binary.LittleEndian, &seed); err != nil {
        // crypto/rand.Reader should never fail on any supported OS.
        // Panic is appropriate — if we cannot get entropy, something is
        // seriously wrong with the runtime environment.
        panic("randutil: failed to read from crypto/rand: " + err.Error())
    }
    return rand.New(rand.NewSource(seed))
}
```

Each stateful component (weighted_random balancer, fault injector, canary splitter, etc.) creates its own `*rand.Rand` at construction time via `randutil.NewRand()`. This ensures:

1. Seeds are unpredictable to an attacker (entropy from the OS CSPRNG).
2. Replicas produce uncorrelated sequences (each replica draws a different seed).
3. Components are independent — one component's Rand state does not affect another's.

### Thread safety

`math/rand.Rand` is not safe for concurrent use. Each component that uses a `*rand.Rand` either:
- Holds its own mutex around Rand calls.
- Creates a new `Rand` per goroutine using a `sync.Pool`.

The global `math/rand` functions (e.g., `rand.Intn`) are safe for concurrent use (they use a mutex internally), but components create their own sources to avoid contention on the global lock.

---

## Alternatives considered

### Alternative 1: Use the global math/rand (Go 1.20+)

Go 1.20+ auto-seeds the global `math/rand` source from `runtime/rand`, which itself draws from the OS. This would eliminate the need for per-component seeding.

Rejected because:
- Components that use their own `*rand.Rand` (for performance — no global mutex) would still need explicit seeding.
- Tests that pin a seed for reproducibility might accidentally leave a fixed seed in production code.
- Per-component sources make it easier to reason about independence.

### Alternative 2: crypto/rand directly

Use `crypto/rand.Read` on every call instead of seeding `math/rand`. This gives cryptographically secure randomness.

Rejected because:
- `crypto/rand.Read` is a system call. At 10K RPS with multiple random decisions per request, this would add significant overhead.
- Statistical security is sufficient for load balancing and fault injection — cryptographic security is not required.

### Alternative 3: Accept the default seed (Go < 1.20 behavior)

Rejected as a correctness issue. The replica correlation problem is not theoretical — it has been observed in production deployments of weighted load balancers that use a fixed seed.

---

## Consequences

**Positive:**
- Load distribution is accurate across replicas — no correlated backend selection.
- Canary weight is truly distributional across replicas, not binary.
- Fault injection and mirror sampling behave independently across replicas.

**Negative:**
- Tests that require reproducible random behavior cannot use the production `randutil.NewRand()` — they must construct their own `rand.Rand` with a fixed seed. This requires test-only seeding paths, which adds a small maintenance burden.
- `panic` on `crypto/rand.Read` failure is a harsh failure mode. On all supported operating systems and container runtimes, `/dev/urandom` is available at process startup. The panic has never been triggered in practice.
