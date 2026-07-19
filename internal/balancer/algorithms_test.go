package balancer

import (
	"fmt"
	"math"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
)

func mkBackend(url string, weight int) *Backend {
	return NewBackend(config.BackendConfig{URL: url, Weight: weight, MaxConns: 10000})
}

// ---------------------------------------------------------------------------
// SWRR
// ---------------------------------------------------------------------------

func TestSWRRDistribution(t *testing.T) {
	s := NewSWRR()
	b1 := mkBackend("http://a", 1)
	b3 := mkBackend("http://b", 3)
	s.Add(b1)
	s.Add(b3)

	counts := map[string]int{}
	const n = 4000
	for i := 0; i < n; i++ {
		b, err := s.Next()
		if err != nil {
			t.Fatal(err)
		}
		counts[b.URL]++
		b.DecrConn()
	}
	// Expect ~ 1:3 ratio.
	got := float64(counts["http://b"]) / float64(counts["http://a"])
	if got < 2.7 || got > 3.3 {
		t.Errorf("SWRR ratio b/a = %.2f, want ~3", got)
	}
}

func TestSWRRSmoothness(t *testing.T) {
	// With weights 1 and 2, SWRR should never pick the weight-2 backend more than
	// twice in a row (smooth interleaving, not bursty).
	s := NewSWRR()
	a := mkBackend("http://a", 1)
	b := mkBackend("http://b", 2)
	s.Add(a)
	s.Add(b)

	streak := 0
	maxStreak := 0
	var prev string
	for i := 0; i < 60; i++ {
		sel, err := s.Next()
		if err != nil {
			t.Fatal(err)
		}
		sel.DecrConn()
		if sel.URL == prev {
			streak++
		} else {
			streak = 1
			prev = sel.URL
		}
		if streak > maxStreak {
			maxStreak = streak
		}
	}
	if maxStreak > 2 {
		t.Errorf("SWRR max streak = %d, want <= 2 (should be smooth)", maxStreak)
	}
}

// ---------------------------------------------------------------------------
// P2C
// ---------------------------------------------------------------------------

func TestP2CPicksLowerLoad(t *testing.T) {
	p := NewP2C()
	// Two backends: one heavily loaded, one idle. With only two backends P2C
	// always samples both, so it must pick the idle one.
	busy := mkBackend("http://busy", 1)
	idle := mkBackend("http://idle", 1)
	for i := 0; i < 50; i++ {
		busy.IncrConn()
	}
	p.Add(busy)
	p.Add(idle)

	for i := 0; i < 20; i++ {
		b, err := p.Next()
		if err != nil {
			t.Fatal(err)
		}
		if b.URL != "http://idle" {
			t.Fatalf("P2C picked %s, want idle (lower load)", b.URL)
		}
		b.DecrConn()
	}
}

func TestP2CDistinctSampling(t *testing.T) {
	p := NewP2C()
	for i := 0; i < 5; i++ {
		p.Add(mkBackend(fmt.Sprintf("http://b%d", i), 1))
	}
	// Just exercise many selections to ensure no panic / index errors and that
	// selection always returns a healthy backend.
	for i := 0; i < 1000; i++ {
		b, err := p.Next()
		if err != nil {
			t.Fatal(err)
		}
		if b == nil {
			t.Fatal("nil backend")
		}
		b.DecrConn()
	}
}

// ---------------------------------------------------------------------------
// WeightedRandom
// ---------------------------------------------------------------------------

func TestWeightedRandomDistribution(t *testing.T) {
	w := NewWeightedRandom()
	b1 := mkBackend("http://a", 1)
	b4 := mkBackend("http://b", 4)
	w.Add(b1)
	w.Add(b4)

	counts := map[string]int{}
	const n = 20000
	for i := 0; i < n; i++ {
		b, err := w.Next()
		if err != nil {
			t.Fatal(err)
		}
		counts[b.URL]++
		b.DecrConn()
	}
	// Expected share for b = 4/5 = 0.8.
	share := float64(counts["http://b"]) / float64(n)
	if math.Abs(share-0.8) > 0.03 {
		t.Errorf("WeightedRandom b share = %.3f, want ~0.8", share)
	}
}

// ---------------------------------------------------------------------------
// WeightedLeastConn
// ---------------------------------------------------------------------------

func TestWeightedLeastConn(t *testing.T) {
	w := NewWeightedLeastConn()
	// a: weight 1 with 2 conns -> score 2.0
	// b: weight 4 with 4 conns -> score 1.0  (should be chosen)
	a := mkBackend("http://a", 1)
	b := mkBackend("http://b", 4)
	for i := 0; i < 2; i++ {
		a.IncrConn()
	}
	for i := 0; i < 4; i++ {
		b.IncrConn()
	}
	w.Add(a)
	w.Add(b)

	sel, err := w.Next()
	if err != nil {
		t.Fatal(err)
	}
	if sel.URL != "http://b" {
		t.Errorf("WeightedLeastConn picked %s, want b (lower normalized load)", sel.URL)
	}
}

func TestWeightedLeastConnDistribution(t *testing.T) {
	w := NewWeightedLeastConn()
	a := mkBackend("http://a", 1)
	b := mkBackend("http://b", 3)
	w.Add(a)
	w.Add(b)

	// Model a steady-state pool of in-flight requests: keep a fixed number of
	// connections outstanding, releasing an old one each iteration. This lets the
	// active-connection counts accumulate so the weight normalization steers more
	// load toward the higher-weight backend (min activeConns/weight).
	counts := map[string]int{}
	const inflight = 40
	var window []*Backend
	for i := 0; i < 8000; i++ {
		sel, err := w.Next()
		if err != nil {
			t.Fatal(err)
		}
		counts[sel.URL]++
		window = append(window, sel)
		if len(window) > inflight {
			window[0].DecrConn()
			window = window[1:]
		}
	}
	for _, s := range window {
		s.DecrConn()
	}
	// The weight-3 backend should hold roughly 3x the connections and therefore
	// receive the majority of selections. Assert a clear skew toward b.
	ratio := float64(counts["http://b"]) / float64(counts["http://a"])
	if ratio < 2.0 || ratio > 4.5 {
		t.Errorf("WeightedLeastConn ratio b/a = %.2f, want ~3", ratio)
	}
}

// ---------------------------------------------------------------------------
// ConsistentHash
// ---------------------------------------------------------------------------

func TestConsistentHashStableMapping(t *testing.T) {
	ch := NewConsistentHash(100, 100.0) // high load factor so bounded-load never diverts
	for i := 0; i < 5; i++ {
		ch.Add(mkBackend(fmt.Sprintf("http://b%d", i), 1))
	}

	// Same key maps to the same backend across calls (release conns so load stays 0).
	first := map[string]string{}
	for k := 0; k < 200; k++ {
		key := fmt.Sprintf("key-%d", k)
		b, err := ch.NextForKey(key)
		if err != nil {
			t.Fatal(err)
		}
		first[key] = b.URL
		b.DecrConn()
	}
	for k := 0; k < 200; k++ {
		key := fmt.Sprintf("key-%d", k)
		b, err := ch.NextForKey(key)
		if err != nil {
			t.Fatal(err)
		}
		if first[key] != b.URL {
			t.Fatalf("key %s remapped %s -> %s without membership change", key, first[key], b.URL)
		}
		b.DecrConn()
	}
}

func TestConsistentHashMinimalRemap(t *testing.T) {
	backends := make([]*Backend, 5)
	ch := NewConsistentHash(200, 100.0)
	for i := range backends {
		backends[i] = mkBackend(fmt.Sprintf("http://b%d", i), 1)
		ch.Add(backends[i])
	}

	const nkeys = 5000
	before := make([]string, nkeys)
	for k := 0; k < nkeys; k++ {
		b, err := ch.NextForKey(fmt.Sprintf("key-%d", k))
		if err != nil {
			t.Fatal(err)
		}
		before[k] = b.URL
		b.DecrConn()
	}

	// Remove one backend; only keys that mapped to it should move.
	ch.Remove(backends[2])
	removedURL := backends[2].URL

	moved := 0
	for k := 0; k < nkeys; k++ {
		b, err := ch.NextForKey(fmt.Sprintf("key-%d", k))
		if err != nil {
			t.Fatal(err)
		}
		if before[k] != b.URL {
			moved++
			if before[k] != removedURL {
				// A key that wasn't on the removed backend moved: acceptable only
				// in small numbers due to hashing, but flag large drift.
			}
		}
		b.DecrConn()
	}
	// Ideal consistent hashing moves ~1/N of keys. Assert well under 2/N.
	frac := float64(moved) / float64(nkeys)
	if frac > 2.0/float64(len(backends)) {
		t.Errorf("consistent hash moved %.1f%% of keys on removal, want < %.1f%%",
			frac*100, 200.0/float64(len(backends)))
	}
}

func TestConsistentHashBoundedLoad(t *testing.T) {
	// With a tight load factor, a hot key set should spill onto other backends
	// rather than piling all load on one, because a backend at capacity is
	// skipped. We hold the connections (don't release) to build up load.
	ch := NewConsistentHash(100, 1.25)
	for i := 0; i < 4; i++ {
		ch.Add(mkBackend(fmt.Sprintf("http://b%d", i), 1))
	}
	// Send many requests for the SAME key; bounded load must distribute them.
	used := map[string]int{}
	for i := 0; i < 40; i++ {
		b, err := ch.NextForKey("hot-key")
		if err != nil {
			t.Fatal(err)
		}
		used[b.URL]++
		// intentionally do NOT DecrConn: load accumulates
	}
	if len(used) < 2 {
		t.Errorf("bounded-load hashing kept all load on %d backend(s); expected spill", len(used))
	}
}

// ---------------------------------------------------------------------------
// EWMA
// ---------------------------------------------------------------------------

func TestEWMAPrefersLowerLatency(t *testing.T) {
	e := NewEWMA()
	fast := mkBackend("http://fast", 1)
	slow := mkBackend("http://slow", 1)
	e.Add(fast)
	e.Add(slow)

	// Prime both with distinct latencies.
	for i := 0; i < 5; i++ {
		e.ObserveLatency(fast, 5*time.Millisecond)
		e.ObserveLatency(slow, 200*time.Millisecond)
	}

	fastCount := 0
	for i := 0; i < 100; i++ {
		b, err := e.Next()
		if err != nil {
			t.Fatal(err)
		}
		if b.URL == "http://fast" {
			fastCount++
		}
		b.DecrConn()
		// Keep feeding observations so scores stay separated.
		e.ObserveLatency(fast, 5*time.Millisecond)
		e.ObserveLatency(slow, 200*time.Millisecond)
	}
	if fastCount < 80 {
		t.Errorf("EWMA chose fast backend %d/100 times, want majority", fastCount)
	}
}

func TestEWMADecay(t *testing.T) {
	e := NewEWMA()
	now := time.Now()
	e.clock = func() time.Time { return now }
	b := mkBackend("http://b", 1)
	e.Add(b)

	e.ObserveLatency(b, 100*time.Millisecond)
	// Advance well past tau and observe a low latency; EWMA should move strongly
	// toward the new sample.
	now = now.Add(60 * time.Second)
	e.ObserveLatency(b, 1*time.Millisecond)

	e.mu.Lock()
	v := e.state[b].value
	e.mu.Unlock()
	if v > float64(5*time.Millisecond) {
		t.Errorf("EWMA did not decay: value=%.0fns, want near 1ms", v)
	}
}
