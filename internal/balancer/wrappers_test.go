package balancer

import (
	"fmt"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
)

func zbackend(url, zone string, tier int) *Backend {
	return NewBackend(config.BackendConfig{URL: url, Weight: 1, MaxConns: 10000, Zone: zone, Tier: tier})
}

// ---------------------------------------------------------------------------
// PriorityTiers
// ---------------------------------------------------------------------------

func TestPriorityTiersFallthrough(t *testing.T) {
	inner := NewRoundRobin()
	primaryA := zbackend("http://p-a", "", 0)
	primaryB := zbackend("http://p-b", "", 0)
	backup := zbackend("http://backup", "", 1)
	inner.Add(primaryA)
	inner.Add(primaryB)
	inner.Add(backup)

	pt := NewPriorityTiers(inner)

	// While primaries are healthy, backup must never be selected.
	for i := 0; i < 50; i++ {
		b, err := pt.Next()
		if err != nil {
			t.Fatal(err)
		}
		if b.URL == "http://backup" {
			t.Fatal("backup selected while primary tier healthy")
		}
		b.DecrConn()
	}

	// Kill both primaries; selection must fall through to the backup tier.
	primaryA.SetHealthy(false)
	primaryB.SetHealthy(false)
	b, err := pt.Next()
	if err != nil {
		t.Fatal(err)
	}
	if b.URL != "http://backup" {
		t.Fatalf("expected fallthrough to backup, got %s", b.URL)
	}
	b.DecrConn()
}

// ---------------------------------------------------------------------------
// ZoneAware
// ---------------------------------------------------------------------------

func TestZoneAwarePrefersInZone(t *testing.T) {
	inner := NewRoundRobin()
	local1 := zbackend("http://local1", "us-east", 0)
	local2 := zbackend("http://local2", "us-east", 0)
	remote := zbackend("http://remote", "us-west", 0)
	inner.Add(local1)
	inner.Add(local2)
	inner.Add(remote)

	za := NewZoneAware(inner, "us-east", true)
	for i := 0; i < 50; i++ {
		b, err := za.Next()
		if err != nil {
			t.Fatal(err)
		}
		if b.Zone != "us-east" {
			t.Fatalf("selected out-of-zone backend %s (zone %s)", b.URL, b.Zone)
		}
		b.DecrConn()
	}
}

func TestZoneAwareFallsBackCrossZone(t *testing.T) {
	inner := NewRoundRobin()
	local := zbackend("http://local", "us-east", 0)
	remote := zbackend("http://remote", "us-west", 0)
	inner.Add(local)
	inner.Add(remote)

	za := NewZoneAware(inner, "us-east", true)
	local.SetHealthy(false) // no in-zone healthy backends

	b, err := za.Next()
	if err != nil {
		t.Fatal(err)
	}
	if b.URL != "http://remote" {
		t.Fatalf("expected cross-zone fallback to remote, got %s", b.URL)
	}
	b.DecrConn()
}

// ---------------------------------------------------------------------------
// SlowStart
// ---------------------------------------------------------------------------

func TestSlowStartRamps(t *testing.T) {
	inner := NewRoundRobin()
	established := zbackend("http://old", "", 0)
	inner.Add(established)

	now := time.Now()
	ss := NewSlowStart(inner, 10*time.Second)
	ss.clock = func() time.Time { return now }

	// Prime: observe the established backend as long-healthy.
	ss.observeTransitions(now.Add(-time.Hour))
	// Manually mark established as healthy long ago so it's fully ramped.
	ss.mu.Lock()
	ss.healthySince[established] = now.Add(-time.Hour)
	ss.mu.Unlock()

	// Add a fresh backend that just became healthy now.
	fresh := zbackend("http://fresh", "", 0)
	inner.Add(fresh)
	ss.observeTransitions(now)

	// Immediately after joining, the fresh backend should receive far less than
	// its fair (50%) share.
	freshCount := 0
	const n = 4000
	for i := 0; i < n; i++ {
		b, err := ss.Next()
		if err != nil {
			t.Fatal(err)
		}
		if b.URL == "http://fresh" {
			freshCount++
		}
		b.DecrConn()
	}
	earlyShare := float64(freshCount) / float64(n)
	if earlyShare > 0.30 {
		t.Errorf("slow-start: fresh backend early share %.2f, want well below 0.5", earlyShare)
	}

	// Advance past the window; now it should get close to its fair share.
	now = now.Add(20 * time.Second)
	freshCount = 0
	for i := 0; i < n; i++ {
		b, err := ss.Next()
		if err != nil {
			t.Fatal(err)
		}
		if b.URL == "http://fresh" {
			freshCount++
		}
		b.DecrConn()
	}
	lateShare := float64(freshCount) / float64(n)
	if lateShare < 0.40 {
		t.Errorf("slow-start: fresh backend share after window %.2f, want ~0.5", lateShare)
	}
}

// ---------------------------------------------------------------------------
// OutlierDetection
// ---------------------------------------------------------------------------

func TestOutlierEjectsAndReinstates(t *testing.T) {
	inner := NewRoundRobin()
	good := zbackend("http://good", "", 0)
	bad := zbackend("http://bad", "", 0)
	inner.Add(good)
	inner.Add(bad)

	now := time.Now()
	// error rate threshold 0.5, min 4 requests, eject 2s, allow up to 50%.
	od := NewOutlierDetection(inner, 0.5, 4, 2*time.Second, 50)
	od.clock = func() time.Time { return now }

	// Feed the bad backend failures past the threshold.
	for i := 0; i < 5; i++ {
		od.ObserveOutcome(bad, false)
	}
	if bad.IsHealthy() {
		t.Fatal("bad backend should have been ejected")
	}

	// Selection should now avoid the ejected backend.
	for i := 0; i < 10; i++ {
		b, err := od.Next()
		if err != nil {
			t.Fatal(err)
		}
		if b.URL == "http://bad" {
			t.Fatal("ejected backend was selected")
		}
		b.DecrConn()
	}

	// After the ejection window, Next() should reinstate it.
	now = now.Add(3 * time.Second)
	if _, err := od.Next(); err != nil {
		t.Fatal(err)
	}
	if !bad.IsHealthy() {
		t.Fatal("bad backend should have been reinstated after ejection window")
	}
}

func TestOutlierRespectsMaxEjectionPercent(t *testing.T) {
	inner := NewRoundRobin()
	var all []*Backend
	for i := 0; i < 4; i++ {
		b := zbackend(fmt.Sprintf("http://b%d", i), "", 0)
		all = append(all, b)
		inner.Add(b)
	}

	now := time.Now()
	// maxEjectionPercent 25 of 4 backends => at most 1 ejected.
	od := NewOutlierDetection(inner, 0.5, 4, time.Minute, 25)
	od.clock = func() time.Time { return now }

	// Make all four backends look bad.
	for _, b := range all {
		for i := 0; i < 5; i++ {
			od.ObserveOutcome(b, false)
		}
	}

	ejected := 0
	for _, b := range all {
		if !b.IsHealthy() {
			ejected++
		}
	}
	if ejected != 1 {
		t.Errorf("expected exactly 1 ejection under 25%% cap, got %d", ejected)
	}
}

// ---------------------------------------------------------------------------
// Composition / capability propagation
// ---------------------------------------------------------------------------

func TestWrapperKeyedPropagation(t *testing.T) {
	ch := NewConsistentHash(100, 100.0)
	for i := 0; i < 3; i++ {
		ch.Add(zbackend(fmt.Sprintf("http://b%d", i), "z", 0))
	}
	// Wrap with zone-aware (same zone for all) and priority (same tier for all)
	// so the eligible set == full healthy set and keyed selection propagates.
	wrapped := NewZoneAware(NewPriorityTiers(ch), "z", true)

	kb, ok := interface{}(wrapped).(KeyedBalancer)
	if !ok {
		t.Fatal("wrapped balancer should expose KeyedBalancer")
	}
	first, err := kb.NextForKey("stable-key")
	if err != nil {
		t.Fatal(err)
	}
	first.DecrConn()
	second, err := kb.NextForKey("stable-key")
	if err != nil {
		t.Fatal(err)
	}
	second.DecrConn()
	if first.URL != second.URL {
		t.Errorf("keyed selection through wrappers not stable: %s vs %s", first.URL, second.URL)
	}
}
