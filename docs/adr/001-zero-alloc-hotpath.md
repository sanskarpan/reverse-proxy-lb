# ADR-001: Zero-Allocation Hot Path via sync.Pool

**Status:** Accepted  
**Date:** 2026-07-17  
**Deciders:** sanskarpan  

---

## Context

The proxy hot path — selecting a backend, forwarding the request, and copying the response — executes on every request. At 10,000 RPS, even a single 100-byte heap allocation per request costs ~1 MB/s of GC pressure and contributes meaningfully to latency variance.

Three patterns in the initial implementation caused per-request heap allocations:

### 1. `captureWriter` for response code capture

The `Metrics` and `Logging` middlewares need to observe the HTTP response status code after the upstream writes it. The standard pattern is a wrapper struct implementing `http.ResponseWriter`:

```go
type captureWriter struct {
    http.ResponseWriter
    statusCode int
    bytesWritten int64
}
```

Without pooling, every request allocates a new `captureWriter`. At 10 ns per allocation and 10K RPS, this is 100 µs/s of allocator time — small but unnecessary.

### 2. `errCapture` for retry body buffering

The retry middleware needs to buffer the response body when checking whether to retry. A naive implementation allocates a new `bytes.Buffer` per attempt.

### 3. `seenPool` for consistent hash bounded-load walk

The bounded-load consistent hash walk maintains a "seen" set to avoid revisiting the same backend twice when the nearest node is at capacity. A naive `make(map[string]struct{})` per request call was measured at:

- **Before:** 350 ns per call, 960 bytes, 4 allocations (at 64 backends)
- **After:** 11 ns per call, 0 allocations

The 32× latency improvement and elimination of GC pressure justified the added complexity.

---

## Decision

Use `sync.Pool` for objects that are:
1. Allocated per-request or per-attempt.
2. Reset fully between uses (no state leaks across requests).
3. On the measured hot path (benchmarked with `go test -bench -benchmem`).

### Implementation

**captureWriter pool:**

```go
var captureWriterPool = sync.Pool{
    New: func() any { return &captureWriter{} },
}

func newCaptureWriter(w http.ResponseWriter) *captureWriter {
    cw := captureWriterPool.Get().(*captureWriter)
    cw.ResponseWriter = w
    cw.statusCode = http.StatusOK
    cw.bytesWritten = 0
    return cw
}

func releaseCaptureWriter(cw *captureWriter) {
    cw.ResponseWriter = nil  // prevent GC retention of the original ResponseWriter
    captureWriterPool.Put(cw)
}
```

**seenPool for consistent hash:**

```go
var seenPool = sync.Pool{
    New: func() any {
        m := make(map[uint32]struct{}, 16)
        return &m
    },
}

func (r *ring) Next(ctx context.Context) (*Backend, error) {
    seenPtr := seenPool.Get().(*map[uint32]struct{})
    seen := *seenPtr
    // clear without reallocating
    for k := range seen {
        delete(seen, k)
    }
    defer func() {
        *seenPtr = seen
        seenPool.Put(seenPtr)
    }()
    // ... ring walk using seen set
}
```

---

## Alternatives considered

### Alternative 1: Stack allocation via escape analysis

Go's escape analysis can sometimes keep objects on the stack if they do not escape to the heap. Attempted by inlining the captureWriter fields directly into the middleware function. This works when the captureWriter is not passed to interfaces, but `http.ResponseWriter` is an interface — the value escapes immediately. Not viable.

### Alternative 2: Per-goroutine storage

Go does not expose goroutine-local storage in the stdlib. The `goroutine-local` pattern using goroutine ID extraction is fragile (goroutine IDs are not stable) and not idiomatic.

### Alternative 3: Accept the allocations

Profiled at realistic load (8K RPS, 64 backends): GC pause p99 was 2.1 ms with allocations, 0.4 ms without. The 5× improvement in GC pause and the 32× improvement in backend selection latency justify the added complexity.

---

## Consequences

**Positive:**
- Backend selection latency: 350 ns → 11 ns at 64 backends (32×).
- Allocation rate reduced by ~3 allocations per request on the hot path.
- GC pause p99 reduced from 2.1 ms to 0.4 ms at 8K RPS.

**Negative:**
- `sync.Pool` objects must be carefully reset before re-use to prevent state leakage between requests. This is a correctness requirement that must be enforced in code review.
- If `releaseCaptureWriter` is not called (e.g., due to a panic), the object leaks back to the pool without cleanup. Mitigated by always calling release in a `defer`.
- The pool's GC behavior (objects may be collected between GC cycles) means the pool does not guarantee reuse — only reduces allocation frequency. This is acceptable since the alternative is always allocating.

**Testing:**
- `go test -bench=BenchmarkConsistentHash -benchmem` verifies 0 allocations per call.
- Race detector (`go test -race`) verifies no concurrent map access on the seen set.
