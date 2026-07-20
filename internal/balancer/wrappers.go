package balancer

import (
	"errors"
	"hash/fnv"
	"math/rand"
	"sync"
	"time"
)

// The wrappers in this file compose around any Balancer so behaviors can stack
// (e.g. OutlierDetection(SlowStart(ZoneAware(PriorityTiers(SWRR))))). They share a
// common strategy: rather than reimplementing each selection algorithm, a wrapper
// narrows the *eligible* backend set and delegates the actual choice to the inner
// balancer.
//
// Composition works by passing the narrowed candidate set down the chain via the
// internal subsetPicker capability, NOT by mutating backend health. When the
// candidate set already equals the inner balancer's full healthy set (the common,
// unrestricted case), selection delegates straight to inner.Next()/NextForKey(),
// preserving the inner algorithm's exact behavior and state. When it is a strict
// subset and the inner balancer can itself pick from a subset (another wrapper),
// the narrowing composes; otherwise a stateless pick over the subset is used.
//
// An earlier design hid excluded backends by toggling their health flag under a
// single package-level mutex held across inner.Next(). That both raced the health
// checker and self-deadlocked when two restricting wrappers stacked. This design
// has neither problem: it takes no shared lock and never mutates health.

var errNoHealthy = errors.New("no healthy backends")

// subsetPicker is an optional internal capability implemented by wrappers: choose
// from an explicit candidate subset (already narrowed by an outer wrapper),
// reserving the chosen backend via IncrConn. Base algorithms need not implement
// it — selectFrom/keyedSelectFrom fall back to a stateless pick over the subset.
type subsetPicker interface {
	pickFrom(candidates []*Backend) (*Backend, error)
	pickFromKey(candidates []*Backend, key string) (*Backend, error)
}

// selectFrom chooses from candidates using inner's own algorithm when the
// candidate set is exactly inner's healthy set (full fidelity, the common case),
// composes through inner when inner can pick from a subset, and otherwise falls
// back to a stateless least-connections pick over the candidates.
func selectFrom(inner Balancer, candidates []*Backend) (*Backend, error) {
	if len(candidates) == 0 {
		return nil, errNoHealthy
	}
	if sameSet(inner.GetHealthy(), candidates) {
		return inner.Next()
	}
	if sp, ok := inner.(subsetPicker); ok {
		return sp.pickFrom(candidates)
	}
	return statelessPick(candidates)
}

// keyedSelectFrom is the keyed (affinity/consistent-hash) counterpart of selectFrom.
func keyedSelectFrom(inner Balancer, candidates []*Backend, key string) (*Backend, error) {
	if len(candidates) == 0 {
		return nil, errNoHealthy
	}
	if sameSet(inner.GetHealthy(), candidates) {
		if kb, ok := inner.(KeyedBalancer); ok {
			return kb.NextForKey(key)
		}
		return inner.Next()
	}
	if sp, ok := inner.(subsetPicker); ok {
		return sp.pickFromKey(candidates, key)
	}
	if _, ok := inner.(KeyedBalancer); ok {
		return hashPick(candidates, key)
	}
	return statelessPick(candidates)
}

// statelessPick chooses the least-loaded candidate and reserves it. Used when the
// inner algorithm cannot select from a restricted subset directly.
func statelessPick(candidates []*Backend) (*Backend, error) {
	if len(candidates) == 0 {
		return nil, errNoHealthy
	}
	best := candidates[0]
	bestConns := best.GetActiveConns()
	for _, b := range candidates[1:] {
		if c := b.GetActiveConns(); c < bestConns {
			best, bestConns = b, c
		}
	}
	best.IncrConn()
	return best, nil
}

// hashPick deterministically maps key onto the candidate subset and reserves the
// chosen backend. Keyed fallback for a restricted subset.
func hashPick(candidates []*Backend, key string) (*Backend, error) {
	if len(candidates) == 0 {
		return nil, errNoHealthy
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	b := candidates[h.Sum32()%uint32(len(candidates))]
	b.IncrConn()
	return b, nil
}

func sameSet(a, b []*Backend) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[*Backend]bool, len(a))
	for _, x := range a {
		seen[x] = true
	}
	for _, x := range b {
		if !seen[x] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// PriorityTiers
// ---------------------------------------------------------------------------

// PriorityTiers restricts selection to the lowest Tier that has healthy members,
// falling through to higher (backup) tiers only when every lower tier is empty.
// Tier 0 is primary; higher numbers are progressively colder backups.
type PriorityTiers struct {
	inner Balancer
}

func NewPriorityTiers(inner Balancer) *PriorityTiers {
	return &PriorityTiers{inner: inner}
}

// eligibleFrom returns the lowest-tier members of the given pool.
func (p *PriorityTiers) eligibleFrom(pool []*Backend) []*Backend {
	if len(pool) == 0 {
		return nil
	}
	minTier := pool[0].Tier
	for _, b := range pool {
		if b.Tier < minTier {
			minTier = b.Tier
		}
	}
	var out []*Backend
	for _, b := range pool {
		if b.Tier == minTier {
			out = append(out, b)
		}
	}
	return out
}

func (p *PriorityTiers) eligible() []*Backend { return p.eligibleFrom(p.inner.GetHealthy()) }

func (p *PriorityTiers) Next() (*Backend, error) { return selectFrom(p.inner, p.eligible()) }

func (p *PriorityTiers) NextForKey(key string) (*Backend, error) {
	return keyedSelectFrom(p.inner, p.eligible(), key)
}

func (p *PriorityTiers) pickFrom(candidates []*Backend) (*Backend, error) {
	return selectFrom(p.inner, p.eligibleFrom(candidates))
}

func (p *PriorityTiers) pickFromKey(candidates []*Backend, key string) (*Backend, error) {
	return keyedSelectFrom(p.inner, p.eligibleFrom(candidates), key)
}

func (p *PriorityTiers) Add(b *Backend)                 { p.inner.Add(b) }
func (p *PriorityTiers) Remove(b *Backend)              { p.inner.Remove(b) }
func (p *PriorityTiers) All() []*Backend                { return p.inner.All() }
func (p *PriorityTiers) GetHealthy() []*Backend         { return p.inner.GetHealthy() }
func (p *PriorityTiers) UpdateWeight(b *Backend, w int) { p.inner.UpdateWeight(b, w) }

// ---------------------------------------------------------------------------
// ZoneAware
// ---------------------------------------------------------------------------

// ZoneAware prefers healthy backends whose Zone matches the local zone. When
// PreferSameZone is enabled and at least one healthy backend shares the local
// zone, only those are eligible; otherwise all healthy backends are eligible
// (cross-zone fallback).
type ZoneAware struct {
	inner          Balancer
	zone           string
	preferSameZone bool
}

func NewZoneAware(inner Balancer, localZone string, preferSameZone bool) *ZoneAware {
	return &ZoneAware{inner: inner, zone: localZone, preferSameZone: preferSameZone}
}

func (z *ZoneAware) eligibleFrom(pool []*Backend) []*Backend {
	if !z.preferSameZone || z.zone == "" {
		return pool
	}
	var inZone []*Backend
	for _, b := range pool {
		if b.Zone == z.zone {
			inZone = append(inZone, b)
		}
	}
	if len(inZone) > 0 {
		return inZone
	}
	return pool
}

func (z *ZoneAware) eligible() []*Backend { return z.eligibleFrom(z.inner.GetHealthy()) }

func (z *ZoneAware) Next() (*Backend, error) { return selectFrom(z.inner, z.eligible()) }

func (z *ZoneAware) NextForKey(key string) (*Backend, error) {
	return keyedSelectFrom(z.inner, z.eligible(), key)
}

func (z *ZoneAware) pickFrom(candidates []*Backend) (*Backend, error) {
	return selectFrom(z.inner, z.eligibleFrom(candidates))
}

func (z *ZoneAware) pickFromKey(candidates []*Backend, key string) (*Backend, error) {
	return keyedSelectFrom(z.inner, z.eligibleFrom(candidates), key)
}

func (z *ZoneAware) Add(b *Backend)                 { z.inner.Add(b) }
func (z *ZoneAware) Remove(b *Backend)              { z.inner.Remove(b) }
func (z *ZoneAware) All() []*Backend                { return z.inner.All() }
func (z *ZoneAware) GetHealthy() []*Backend         { return z.inner.GetHealthy() }
func (z *ZoneAware) UpdateWeight(b *Backend, w int) { z.inner.UpdateWeight(b, w) }

// ---------------------------------------------------------------------------
// SlowStart
// ---------------------------------------------------------------------------

// SlowStart ramps traffic to a backend that has just transitioned from unhealthy
// to healthy. For the configured window after a backend becomes healthy, its
// effective selection weight is scaled linearly from ~0 up to full, preventing a
// cold backend from being hit with full load the instant it recovers.
//
// The wrapper observes health transitions lazily on each selection: it snapshots
// current health and, when a backend flips false->true, records healthySince.
// The ramp is applied by probabilistically skipping the backend proportional to
// its remaining ramp deficit and re-selecting.
type SlowStart struct {
	inner  Balancer
	window time.Duration
	clock  func() time.Time
	rngMu  sync.Mutex
	rng    *rand.Rand

	mu           sync.Mutex
	wasHealthy   map[*Backend]bool
	healthySince map[*Backend]time.Time
}

func NewSlowStart(inner Balancer, window time.Duration) *SlowStart {
	return &SlowStart{
		inner:        inner,
		window:       window,
		clock:        time.Now,
		rng:          rand.New(rand.NewSource(time.Now().UnixNano())), // #nosec G404 -- non-crypto slow-start ramp jitter
		wasHealthy:   make(map[*Backend]bool),
		healthySince: make(map[*Backend]time.Time),
	}
}

// factor returns b's current ramp factor in [0,1]; 1 means fully ramped (or slow
// start disabled). Caller must not hold s.mu.
func (s *SlowStart) factor(b *Backend, now time.Time) float64 {
	if s.window <= 0 {
		return 1
	}
	s.mu.Lock()
	since, ok := s.healthySince[b]
	s.mu.Unlock()
	if !ok {
		return 1
	}
	elapsed := now.Sub(since)
	if elapsed >= s.window {
		return 1
	}
	if elapsed <= 0 {
		return 0.01
	}
	f := float64(elapsed) / float64(s.window)
	if f < 0.01 {
		f = 0.01
	}
	return f
}

// observeTransitions updates healthySince for any backend that flipped
// unhealthy->healthy since the last observation.
func (s *SlowStart) observeTransitions(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.inner.All() {
		h := b.IsHealthy()
		prev, seen := s.wasHealthy[b]
		if h && (!seen || !prev) {
			s.healthySince[b] = now
		}
		s.wasHealthy[b] = h
	}
}

// rampedSelect runs a bounded rejection loop over sel(), skipping not-yet-ramped
// backends in proportion to their deficit so their traffic share grows linearly.
func (s *SlowStart) rampedSelect(now time.Time, sel func() (*Backend, error)) (*Backend, error) {
	const attempts = 8
	var last *Backend
	for i := 0; i < attempts; i++ {
		b, err := sel()
		if err != nil {
			return nil, err
		}
		f := s.factor(b, now)
		if f >= 1 {
			return b, nil
		}
		s.rngMu.Lock()
		r := s.rng.Float64()
		s.rngMu.Unlock()
		if r < f {
			return b, nil
		}
		// Reject: release the reservation sel() made and try again.
		b.DecrConn()
		last = b
	}
	// Exhausted attempts (e.g. only cold backends available): accept the last.
	if last != nil {
		last.IncrConn()
		return last, nil
	}
	return nil, errNoHealthy
}

func (s *SlowStart) Next() (*Backend, error) {
	now := s.clock()
	s.observeTransitions(now)
	return s.rampedSelect(now, s.inner.Next)
}

func (s *SlowStart) NextForKey(key string) (*Backend, error) {
	// Affinity dominates over ramping for keyed selection; forward directly.
	now := s.clock()
	s.observeTransitions(now)
	return keyedSelectFrom(s.inner, s.inner.GetHealthy(), key)
}

func (s *SlowStart) pickFrom(candidates []*Backend) (*Backend, error) {
	now := s.clock()
	s.observeTransitions(now)
	return s.rampedSelect(now, func() (*Backend, error) { return selectFrom(s.inner, candidates) })
}

func (s *SlowStart) pickFromKey(candidates []*Backend, key string) (*Backend, error) {
	now := s.clock()
	s.observeTransitions(now)
	return keyedSelectFrom(s.inner, candidates, key)
}

func (s *SlowStart) Add(b *Backend)                 { s.inner.Add(b) }
func (s *SlowStart) Remove(b *Backend)              { s.inner.Remove(b) }
func (s *SlowStart) All() []*Backend                { return s.inner.All() }
func (s *SlowStart) GetHealthy() []*Backend         { return s.inner.GetHealthy() }
func (s *SlowStart) UpdateWeight(b *Backend, w int) { s.inner.UpdateWeight(b, w) }

// ---------------------------------------------------------------------------
// OutlierDetection
// ---------------------------------------------------------------------------

// OutlierDetection tracks per-backend rolling success/failure counts and ejects
// (marks unhealthy) backends whose error rate exceeds ErrorRateThreshold over at
// least MinRequests observations. Ejected backends are automatically reinstated
// after BaseEjection elapses. It never ejects more than MaxEjectionPercent of the
// backend fleet at once. It satisfies the OutcomeObserver capability via
// ObserveOutcome.
type OutlierDetection struct {
	inner Balancer

	enabled            bool
	errorRateThreshold float64
	minRequests        int
	baseEjection       time.Duration
	maxEjectionPercent int
	clock              func() time.Time

	mu    sync.Mutex
	stats map[*Backend]*outlierStat
}

type outlierStat struct {
	success   int
	failure   int
	ejected   bool
	ejectedAt time.Time
}

func NewOutlierDetection(inner Balancer, errorRateThreshold float64, minRequests int, baseEjection time.Duration, maxEjectionPercent int) *OutlierDetection {
	return &OutlierDetection{
		inner:              inner,
		enabled:            true,
		errorRateThreshold: errorRateThreshold,
		minRequests:        minRequests,
		baseEjection:       baseEjection,
		maxEjectionPercent: maxEjectionPercent,
		clock:              time.Now,
		stats:              make(map[*Backend]*outlierStat),
	}
}

func (o *OutlierDetection) statFor(b *Backend) *outlierStat {
	st := o.stats[b]
	if st == nil {
		st = &outlierStat{}
		o.stats[b] = st
	}
	return st
}

// currentlyEjectedLocked counts backends this detector has ejected and not yet
// reinstated. Caller holds o.mu.
func (o *OutlierDetection) currentlyEjectedLocked() int {
	n := 0
	for _, st := range o.stats {
		if st.ejected {
			n++
		}
	}
	return n
}

// ObserveOutcome records a success/failure for b and ejects it if it now exceeds
// the error-rate threshold (subject to the MaxEjectionPercent cap).
func (o *OutlierDetection) ObserveOutcome(b *Backend, ok bool) {
	if !o.enabled || b == nil {
		return
	}
	now := o.clock()
	o.mu.Lock()
	defer o.mu.Unlock()

	st := o.statFor(b)
	if ok {
		st.success++
	} else {
		st.failure++
	}

	total := st.success + st.failure
	if st.ejected || total < o.minRequests {
		return
	}

	errRate := float64(st.failure) / float64(total)
	if errRate <= o.errorRateThreshold {
		return
	}

	// Respect the ejection cap: never eject more than maxEjectionPercent of the
	// fleet. A cap computing to <1 is treated as "at least one may be ejected" so
	// detection is not silently disabled on small fleets.
	fleet := len(o.inner.All())
	maxEject := fleet * o.maxEjectionPercent / 100
	if maxEject < 1 {
		maxEject = 1
	}
	if o.currentlyEjectedLocked() >= maxEject {
		return
	}

	st.ejected = true
	st.ejectedAt = now
	b.SetHealthy(false)
}

// reinstateExpired reinstates any backend whose ejection window has elapsed and
// resets its counters. Called before each selection.
func (o *OutlierDetection) reinstateExpired(now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for b, st := range o.stats {
		if st.ejected && now.Sub(st.ejectedAt) >= o.baseEjection {
			st.ejected = false
			st.success = 0
			st.failure = 0
			b.SetHealthy(true)
		}
	}
}

func (o *OutlierDetection) Next() (*Backend, error) {
	o.reinstateExpired(o.clock())
	return o.inner.Next()
}

func (o *OutlierDetection) NextForKey(key string) (*Backend, error) {
	o.reinstateExpired(o.clock())
	return keyedSelectFrom(o.inner, o.inner.GetHealthy(), key)
}

func (o *OutlierDetection) pickFrom(candidates []*Backend) (*Backend, error) {
	o.reinstateExpired(o.clock())
	return selectFrom(o.inner, candidates)
}

func (o *OutlierDetection) pickFromKey(candidates []*Backend, key string) (*Backend, error) {
	o.reinstateExpired(o.clock())
	return keyedSelectFrom(o.inner, candidates, key)
}

func (o *OutlierDetection) Add(b *Backend)                 { o.inner.Add(b) }
func (o *OutlierDetection) Remove(b *Backend)              { o.inner.Remove(b) }
func (o *OutlierDetection) All() []*Backend                { return o.inner.All() }
func (o *OutlierDetection) GetHealthy() []*Backend         { return o.inner.GetHealthy() }
func (o *OutlierDetection) UpdateWeight(b *Backend, w int) { o.inner.UpdateWeight(b, w) }
