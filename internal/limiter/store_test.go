package limiter

import (
	"testing"
	"time"
)

// A shared Store enforces a COMBINED limit across multiple RateLimiter instances
// (simulating a fleet sharing e.g. Redis). Two limiters share one MemStore with a
// global budget of burst=5; across both, only ~5 requests are admitted immediately.
func TestSharedStoreCombinedLimitAcrossInstances(t *testing.T) {
	store := NewMemStore()

	inst1 := NewRateLimiter(1, 1) // generous local so the store is the binding limit
	inst2 := NewRateLimiter(1, 1)
	// Local limits high, shared limit low.
	inst1.UpdateRates(10000, 10000, 10000, 10000)
	inst2.UpdateRates(10000, 10000, 10000, 10000)
	inst1.SetStore(store, 1, 5, "tenant-A") // rps 1, burst 5
	inst2.SetStore(store, 1, 5, "tenant-A")

	allowed := 0
	// Alternate requests across the two "instances"; the shared burst is 5.
	for i := 0; i < 20; i++ {
		inst := inst1
		if i%2 == 1 {
			inst = inst2
		}
		if ok, _ := inst.AllowKey("client"); ok {
			allowed++
		}
	}
	if allowed < 4 || allowed > 5 {
		t.Errorf("combined admitted = %d, want ~5 (shared burst), proving cross-instance limiting", allowed)
	}
}

// The MemStore GCRA admits burst then throttles and refills over time.
func TestMemStoreBurstAndRefill(t *testing.T) {
	m := NewMemStore()
	now := time.Now()
	adm := 0
	for i := 0; i < 10; i++ {
		if ok, _ := m.Allow("k", 10, 3, now); ok { // rps 10, burst 3
			adm++
		}
	}
	if adm != 2 { // GCRA admits burst-1 at a single instant
		t.Fatalf("burst admitted = %d, want 2 (GCRA burst-1)", adm)
	}
	// After ~1 emission (100ms at 10rps), one more is admitted.
	if ok, _ := m.Allow("k", 10, 3, now.Add(120*time.Millisecond)); !ok {
		t.Errorf("expected admission after refill window")
	}
}
