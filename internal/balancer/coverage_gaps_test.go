// coverage_gaps_test.go adds targeted tests for code paths that were at 0%
// or very low coverage after the initial test run: registry.NewByAlgorithm,
// IPHash, ConsistentHash.Next, and the wrapper delegate methods (Remove, All,
// UpdateWeight, pickFrom/pickFromKey on ZoneAware/OutlierDetection/SlowStart).
package balancer

import (
	"fmt"
	"testing"

	"reverse-proxy-lb/internal/config"
)

// ---------------------------------------------------------------------------
// registry.go — NewByAlgorithm
// ---------------------------------------------------------------------------

func TestNewByAlgorithmAllKnown(t *testing.T) {
	algorithms := []string{
		"round_robin",
		"least_conn",
		"weighted",
		"swrr",
		"weighted_least_conn",
		"weighted_random",
		"p2c",
		"consistent_hash",
		"ewma",
		"ip_hash",
	}
	for _, alg := range algorithms {
		t.Run(alg, func(t *testing.T) {
			b, err := NewByAlgorithm(alg, Options{})
			if err != nil {
				t.Fatalf("NewByAlgorithm(%q) returned error: %v", alg, err)
			}
			if b == nil {
				t.Fatalf("NewByAlgorithm(%q) returned nil balancer", alg)
			}
		})
	}
}

func TestNewByAlgorithmUnknown(t *testing.T) {
	_, err := NewByAlgorithm("nonexistent", Options{})
	if err == nil {
		t.Fatal("expected error for unknown algorithm, got nil")
	}
}

func TestNewByAlgorithmConsistentHashOptions(t *testing.T) {
	// Non-zero options should be passed through.
	b, err := NewByAlgorithm("consistent_hash", Options{
		ConsistentHashReplicas:   50,
		ConsistentHashLoadFactor: 1.5,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b == nil {
		t.Fatal("nil balancer returned")
	}
}

// ---------------------------------------------------------------------------
// iphash.go — NextForIP, NextForKey, Next
// ---------------------------------------------------------------------------

func TestIPHashNextForIP(t *testing.T) {
	ih := NewIPHash()
	b1 := NewBackend(config.BackendConfig{URL: "http://b1", Weight: 1, MaxConns: 100})
	b2 := NewBackend(config.BackendConfig{URL: "http://b2", Weight: 1, MaxConns: 100})
	ih.Add(b1)
	ih.Add(b2)

	// Same IP must always map to the same backend.
	first, err := ih.NextForIP("10.0.0.1")
	if err != nil {
		t.Fatalf("NextForIP error: %v", err)
	}
	first.DecrConn()

	for i := 0; i < 20; i++ {
		got, err := ih.NextForIP("10.0.0.1")
		if err != nil {
			t.Fatalf("NextForIP iteration %d error: %v", i, err)
		}
		if got.URL != first.URL {
			t.Errorf("iteration %d: IP hash not stable: got %s, want %s", i, got.URL, first.URL)
		}
		got.DecrConn()
	}
}

func TestIPHashNextForKey(t *testing.T) {
	ih := NewIPHash()
	b := NewBackend(config.BackendConfig{URL: "http://b", Weight: 1, MaxConns: 100})
	ih.Add(b)

	got, err := ih.NextForKey("any-key")
	if err != nil {
		t.Fatalf("NextForKey error: %v", err)
	}
	if got == nil {
		t.Fatal("got nil backend from NextForKey")
	}
	got.DecrConn()
}

func TestIPHashNextError(t *testing.T) {
	ih := NewIPHash()
	ih.Add(NewBackend(config.BackendConfig{URL: "http://b", Weight: 1, MaxConns: 100}))

	_, err := ih.Next()
	if err == nil {
		t.Fatal("IPHash.Next() should return an error (requires client IP)")
	}
}

func TestIPHashNoHealthy(t *testing.T) {
	ih := NewIPHash()
	b := NewBackend(config.BackendConfig{URL: "http://b", Weight: 1, MaxConns: 100})
	b.SetHealthy(false)
	ih.Add(b)

	_, err := ih.NextForIP("10.0.0.1")
	if err == nil {
		t.Fatal("NextForIP should error when no healthy backends")
	}
}

// ---------------------------------------------------------------------------
// consistenthash.go — Next (fallback with random key)
// ---------------------------------------------------------------------------

func TestConsistentHashNext(t *testing.T) {
	ch := NewConsistentHash(100, 1.25)
	for i := 0; i < 3; i++ {
		ch.Add(NewBackend(config.BackendConfig{
			URL: fmt.Sprintf("http://b%d", i), Weight: 1, MaxConns: 100,
		}))
	}

	// Next should not error.
	for i := 0; i < 20; i++ {
		b, err := ch.Next()
		if err != nil {
			t.Fatalf("ConsistentHash.Next() error: %v", err)
		}
		b.DecrConn()
	}
}

// ---------------------------------------------------------------------------
// wrappers.go — Remove, All, UpdateWeight on PriorityTiers
// ---------------------------------------------------------------------------

func TestPriorityTiersDelegates(t *testing.T) {
	inner := NewRoundRobin()
	b1 := zbackend("http://b1", "z", 0)
	b2 := zbackend("http://b2", "z", 0)
	inner.Add(b1)
	inner.Add(b2)
	pt := NewPriorityTiers(inner)

	// All/GetHealthy must return both backends initially.
	if got := len(pt.All()); got != 2 {
		t.Errorf("All() = %d, want 2", got)
	}
	if got := len(pt.GetHealthy()); got != 2 {
		t.Errorf("GetHealthy() = %d, want 2", got)
	}

	// UpdateWeight should propagate.
	pt.UpdateWeight(b1, 5)
	if b1.GetWeight() != 5 {
		t.Errorf("UpdateWeight not propagated: weight = %d, want 5", b1.GetWeight())
	}

	// Remove should propagate.
	pt.Remove(b1)
	if got := len(pt.All()); got != 1 {
		t.Errorf("after Remove, All() = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// wrappers.go — ZoneAware delegates (Remove, All, UpdateWeight, pickFrom/pickFromKey)
// ---------------------------------------------------------------------------

func TestZoneAwareDelegates(t *testing.T) {
	inner := NewRoundRobin()
	b1 := zbackend("http://b1", "us-east", 0)
	b2 := zbackend("http://b2", "us-west", 0)
	inner.Add(b1)
	inner.Add(b2)
	za := NewZoneAware(inner, "us-east", true)

	if got := len(za.All()); got != 2 {
		t.Errorf("All() = %d, want 2", got)
	}

	za.UpdateWeight(b1, 3)
	if b1.GetWeight() != 3 {
		t.Errorf("UpdateWeight not propagated: weight = %d, want 3", b1.GetWeight())
	}

	za.Remove(b2)
	if got := len(za.All()); got != 1 {
		t.Errorf("after Remove, All() = %d, want 1", got)
	}
}

func TestZoneAwarePickFromSubset(t *testing.T) {
	// Build an outer PriorityTiers that wraps ZoneAware so pickFrom/pickFromKey
	// are invoked on ZoneAware by the outer wrapper.
	inner := NewRoundRobin()
	b1 := zbackend("http://b1", "us-east", 0)
	b2 := zbackend("http://b2", "us-east", 0)
	b3 := zbackend("http://b3", "us-west", 1) // different tier + zone
	inner.Add(b1)
	inner.Add(b2)
	inner.Add(b3)

	za := NewZoneAware(inner, "us-east", true)
	pt := NewPriorityTiers(za)

	// With all healthy, tier-0 in-zone backends should always win.
	for i := 0; i < 20; i++ {
		b, err := pt.Next()
		if err != nil {
			t.Fatalf("pt.Next() error: %v", err)
		}
		if b.Zone != "us-east" {
			t.Errorf("selected out-of-zone backend %s", b.URL)
		}
		b.DecrConn()
	}
}

func TestZoneAwareNextForKey(t *testing.T) {
	ch := NewConsistentHash(100, 100.0)
	b1 := zbackend("http://b1", "us-east", 0)
	b2 := zbackend("http://b2", "us-east", 0)
	ch.Add(b1)
	ch.Add(b2)

	za := NewZoneAware(ch, "us-east", true)
	first, err := za.NextForKey("stable-key")
	if err != nil {
		t.Fatalf("NextForKey error: %v", err)
	}
	first.DecrConn()

	// Same key should map to same backend.
	for i := 0; i < 10; i++ {
		got, err := za.NextForKey("stable-key")
		if err != nil {
			t.Fatalf("NextForKey iteration %d error: %v", i, err)
		}
		if got.URL != first.URL {
			t.Errorf("iteration %d: keyed selection not stable", i)
		}
		got.DecrConn()
	}
}

// ---------------------------------------------------------------------------
// wrappers.go — SlowStart delegates + NextForKey
// ---------------------------------------------------------------------------

func TestSlowStartDelegates(t *testing.T) {
	inner := NewRoundRobin()
	b1 := zbackend("http://b1", "", 0)
	b2 := zbackend("http://b2", "", 0)
	inner.Add(b1)
	inner.Add(b2)
	ss := NewSlowStart(inner, 0) // window=0 → always fully ramped

	if got := len(ss.All()); got != 2 {
		t.Errorf("All() = %d, want 2", got)
	}
	if got := len(ss.GetHealthy()); got != 2 {
		t.Errorf("GetHealthy() = %d, want 2", got)
	}
	ss.UpdateWeight(b1, 7)
	if b1.GetWeight() != 7 {
		t.Errorf("UpdateWeight not propagated: weight = %d, want 7", b1.GetWeight())
	}
	ss.Remove(b2)
	if got := len(ss.All()); got != 1 {
		t.Errorf("after Remove, All() = %d, want 1", got)
	}
}

func TestSlowStartNextForKey(t *testing.T) {
	ch := NewConsistentHash(100, 100.0)
	b := zbackend("http://b", "", 0)
	ch.Add(b)
	ss := NewSlowStart(ch, 0)

	got, err := ss.NextForKey("my-key")
	if err != nil {
		t.Fatalf("SlowStart.NextForKey error: %v", err)
	}
	if got == nil {
		t.Fatal("got nil backend")
	}
	got.DecrConn()
}

func TestSlowStartPickFrom(t *testing.T) {
	inner := NewRoundRobin()
	b1 := zbackend("http://b1", "", 0)
	b2 := zbackend("http://b2", "", 0)
	inner.Add(b1)
	inner.Add(b2)
	ss := NewSlowStart(inner, 0)

	// Wrap with PriorityTiers so pickFrom/pickFromKey on SlowStart get called.
	pt := NewPriorityTiers(ss)
	for i := 0; i < 20; i++ {
		b, err := pt.Next()
		if err != nil {
			t.Fatalf("iteration %d: pt.Next() error: %v", i, err)
		}
		b.DecrConn()
	}
}

// ---------------------------------------------------------------------------
// wrappers.go — OutlierDetection delegates + NextForKey + pickFrom/pickFromKey
// ---------------------------------------------------------------------------

func TestOutlierDetectionDelegates(t *testing.T) {
	inner := NewRoundRobin()
	b1 := zbackend("http://b1", "", 0)
	b2 := zbackend("http://b2", "", 0)
	inner.Add(b1)
	inner.Add(b2)
	od := NewOutlierDetection(inner, 0.5, 4, 0, 50)

	if got := len(od.All()); got != 2 {
		t.Errorf("All() = %d, want 2", got)
	}
	if got := len(od.GetHealthy()); got != 2 {
		t.Errorf("GetHealthy() = %d, want 2", got)
	}

	od.UpdateWeight(b1, 9)
	if b1.GetWeight() != 9 {
		t.Errorf("UpdateWeight not propagated: weight = %d, want 9", b1.GetWeight())
	}

	od.Remove(b2)
	if got := len(od.All()); got != 1 {
		t.Errorf("after Remove, All() = %d, want 1", got)
	}
}

func TestOutlierDetectionNextForKey(t *testing.T) {
	ch := NewConsistentHash(100, 100.0)
	b := zbackend("http://b", "", 0)
	ch.Add(b)
	od := NewOutlierDetection(ch, 0.5, 4, 0, 50)

	got, err := od.NextForKey("my-key")
	if err != nil {
		t.Fatalf("OutlierDetection.NextForKey error: %v", err)
	}
	got.DecrConn()
}

func TestOutlierDetectionPickFromPickFromKey(t *testing.T) {
	inner := NewRoundRobin()
	b1 := zbackend("http://b1", "", 0)
	b2 := zbackend("http://b2", "", 1) // different tier
	inner.Add(b1)
	inner.Add(b2)
	od := NewOutlierDetection(inner, 0.5, 4, 0, 50)
	pt := NewPriorityTiers(od)

	// PriorityTiers calls od.pickFrom; only tier-0 backends should be returned.
	for i := 0; i < 20; i++ {
		b, err := pt.Next()
		if err != nil {
			t.Fatalf("iteration %d: pt.Next() error: %v", i, err)
		}
		if b.Tier != 0 {
			t.Errorf("expected tier-0 backend, got tier %d (%s)", b.Tier, b.URL)
		}
		b.DecrConn()
	}
}

// ---------------------------------------------------------------------------
// wrappers.go — ObserveOutcome nil-safety
// ---------------------------------------------------------------------------

func TestOutlierDetectionObserveNilBackend(t *testing.T) {
	inner := NewRoundRobin()
	od := NewOutlierDetection(inner, 0.5, 1, 0, 50)
	// Calling ObserveOutcome with a nil backend should not panic.
	od.ObserveOutcome(nil, false)
	od.ObserveOutcome(nil, true)
}

// ---------------------------------------------------------------------------
// hashPick / statelessPick empty-set guard
// ---------------------------------------------------------------------------

func TestHashPickAndStatelessPickEmpty(t *testing.T) {
	_, err := hashPick(nil, "key")
	if err == nil {
		t.Error("hashPick with nil candidates should return error")
	}
	_, err = statelessPick(nil)
	if err == nil {
		t.Error("statelessPick with nil candidates should return error")
	}
}
