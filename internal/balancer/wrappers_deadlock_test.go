package balancer

import (
	"reverse-proxy-lb/internal/config"
	"testing"
	"time"
)

// Regression: stacking ZoneAware(PriorityTiers(...)) — the exact composition the
// server builds when prefer_same_zone is set with tiered backends — must not
// deadlock. The original wrapper design held a single non-reentrant package mutex
// across the inner Next(), so two stacked restricting wrappers self-deadlocked the
// selection goroutine.
func TestStackedWrappersNoDeadlock(t *testing.T) {
	base := NewRoundRobin()
	b := NewZoneAware(NewPriorityTiers(base), "east", true)

	pEast := NewBackend(config.BackendConfig{URL: "http://p-east", Zone: "east", Tier: 0})
	pWest := NewBackend(config.BackendConfig{URL: "http://p-west", Zone: "west", Tier: 0})
	backupEast := NewBackend(config.BackendConfig{URL: "http://backup-east", Zone: "east", Tier: 1})
	b.Add(pEast)
	b.Add(pWest)
	b.Add(backupEast)

	done := make(chan *Backend, 1)
	go func() {
		sel, err := b.Next()
		if err != nil {
			done <- nil
			return
		}
		done <- sel
	}()

	select {
	case sel := <-done:
		// In-zone (east) tier-0 leaves only p-east eligible.
		if sel != pEast {
			t.Errorf("expected in-zone tier-0 backend p-east, got %v", sel)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("b.Next() deadlocked with stacked ZoneAware(PriorityTiers)")
	}
}

// Also cover the keyed path (consistent-hash / ip_hash under the same stacking).
func TestStackedWrappersKeyedNoDeadlock(t *testing.T) {
	base := NewIPHash()
	b := NewZoneAware(NewPriorityTiers(base), "east", true)

	pEast := NewBackend(config.BackendConfig{URL: "http://p-east", Zone: "east", Tier: 0})
	pWest := NewBackend(config.BackendConfig{URL: "http://p-west", Zone: "west", Tier: 0})
	backupEast := NewBackend(config.BackendConfig{URL: "http://backup-east", Zone: "east", Tier: 1})
	b.Add(pEast)
	b.Add(pWest)
	b.Add(backupEast)

	done := make(chan *Backend, 1)
	go func() {
		sel, err := b.NextForKey("1.2.3.4")
		if err != nil {
			done <- nil
			return
		}
		done <- sel
	}()

	select {
	case sel := <-done:
		if sel != pEast {
			t.Errorf("expected in-zone tier-0 backend p-east, got %v", sel)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("NextForKey deadlocked with stacked ZoneAware(PriorityTiers)")
	}
}
