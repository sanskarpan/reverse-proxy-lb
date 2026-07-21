package balancer

import (
	"errors"
	"hash/fnv"
	"math/rand"
	"sort"
	"strconv"
	"sync"

	"reverse-proxy-lb/internal/randutil"
)

// ConsistentHash implements consistent hashing with bounded loads (the
// Mirror/Vixie "consistent hashing with bounded loads" scheme used by HAProxy
// and Vimeo). Keys map onto a hash ring of virtual nodes; each key is served by
// the first backend clockwise from the key whose current active-connection load
// is within LoadFactor * average. This keeps affinity stable while capping how
// overloaded any single backend can get.
//
// The ring is rebuilt whenever membership (the healthy backend set) changes, so
// selection stays consistent between rebuilds and remaps only a small fraction
// of keys when a backend is added or removed.
type ConsistentHash struct {
	BaseBalancer

	replicas   int
	loadFactor float64

	mu      sync.Mutex
	ring    []uint32            // sorted hash values of virtual nodes
	ringMap map[uint32]*Backend // hash -> backend
	ringKey string              // membership signature the current ring was built for
	rng     *rand.Rand

	// seenPool caches a []bool scratch slice (len == number of healthy backends)
	// to avoid a per-call map[*Backend]bool allocation inside NextForKey.
	seenPool sync.Pool
}

// NewConsistentHash builds a bounded-load consistent-hash balancer. replicas is
// the number of virtual nodes per backend (default 100 when <= 0); loadFactor is
// the bounded-load multiplier over the average (default 1.25 when <= 1).
func NewConsistentHash(replicas int, loadFactor float64) *ConsistentHash {
	if replicas <= 0 {
		replicas = 100
	}
	if loadFactor <= 1 {
		loadFactor = 1.25
	}
	return &ConsistentHash{
		replicas:   replicas,
		loadFactor: loadFactor,
		ringMap:    make(map[uint32]*Backend),
		rng:        randutil.NewRand(), // #nosec G404 -- non-crypto fallback key generation
	}
}

func hashKey(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

// membershipSignature returns a stable string identifying the current healthy
// set. When it changes we rebuild the ring.
func membershipSignature(healthy []*Backend) string {
	urls := make([]string, len(healthy))
	for i, b := range healthy {
		urls[i] = b.URL
	}
	sort.Strings(urls)
	sig := ""
	for _, u := range urls {
		sig += u + "|"
	}
	return sig
}

// rebuildLocked rebuilds the ring for the given healthy set. Caller holds mu.
func (c *ConsistentHash) rebuildLocked(healthy []*Backend, sig string) {
	c.ring = c.ring[:0]
	c.ringMap = make(map[uint32]*Backend, len(healthy)*c.replicas)
	for _, b := range healthy {
		for i := 0; i < c.replicas; i++ {
			h := hashKey(b.URL + "#" + strconv.Itoa(i))
			// Skip exact collisions to keep ring lookups deterministic.
			if _, exists := c.ringMap[h]; exists {
				continue
			}
			c.ringMap[h] = b
			c.ring = append(c.ring, h)
		}
	}
	sort.Slice(c.ring, func(i, j int) bool { return c.ring[i] < c.ring[j] })
	c.ringKey = sig
}

// NextForKey selects the backend serving key using bounded-load consistent
// hashing and reserves it. It satisfies the KeyedBalancer capability.
func (c *ConsistentHash) NextForKey(key string) (*Backend, error) {
	healthy := c.GetHealthy()
	if len(healthy) == 0 {
		return nil, errors.New("no healthy backends")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	sig := membershipSignature(healthy)
	if sig != c.ringKey || len(c.ring) == 0 {
		c.rebuildLocked(healthy, sig)
	}
	if len(c.ring) == 0 {
		return nil, errors.New("no healthy backends")
	}

	// Bounded-load capacity: no backend may exceed loadFactor * average load,
	// rounded up. total counts current in-flight plus the one we're about to add.
	total := 1
	for _, b := range healthy {
		total += b.GetActiveConns()
	}
	avg := float64(total) / float64(len(healthy))
	capacity := int(c.loadFactor*avg) + 1

	h := hashKey(key)
	start := sort.Search(len(c.ring), func(i int) bool { return c.ring[i] >= h })
	if start == len(c.ring) {
		start = 0
	}

	// Walk clockwise from the key's position, choosing the first backend under
	// capacity. If every backend is at capacity (all equally saturated), fall
	// back to the first one on the ring so we never fail to place the key.
	//
	// Use a pooled []bool indexed by healthy-slice position instead of a
	// per-call map[*Backend]bool to eliminate the allocation on the hot path.
	n := len(healthy)
	var rawSeen []bool
	if v := c.seenPool.Get(); v != nil {
		rawSeen = v.([]bool)
	}
	if cap(rawSeen) < n {
		rawSeen = make([]bool, n)
	}
	rawSeen = rawSeen[:n]
	for i := range rawSeen {
		rawSeen[i] = false
	}
	defer c.seenPool.Put(rawSeen) //lint:ignore SA6002 slice header is pointer-sized; amortized allocation is the goal

	// backendIdx returns the position of b in healthy, or -1 if absent.
	// Linear search is O(n) but backend counts are typically < 50 so this
	// is faster in practice than a map lookup (no alloc, cache-friendly).
	backendIdx := func(b *Backend) int {
		for i, h := range healthy {
			if h == b {
				return i
			}
		}
		return -1
	}

	var fallback *Backend
	for i := 0; i < len(c.ring); i++ {
		idx := (start + i) % len(c.ring)
		b := c.ringMap[c.ring[idx]]
		if fallback == nil {
			fallback = b
		}
		bi := backendIdx(b)
		if bi < 0 || rawSeen[bi] {
			continue
		}
		rawSeen[bi] = true
		if b.GetActiveConns() < capacity {
			b.IncrConn()
			return b, nil
		}
	}

	if fallback == nil {
		fallback = healthy[0]
	}
	fallback.IncrConn()
	return fallback, nil
}

// Next selects a backend using a random routing key. Consistent hashing is
// intended to be called via NextForKey; Next exists so ConsistentHash still
// satisfies the Balancer interface (e.g. when no key is available).
func (c *ConsistentHash) Next() (*Backend, error) {
	c.mu.Lock()
	k := strconv.FormatUint(uint64(c.rng.Uint32()), 10)
	c.mu.Unlock()
	return c.NextForKey(k)
}
