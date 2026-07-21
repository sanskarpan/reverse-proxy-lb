package balancer

import (
	"reverse-proxy-lb/internal/config"
	"sync"
	"sync/atomic"
	"time"
)

type Backend struct {
	URL         string
	weight      atomic.Int32
	MaxConns    int
	healthy     atomic.Bool
	ActiveConns int32
	Failures    int32
	Successes   int32
	// Zone is the availability zone/region this backend lives in. Used by the
	// ZoneAware wrapper to prefer same-zone backends.
	Zone string
	// Tier is the priority tier (0 = primary, higher = backup). Used by the
	// PriorityTiers wrapper to fall through to backup tiers.
	Tier int
}

func NewBackend(cfg config.BackendConfig) *Backend {
	b := &Backend{
		URL:      cfg.URL,
		MaxConns: cfg.MaxConns,
		Zone:     cfg.Zone,
		Tier:     cfg.Tier,
	}
	b.weight.Store(int32(cfg.Weight)) // #nosec G115 -- weight is a small positive int; overflow impossible in practice
	b.healthy.Store(true)
	return b
}

// GetWeight returns the backend's current weight. Safe for concurrent use with
// SetWeight/UpdateWeight (the admin API and reload can change it live).
func (b *Backend) GetWeight() int { return int(b.weight.Load()) }

// SetWeight updates the backend's weight. Safe for concurrent use.
func (b *Backend) SetWeight(w int) { b.weight.Store(int32(w)) } // #nosec G115

// IsHealthy reports whether the backend is currently eligible to serve traffic.
func (b *Backend) IsHealthy() bool {
	return b.healthy.Load()
}

// healthEpoch is a process-wide counter bumped whenever any backend's health flag
// actually changes. GetHealthy caches its result and rebuilds when this (or the
// balancer's own generation) advances — turning the hot-path health filter into a
// cache read while staying correct: any health flip invalidates every cache.
var healthEpoch atomic.Uint64

// SetHealthy updates the backend's health flag. Safe for concurrent use. Bumps the
// global health epoch only when the value actually changes.
func (b *Backend) SetHealthy(v bool) {
	if b.healthy.Swap(v) != v {
		healthEpoch.Add(1)
	}
}

func (b *Backend) IncrConn() {
	atomic.AddInt32(&b.ActiveConns, 1)
}

func (b *Backend) DecrConn() {
	atomic.AddInt32(&b.ActiveConns, -1)
}

func (b *Backend) GetActiveConns() int {
	return int(atomic.LoadInt32(&b.ActiveConns))
}

func (b *Backend) RecordSuccess() {
	atomic.StoreInt32(&b.Failures, 0)
	atomic.AddInt32(&b.Successes, 1)
}

func (b *Backend) RecordFailure() {
	atomic.AddInt32(&b.Failures, 1)
	atomic.StoreInt32(&b.Successes, 0)
}

func (b *Backend) GetFailures() int {
	return int(atomic.LoadInt32(&b.Failures))
}

func (b *Backend) GetSuccesses() int {
	return int(atomic.LoadInt32(&b.Successes))
}

type Balancer interface {
	Next() (*Backend, error)
	Add(*Backend)
	Remove(*Backend)
	All() []*Backend
	GetHealthy() []*Backend
	UpdateWeight(*Backend, int)
}

// KeyedBalancer is an optional capability implemented by balancers that select a
// backend from a routing key (client IP, session id, cookie value, etc.). The
// proxy discovers it via a type assertion so key-based algorithms (consistent
// hashing, affinity) integrate without changing the base Balancer interface.
// Like Next, NextForKey MUST reserve the returned backend via IncrConn.
type KeyedBalancer interface {
	NextForKey(key string) (*Backend, error)
}

// LatencyObserver is an optional capability implemented by latency-aware
// balancers (EWMA). The proxy reports the observed request latency for a backend
// after each request completes so the balancer can update its scoring.
type LatencyObserver interface {
	ObserveLatency(b *Backend, d time.Duration)
}

// OutcomeObserver is an optional capability implemented by components that react
// to per-request success/failure outcomes (outlier detection). The proxy calls
// ObserveOutcome with ok=true on a successful response and ok=false on failure.
type OutcomeObserver interface {
	ObserveOutcome(b *Backend, ok bool)
}

type BaseBalancer struct {
	backends []*Backend
	mu       sync.RWMutex
	gen      uint64 // bumped on Add/Remove (topology change); guarded by mu

	cacheMu       sync.Mutex
	cachedHealthy []*Backend
	cacheEpoch    uint64
	cacheGen      uint64
	cacheValid    bool
}

func (b *BaseBalancer) Add(backend *Backend) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.backends = append(b.backends, backend)
	b.gen++
}

func (b *BaseBalancer) Remove(backend *Backend) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, be := range b.backends {
		if be == backend {
			b.backends = append(b.backends[:i], b.backends[i+1:]...)
			b.gen++
			return
		}
	}
}

func (b *BaseBalancer) All() []*Backend {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]*Backend, len(b.backends))
	copy(result, b.backends)
	return result
}

// GetHealthy returns the currently-healthy backends. The result is cached and only
// rebuilt when a backend's health changed (global healthEpoch) or the balancer's
// topology changed (gen), turning the hot-path health filter into a cache read. The
// returned slice is read-only and shared between callers; callers must not mutate it
// (all internal callers only read/iterate it).
func (b *BaseBalancer) GetHealthy() []*Backend {
	ep := healthEpoch.Load()

	b.mu.RLock()
	gen := b.gen

	b.cacheMu.Lock()
	if b.cacheValid && b.cacheEpoch == ep && b.cacheGen == gen {
		cached := b.cachedHealthy
		b.cacheMu.Unlock()
		b.mu.RUnlock()
		return cached
	}
	b.cacheMu.Unlock()

	var healthy []*Backend
	for _, be := range b.backends {
		if be.IsHealthy() {
			healthy = append(healthy, be)
		}
	}
	b.mu.RUnlock()

	b.cacheMu.Lock()
	b.cachedHealthy = healthy
	b.cacheEpoch = ep
	b.cacheGen = gen
	b.cacheValid = true
	b.cacheMu.Unlock()
	return healthy
}

func (b *BaseBalancer) UpdateWeight(backend *Backend, weight int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, be := range b.backends {
		if be == backend {
			be.SetWeight(weight)
			return
		}
	}
}
