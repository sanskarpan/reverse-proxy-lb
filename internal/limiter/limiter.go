package limiter

import (
	"errors"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	defaultIdleTTL     = time.Minute * 10
	defaultMaxEntries  = 100000
	defaultCleanupTick = time.Minute * 5
)

// Algorithm names accepted by Options.Algorithm. These mirror the config
// package's RateLimiterConfig.Algorithm values.
const (
	AlgorithmTokenBucket = "token_bucket"
	AlgorithmGCRA        = "gcra"
)

// algoLimiter is the small internal interface both the token-bucket and GCRA
// implementations satisfy. reserve reports whether a single event is permitted
// right now and, when it is not, how long the caller should wait before a token
// becomes available (the suggested Retry-After).
//
// A reserve that returns allowed==true has consumed a token. A reserve that
// returns allowed==false consumes nothing.
type algoLimiter interface {
	reserve(now time.Time) (allowed bool, retryAfter time.Duration)
	// burst / limit are exposed so tests and UpdateRate introspection keep
	// working the way the shipped *rate.Limiter did.
	burstN() int
	limitRPS() float64
}

// tokenBucketLimiter wraps golang.org/x/time/rate.Limiter. It is the default
// algorithm and preserves the exact allow semantics of the shipped code.
type tokenBucketLimiter struct {
	l *rate.Limiter
}

func newTokenBucket(rps float64, burst int) *tokenBucketLimiter {
	return &tokenBucketLimiter{l: rate.NewLimiter(rate.Limit(rps), burst)}
}

func (t *tokenBucketLimiter) reserve(now time.Time) (bool, time.Duration) {
	r := t.l.ReserveN(now, 1)
	if !r.OK() {
		// Burst is smaller than 1 event; never satisfiable.
		return false, 0
	}
	delay := r.DelayFrom(now)
	if delay > 0 {
		// Not available yet: cancel so the token is returned to the bucket and
		// report the wait as Retry-After.
		r.CancelAt(now)
		return false, delay
	}
	return true, 0
}

func (t *tokenBucketLimiter) burstN() int       { return t.l.Burst() }
func (t *tokenBucketLimiter) limitRPS() float64 { return float64(t.l.Limit()) }

// gcraLimiter is a hand-written Generic Cell Rate Algorithm limiter
// (leaky-bucket-as-meter). It allows a steady rate of rps events per second and
// tolerates bursts of up to burst events before throttling.
//
// The "theoretical arrival time" (tat) tracks the earliest time the virtual
// scheduling clock permits the next event. emission is the spacing between
// events (1/rps). tolerance = emission*burst is how far ahead of "now" the tat
// is allowed to run, i.e. the burst allowance.
type gcraLimiter struct {
	mu        sync.Mutex
	emission  time.Duration // spacing between events at the steady rate
	tolerance time.Duration // burst allowance (emission * burst)
	tat       time.Time     // theoretical arrival time
	rps       float64
	burst     int
}

func newGCRA(rps float64, burst int) *gcraLimiter {
	g := &gcraLimiter{rps: rps, burst: burst}
	g.configure(rps, burst)
	return g
}

func (g *gcraLimiter) configure(rps float64, burst int) {
	g.rps = rps
	g.burst = burst
	if rps <= 0 {
		// Degenerate rate: block everything (except that burst<=0 stays blocked
		// too). Use a very large emission so nothing is ever admitted steadily.
		g.emission = time.Duration(1<<62 - 1)
	} else {
		g.emission = time.Duration(float64(time.Second) / rps)
	}
	if burst < 1 {
		// A bucket that cannot hold even one event still needs room for the
		// single event being tested; model burst>=1 as one slot.
		g.tolerance = 0
	} else {
		g.tolerance = time.Duration(int64(g.emission) * int64(burst-1))
	}
}

func (g *gcraLimiter) reserve(now time.Time) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.burst < 1 {
		return false, 0
	}

	// The new theoretical arrival time if this event were admitted.
	tat := g.tat
	if tat.Before(now) {
		tat = now
	}
	// Allowed when the (would-be) tat does not run further than tolerance ahead
	// of now. The earliest allowed arrival is tat - tolerance.
	allowAt := tat.Add(-g.tolerance)
	if now.Before(allowAt) {
		// Too early: report how long until a slot frees up.
		return false, allowAt.Sub(now)
	}
	g.tat = tat.Add(g.emission)
	return true, 0
}

func (g *gcraLimiter) burstN() int       { return g.burst }
func (g *gcraLimiter) limitRPS() float64 { return g.rps }

// newAlgoLimiter builds the configured algorithm implementation.
func newAlgoLimiter(algorithm string, rps float64, burst int) algoLimiter {
	if algorithm == AlgorithmGCRA {
		return newGCRA(rps, burst)
	}
	return newTokenBucket(rps, burst)
}

// keyLimiter holds a per-key limiter together with the last time it was used.
// lastSeen drives idle-TTL eviction and oldest-entry eviction when the map is
// at capacity.
//
// For back-compat the token-bucket path keeps a concrete *rate.Limiter in the
// limiter field (existing tests read entry.limiter.Burst()/Limit()). algo holds
// the algorithm-agnostic view used by the hot path; for token_bucket it wraps
// the same *rate.Limiter so both fields stay in sync.
type keyLimiter struct {
	limiter  *rate.Limiter // non-nil only for the token_bucket algorithm
	algo     algoLimiter
	lastSeen time.Time
}

// ruleLimiter holds the sub-limiters for a single named rule keyed by client
// key, plus the rule's own rate/burst so it can be rebuilt independently of the
// default per-key bucket.
type ruleLimiter struct {
	rps      float64
	burst    int
	limiters map[string]*keyLimiter
}

// Options configures a RateLimiter. All fields are optional; zero values fall
// back to the shipped defaults so callers can opt in incrementally.
type Options struct {
	// Algorithm selects the per-key/rule algorithm: "token_bucket" (default) or
	// "gcra". An empty string means token_bucket.
	Algorithm string

	// PerKeyRPS/PerKeyBurst govern each distinct client key's own bucket.
	PerKeyRPS   float64
	PerKeyBurst int

	// GlobalRPS/GlobalBurst govern the single aggregate limiter shared across
	// all keys. When GlobalRPS<=0 they fall back to the per-key values so the
	// shipped "global and per-key share the same numbers" behaviour is kept.
	GlobalRPS   float64
	GlobalBurst int
}

type RateLimiter struct {
	mu                sync.RWMutex
	algorithm         string
	requestsPerSecond float64
	burst             int
	globalRPS         float64
	globalBurst       int
	limiters          map[string]*keyLimiter
	globalLimiter     algoLimiter
	rules             map[string]*ruleLimiter
	cleanupInterval   time.Duration
	// idleTTL is how long a per-key limiter may sit unused before cleanup()
	// evicts it. Active keys (recently seen) are never evicted.
	idleTTL time.Duration
	// maxEntries bounds the number of distinct per-key limiters held at once,
	// preventing memory exhaustion from many distinct keys between cleanups.
	maxEntries int
	stopCh     chan struct{}

	// store, when set, is a shared (potentially cross-instance) backend enforcing a
	// distributed global limit before the local limiters (ENHANCEMENTS 4.4).
	store      Store
	storeRPS   float64
	storeBurst int
	storeKey   string
}

// SetStore installs a shared Store enforcing a distributed global limit of rps/burst.
// Multiple RateLimiters sharing one Store enforce a combined limit across instances.
// key namespaces the shared budget (use the same key on every instance). A nil store
// disables the distributed gate.
func (r *RateLimiter) SetStore(store Store, rps float64, burst int, key string) {
	if key == "" {
		key = "__global__"
	}
	r.mu.Lock()
	r.store, r.storeRPS, r.storeBurst, r.storeKey = store, rps, burst, key
	r.mu.Unlock()
}

// NewRateLimiter keeps the shipped constructor signature: the global limiter and
// each per-key limiter share the same rps/burst and use the token_bucket
// algorithm. Use NewRateLimiterWithOptions for independent global limits or the
// GCRA algorithm.
func NewRateLimiter(requestsPerSecond float64, burst int) *RateLimiter {
	return NewRateLimiterWithOptions(Options{
		Algorithm:   AlgorithmTokenBucket,
		PerKeyRPS:   requestsPerSecond,
		PerKeyBurst: burst,
	})
}

// NewRateLimiterWithOptions builds a RateLimiter with independent global vs
// per-key limits and a selectable algorithm.
func NewRateLimiterWithOptions(opts Options) *RateLimiter {
	algorithm := opts.Algorithm
	if algorithm == "" {
		algorithm = AlgorithmTokenBucket
	}

	globalRPS := opts.GlobalRPS
	globalBurst := opts.GlobalBurst
	if globalRPS <= 0 {
		globalRPS = opts.PerKeyRPS
	}
	if globalBurst <= 0 {
		globalBurst = opts.PerKeyBurst
	}

	return &RateLimiter{
		algorithm:         algorithm,
		requestsPerSecond: opts.PerKeyRPS,
		burst:             opts.PerKeyBurst,
		globalRPS:         globalRPS,
		globalBurst:       globalBurst,
		limiters:          make(map[string]*keyLimiter),
		globalLimiter:     newAlgoLimiter(algorithm, globalRPS, globalBurst),
		rules:             make(map[string]*ruleLimiter),
		cleanupInterval:   defaultCleanupTick,
		idleTTL:           defaultIdleTTL,
		maxEntries:        defaultMaxEntries,
		stopCh:            make(chan struct{}),
	}
}

// AddRule registers (or replaces) a named rule sub-limiter with its own
// rps/burst. Rule sub-limiters are keyed by client key independently from the
// default per-key bucket, so a route can be throttled on its own budget.
func (r *RateLimiter) AddRule(name string, rps float64, burst int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules[name] = &ruleLimiter{
		rps:      rps,
		burst:    burst,
		limiters: make(map[string]*keyLimiter),
	}
}

// newKeyLimiter builds a per-key entry for the given rps/burst using the
// limiter's configured algorithm. For token_bucket it also stores the concrete
// *rate.Limiter so callers/tests that introspect entry.limiter keep working.
func (r *RateLimiter) newKeyLimiter(rps float64, burst int, now time.Time) *keyLimiter {
	if r.algorithm == AlgorithmGCRA {
		return &keyLimiter{
			algo:     newGCRA(rps, burst),
			lastSeen: now,
		}
	}
	tb := newTokenBucket(rps, burst)
	return &keyLimiter{
		limiter:  tb.l,
		algo:     tb,
		lastSeen: now,
	}
}

// Allow preserves the shipped API: it returns a non-nil error when the request
// is denied (global or per-key). It is a thin wrapper over AllowKey that drops
// the Retry-After. Prefer AllowKey/AllowRule for the richer result.
func (r *RateLimiter) Allow(key string) error {
	allowed, _, scope := r.allow(key, "")
	if !allowed {
		if scope == scopeGlobal {
			return errors.New("rate limit exceeded (global)")
		}
		return errors.New("rate limit exceeded (per-ip)")
	}
	return nil
}

// AllowKey reports whether the request for key is permitted and, when denied,
// the suggested Retry-After (time until a token frees up). This is the richer
// replacement for Allow that the middleware uses to set the Retry-After header.
func (r *RateLimiter) AllowKey(key string) (allowed bool, retryAfter time.Duration) {
	allowed, retryAfter, _ = r.allow(key, "")
	return allowed, retryAfter
}

// AllowRule is like AllowKey but consults the named rule's sub-limiter for the
// (rule, key) pair instead of the default per-key bucket. The global limiter is
// still enforced first. If the rule name is unknown it falls back to the default
// per-key bucket.
func (r *RateLimiter) AllowRule(rule, key string) (allowed bool, retryAfter time.Duration) {
	allowed, retryAfter, _ = r.allow(key, rule)
	return allowed, retryAfter
}

type denyScope int

const (
	scopeNone denyScope = iota
	scopeGlobal
	scopePerKey
)

func (r *RateLimiter) allow(key, rule string) (bool, time.Duration, denyScope) {
	now := time.Now()

	// Distributed global gate first: when a shared Store is configured, the combined
	// limit across all instances sharing it is authoritative (ENHANCEMENTS 4.4).
	r.mu.Lock()
	st, srps, sburst, skey := r.store, r.storeRPS, r.storeBurst, r.storeKey
	r.mu.Unlock()
	if st != nil {
		if ok, retry := st.Allow(skey, srps, sburst, now); !ok {
			return false, retry, scopeGlobal
		}
	}

	// Global limiter first. It is guarded by r.mu because algoLimiter
	// implementations (GCRA) mutate shared state and UpdateRate may swap it.
	r.mu.Lock()
	gl := r.globalLimiter
	r.mu.Unlock()
	if ok, retry := gl.reserve(now); !ok {
		return false, retry, scopeGlobal
	}

	// Resolve the target limiter map: a rule's sub-map when a known rule is
	// named, otherwise the default per-key map.
	r.mu.Lock()
	var (
		targetMap map[string]*keyLimiter
		rps       = r.requestsPerSecond
		burst     = r.burst
		isDefault = true
	)
	if rule != "" {
		if rl, ok := r.rules[rule]; ok {
			targetMap = rl.limiters
			rps = rl.rps
			burst = rl.burst
			isDefault = false
		}
	}
	if targetMap == nil {
		targetMap = r.limiters
	}

	entry, exists := targetMap[key]
	if exists {
		entry.lastSeen = now
	} else {
		// Capacity management only applies to the default per-key map (the one
		// exposed to untrusted client-key cardinality). Rule maps are bounded
		// by the same client-key space in practice.
		if isDefault {
			r.ensureCapacityLocked(now)
		}
		entry = r.newKeyLimiter(rps, burst, now)
		targetMap[key] = entry
	}
	algo := entry.algo
	r.mu.Unlock()

	if ok, retry := algo.reserve(now); !ok {
		return false, retry, scopePerKey
	}
	return true, 0, scopeNone
}

// ensureCapacityLocked makes room for at least one new key when the map is at
// capacity. It first evicts stale (idle) entries; if the map is still full it
// evicts the single oldest-lastSeen entry. Caller must hold r.mu.
func (r *RateLimiter) ensureCapacityLocked(now time.Time) {
	if r.maxEntries <= 0 || len(r.limiters) < r.maxEntries {
		return
	}

	// First pass: drop everything past the idle TTL.
	for key, entry := range r.limiters {
		if now.Sub(entry.lastSeen) > r.idleTTL {
			delete(r.limiters, key)
		}
	}

	if len(r.limiters) < r.maxEntries {
		return
	}

	// Still full: evict the oldest-lastSeen entry to make room for one insert.
	var oldestKey string
	var oldestSeen time.Time
	first := true
	for key, entry := range r.limiters {
		if first || entry.lastSeen.Before(oldestSeen) {
			oldestKey = key
			oldestSeen = entry.lastSeen
			first = false
		}
	}
	if !first {
		delete(r.limiters, oldestKey)
	}
}

func (r *RateLimiter) Start() {
	go r.cleanup()
}

func (r *RateLimiter) Stop() {
	close(r.stopCh)
}

func (r *RateLimiter) cleanup() {
	ticker := time.NewTicker(r.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			r.mu.Lock()
			// Evict ONLY entries idle past the TTL; active keys keep their
			// buckets so they can't burst again by having state wiped.
			for key, entry := range r.limiters {
				if now.Sub(entry.lastSeen) > r.idleTTL {
					delete(r.limiters, key)
				}
			}
			// Apply the same eviction to each rule's sub-map.
			for _, rl := range r.rules {
				for key, entry := range rl.limiters {
					if now.Sub(entry.lastSeen) > r.idleTTL {
						delete(rl.limiters, key)
					}
				}
			}
			r.mu.Unlock()
		case <-r.stopCh:
			return
		}
	}
}

// UpdateRate updates the per-key and global rate/burst. It preserves the shipped
// signature: both global and per-key limits are set to the same rps/burst. Use
// UpdateRates to set them independently.
func (r *RateLimiter) UpdateRate(requestsPerSecond float64, burst int) {
	r.UpdateRates(requestsPerSecond, burst, requestsPerSecond, burst)
}

// UpdateRates updates the per-key and global limits independently, rebuilding
// the global limiter and every existing per-key/rule limiter so the new rates
// take effect immediately. Each entry's lastSeen is preserved so cleanup and
// capacity eviction still treat recently-active keys as active.
func (r *RateLimiter) UpdateRates(perKeyRPS float64, perKeyBurst int, globalRPS float64, globalBurst int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.requestsPerSecond = perKeyRPS
	r.burst = perKeyBurst
	r.globalRPS = globalRPS
	r.globalBurst = globalBurst
	r.globalLimiter = newAlgoLimiter(r.algorithm, globalRPS, globalBurst)

	for _, entry := range r.limiters {
		r.rebuildEntryLocked(entry, perKeyRPS, perKeyBurst)
	}
	for _, rl := range r.rules {
		for _, entry := range rl.limiters {
			r.rebuildEntryLocked(entry, rl.rps, rl.burst)
		}
	}
}

// rebuildEntryLocked recreates an entry's underlying limiter at the new rate
// while preserving lastSeen. Caller holds r.mu.
func (r *RateLimiter) rebuildEntryLocked(entry *keyLimiter, rps float64, burst int) {
	if r.algorithm == AlgorithmGCRA {
		entry.limiter = nil
		entry.algo = newGCRA(rps, burst)
		return
	}
	tb := newTokenBucket(rps, burst)
	entry.limiter = tb.l
	entry.algo = tb
}
