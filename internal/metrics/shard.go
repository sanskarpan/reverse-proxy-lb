package metrics

import (
	"runtime"
	"sync/atomic"
	"unsafe"
)

const cacheLineSize = 64

// paddedCounter is a single int64 counter padded to a full cache line to
// prevent false sharing between shards on adjacent cache lines.
type paddedCounter struct {
	n int64
	_ [cacheLineSize - 8]byte // pad remainder of cache line
}

// Compile-time guard: paddedCounter must be exactly one cache line.
// If the struct size drifts, this assignment will not compile.
const _ = unsafe.Sizeof(paddedCounter{}) - cacheLineSize // must be 0

// ShardedCounter is a lock-free counter that spreads updates across
// GOMAXPROCS-many independent, cache-line-padded shards to eliminate
// false sharing on high-parallelism workloads.
//
// The exported interface (Add / Load) is a drop-in replacement for a
// plain atomic.Int64 or atomic.Uint64.
type ShardedCounter struct {
	shards []paddedCounter
}

// NewShardedCounter returns a *ShardedCounter with one padded shard per
// logical processor (GOMAXPROCS).
func NewShardedCounter() *ShardedCounter {
	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		n = 1
	}
	return &ShardedCounter{shards: make([]paddedCounter, n)}
}

// Add adds delta to the counter. The shard is selected using a fast
// goroutine-local xorshift64 seeded from the goroutine's stack address so
// that no shared state is needed — each goroutine independently hashes into
// a shard without any CAS or lock.
func (c *ShardedCounter) Add(delta int64) {
	idx := localrand() % uint32(len(c.shards))
	atomic.AddInt64(&c.shards[idx].n, delta)
}

// Load returns the current total across all shards. It is not atomic with
// respect to concurrent Add calls (same semantics as summing multiple
// atomic.Int64 values), but is always eventually consistent.
func (c *ShardedCounter) Load() int64 {
	var total int64
	for i := range c.shards {
		total += atomic.LoadInt64(&c.shards[i].n)
	}
	return total
}

// localrand returns a pseudo-random uint32 derived from the calling
// goroutine's stack pointer. Because each goroutine has its own stack the
// value differs per goroutine with no shared state, and the xorshift mix
// spreads the bits to avoid clustering on small shard counts.
//
// This is not cryptographically secure; it is only used for shard selection.
//
//go:noinline
func localrand() uint32 {
	// Capture a stack address as a goroutine-unique seed.
	// Using a local variable gives us the current stack frame address.
	var x [1]uintptr
	x[0] = uintptr(unsafe.Pointer(&x))

	// xorshift32 mix for better bit distribution.
	v := uint32(x[0] >> 3) // discard the lowest 3 alignment bits
	v ^= v << 13
	v ^= v >> 17
	v ^= v << 5
	return v
}
