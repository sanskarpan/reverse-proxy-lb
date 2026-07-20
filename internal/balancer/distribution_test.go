package balancer

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
	"testing/quick"

	"reverse-proxy-lb/internal/config"
)

// makeEqualBackends creates n backends all with weight 1.
func makeEqualBackends(n int) []*Backend {
	bs := make([]*Backend, n)
	for i := 0; i < n; i++ {
		bs[i] = NewBackend(config.BackendConfig{
			URL:      fmt.Sprintf("http://rr-%d:8080", i),
			Weight:   1,
			MaxConns: 100000,
		})
	}
	return bs
}

// TestRoundRobinDistribution verifies that with N equal-weight backends each
// backend is selected count±1 times after N*rounds total selections.
func TestRoundRobinDistribution(t *testing.T) {
	property := func(nRaw uint8, roundsRaw uint8) bool {
		// Clamp to [2,10] and [10,50].
		n := int(nRaw%9) + 2             // [2, 10]
		rounds := int(roundsRaw%41) + 10 // [10, 50]

		rr := NewRoundRobin()
		backends := makeEqualBackends(n)
		for _, b := range backends {
			rr.Add(b)
		}

		total := n * rounds
		counts := make(map[string]int, n)
		for i := 0; i < total; i++ {
			b, err := rr.Next()
			if err != nil {
				return false
			}
			counts[b.URL]++
			b.DecrConn()
		}

		// Each backend should be selected exactly `rounds` times (±0 for RR with
		// total = n*rounds). Allow a tolerance of 1 in case of integer wrap-around.
		minC, maxC := math.MaxInt32, 0
		for _, b := range backends {
			c := counts[b.URL]
			if c < minC {
				minC = c
			}
			if c > maxC {
				maxC = c
			}
		}
		return maxC-minC <= 1
	}

	cfg := &quick.Config{MaxCount: 200}
	if err := quick.Check(property, cfg); err != nil {
		t.Errorf("RoundRobin distribution property failed: %v", err)
	}
}

// TestWeightedRandomChiSquaredDistribution verifies that each backend is selected
// proportionally to its weight within epsilon=5% over 10000 selections using a
// chi-squared goodness-of-fit test at 95% confidence.
func TestWeightedRandomChiSquaredDistribution(t *testing.T) {
	const (
		selections  = 10_000
		numBackends = 5
		// Chi-squared critical value for (numBackends-1)=4 degrees of freedom at 95% confidence.
		chiSqCritical = 9.488
	)

	rng := rand.New(rand.NewSource(42))
	weights := make([]int, numBackends)
	totalWeight := 0
	for i := range weights {
		// Random weights in [1, 20].
		weights[i] = rng.Intn(20) + 1
		totalWeight += weights[i]
	}

	wr := NewWeightedRandom()
	backends := make([]*Backend, numBackends)
	for i := 0; i < numBackends; i++ {
		backends[i] = NewBackend(config.BackendConfig{
			URL:      fmt.Sprintf("http://wr-%d:8080", i),
			Weight:   weights[i],
			MaxConns: 100000,
		})
		wr.Add(backends[i])
	}

	counts := make([]int, numBackends)
	urlToIdx := make(map[string]int, numBackends)
	for i, b := range backends {
		urlToIdx[b.URL] = i
	}

	for s := 0; s < selections; s++ {
		b, err := wr.Next()
		if err != nil {
			t.Fatalf("WeightedRandom.Next() returned error: %v", err)
		}
		counts[urlToIdx[b.URL]]++
		b.DecrConn()
	}

	// Chi-squared statistic: sum((observed - expected)^2 / expected)
	chiSq := 0.0
	for i, w := range weights {
		expected := float64(selections) * float64(w) / float64(totalWeight)
		diff := float64(counts[i]) - expected
		chiSq += (diff * diff) / expected
	}

	if chiSq >= chiSqCritical {
		t.Errorf("WeightedRandom chi-squared = %.3f >= critical %.3f (weights=%v, counts=%v)",
			chiSq, chiSqCritical, weights, counts)
	}
}

// TestConsistentHashDeterminism verifies that for any key NextForKey always
// returns the same backend given the same backend set.
func TestConsistentHashDeterminism(t *testing.T) {
	const (
		numBackends = 5
		callsPerKey = 100
	)

	ch := NewConsistentHash(100, 1.25)
	for i := 0; i < numBackends; i++ {
		b := NewBackend(config.BackendConfig{
			URL:      fmt.Sprintf("http://ch-%d:8080", i),
			Weight:   1,
			MaxConns: 100000,
		})
		ch.Add(b)
	}

	property := func(key string) bool {
		if len(key) == 0 {
			key = "default-key"
		}

		// First call determines the expected backend.
		first, err := ch.NextForKey(key)
		if err != nil {
			return false
		}
		first.DecrConn()
		expected := first.URL

		// Subsequent calls must return the same backend.
		for i := 1; i < callsPerKey; i++ {
			b, err := ch.NextForKey(key)
			if err != nil {
				return false
			}
			got := b.URL
			b.DecrConn()
			if got != expected {
				return false
			}
		}
		return true
	}

	cfg := &quick.Config{MaxCount: 100}
	if err := quick.Check(property, cfg); err != nil {
		t.Errorf("ConsistentHash determinism property failed: %v", err)
	}
}

// TestP2CLeastLoadedPreferred verifies that P2C with one heavily-loaded backend
// (100 connections) and one idle backend selects the idle one >90% of the time.
func TestP2CLeastLoadedPreferred(t *testing.T) {
	const (
		totalSelections = 1000
		minIdleRatio    = 0.90
	)

	p2c := NewP2C()

	idle := NewBackend(config.BackendConfig{
		URL:      "http://p2c-idle:8080",
		Weight:   1,
		MaxConns: 100000,
	})
	heavy := NewBackend(config.BackendConfig{
		URL:      "http://p2c-heavy:8080",
		Weight:   1,
		MaxConns: 100000,
	})

	// Pre-load the heavy backend with 100 active connections.
	for i := 0; i < 100; i++ {
		heavy.IncrConn()
	}

	p2c.Add(idle)
	p2c.Add(heavy)

	idleCount := 0
	for i := 0; i < totalSelections; i++ {
		b, err := p2c.Next()
		if err != nil {
			t.Fatalf("P2C.Next() returned error: %v", err)
		}
		if b.URL == idle.URL {
			idleCount++
		}
		// Release the connection so the idle backend's count doesn't balloon.
		b.DecrConn()
	}

	idleRatio := float64(idleCount) / float64(totalSelections)
	if idleRatio <= minIdleRatio {
		t.Errorf("P2C idle selection ratio = %.3f, want > %.2f (idleCount=%d/%d)",
			idleRatio, minIdleRatio, idleCount, totalSelections)
	}
}

// TestLeastConnPreference verifies that with 5 backends where backend 0 has 0
// connections and others have 10, LeastConnections always selects backend 0.
func TestLeastConnPreference(t *testing.T) {
	const (
		numBackends    = 5
		otherConnCount = 10
		totalChecks    = 500
	)

	lc := NewLeastConnections()
	backends := make([]*Backend, numBackends)
	for i := 0; i < numBackends; i++ {
		backends[i] = NewBackend(config.BackendConfig{
			URL:      fmt.Sprintf("http://lc-%d:8080", i),
			Weight:   1,
			MaxConns: 100000,
		})
		lc.Add(backends[i])
	}

	// Give backends 1..4 each otherConnCount connections.
	for i := 1; i < numBackends; i++ {
		for j := 0; j < otherConnCount; j++ {
			backends[i].IncrConn()
		}
	}

	// Backend 0 has 0 connections — LeastConn must always pick it.
	property := func() bool {
		// LeastConn increments the selected backend's connection count on selection.
		// We must decrement it right after to preserve the invariant that backend 0
		// always has the fewest connections across all calls.
		b, err := lc.Next()
		if err != nil {
			return false
		}
		selected := b.URL
		b.DecrConn()
		return selected == backends[0].URL
	}

	cfg := &quick.Config{MaxCount: totalChecks}
	if err := quick.Check(property, cfg); err != nil {
		t.Errorf("LeastConn preference property failed: %v", err)
	}
}
