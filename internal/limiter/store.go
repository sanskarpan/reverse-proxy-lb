package limiter

import (
	"sync"
	"time"
)

// Store is a shared rate-limit backend. Multiple RateLimiter instances that share one
// Store enforce a COMBINED limit — the abstraction point for distributed rate limiting
// (ENHANCEMENTS 4.4). MemStore shares state within one process; a Redis-backed Store
// implementing this same interface (with a server-side GCRA/token-bucket Lua script)
// provides true multi-instance limiting across a fleet, without changing any caller.
type Store interface {
	// Allow atomically admits one unit against key's rps/burst budget, returning
	// whether it was admitted and, if not, roughly how long until it would be.
	Allow(key string, rps float64, burst int, now time.Time) (allowed bool, retryAfter time.Duration)
}

// MemStore is an in-memory Store shared by pointer across RateLimiter instances. It
// uses the same GCRA (leaky-bucket-as-meter) algorithm as the local gcra limiter, so
// its admission semantics match. Safe for concurrent use.
type MemStore struct {
	mu      sync.Mutex
	buckets map[string]time.Time // key -> theoretical arrival time (TAT)
}

// NewMemStore returns an empty in-memory shared store.
func NewMemStore() *MemStore { return &MemStore{buckets: make(map[string]time.Time)} }

// Allow implements Store using GCRA.
func (m *MemStore) Allow(key string, rps float64, burst int, now time.Time) (bool, time.Duration) {
	if rps <= 0 {
		return true, 0
	}
	if burst < 1 {
		burst = 1
	}
	emission := time.Duration(float64(time.Second) / rps)
	tolerance := emission * time.Duration(burst-1)

	m.mu.Lock()
	defer m.mu.Unlock()

	tat, ok := m.buckets[key]
	if !ok || tat.Before(now) {
		tat = now
	}
	newTat := tat.Add(emission)
	allowAt := newTat.Add(-tolerance)
	if now.Before(allowAt) {
		return false, allowAt.Sub(now)
	}
	m.buckets[key] = newTat
	return true, 0
}
