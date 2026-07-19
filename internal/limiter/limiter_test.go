package limiter

import (
	"strconv"
	"testing"
	"time"
)

func TestRateLimiterGlobalLimit(t *testing.T) {
	rl := NewRateLimiter(10, 5)

	for i := 0; i < 5; i++ {
		if err := rl.Allow("client1"); err != nil {
			t.Errorf("Expected no error on request %d, got %v", i, err)
		}
	}

	err := rl.Allow("client1")
	if err == nil {
		t.Error("Expected rate limit error after burst exceeded")
	}
}

func TestRateLimiterPerClientLimit(t *testing.T) {
	rl := NewRateLimiter(100, 2)

	for i := 0; i < 2; i++ {
		if err := rl.Allow("client1"); err != nil {
			t.Errorf("Expected no error on request %d, got %v", i, err)
		}
	}

	err := rl.Allow("client1")
	if err == nil {
		t.Error("Expected per-client rate limit error after burst exceeded")
	}
}

func TestRateLimiterUpdateRate(t *testing.T) {
	rl := NewRateLimiter(10, 5)

	rl.UpdateRate(20, 10)

	for i := 0; i < 10; i++ {
		if err := rl.Allow("client1"); err != nil {
			t.Errorf("Expected no error on request %d after update, got %v", i, err)
		}
	}
}

func TestRateLimiterStartStop(t *testing.T) {
	rl := NewRateLimiter(10, 5)
	rl.Start()

	select {
	case <-rl.stopCh:
		t.Error("stopCh should not be closed yet")
	default:
	}

	rl.Stop()

	select {
	case <-rl.stopCh:
	default:
		t.Error("stopCh should be closed after Stop()")
	}
}

// ID 8: UpdateRate must actually update EXISTING per-key limiters, not just new ones.
func TestRateLimiterUpdateRateAffectsExistingKeys(t *testing.T) {
	rl := NewRateLimiter(10, 5)
	// Create a per-key limiter.
	_ = rl.Allow("existing-client")

	if got := rl.limiters["existing-client"].limiter.Burst(); got != 5 {
		t.Fatalf("precondition: expected burst 5, got %d", got)
	}

	rl.UpdateRate(20, 50)

	entry, ok := rl.limiters["existing-client"]
	if !ok {
		t.Fatal("existing per-key limiter disappeared after UpdateRate")
	}
	if entry.limiter.Burst() != 50 {
		t.Errorf("existing per-key limiter burst not updated: got %d, want 50", entry.limiter.Burst())
	}
	if float64(entry.limiter.Limit()) != 20 {
		t.Errorf("existing per-key limiter rate not updated: got %v, want 20", entry.limiter.Limit())
	}
}

// FIX 4: cleanup() must evict only idle (stale) keys and preserve recently-used ones.
func TestRateLimiterCleanupEvictsOnlyStaleKeys(t *testing.T) {
	rl := NewRateLimiter(100, 5)
	rl.idleTTL = 50 * time.Millisecond
	rl.cleanupInterval = time.Millisecond // fast tick for the test

	// Create two per-key limiters.
	_ = rl.Allow("stale-client")
	_ = rl.Allow("active-client")

	// Let "stale-client" age past the idle TTL while keeping "active-client" fresh.
	time.Sleep(60 * time.Millisecond)
	_ = rl.Allow("active-client") // refresh lastSeen

	rl.Start()
	defer rl.Stop()

	// Wait for at least one cleanup tick to process the eviction.
	deadline := time.Now().Add(2 * time.Second)
	for {
		rl.mu.RLock()
		_, staleExists := rl.limiters["stale-client"]
		_, activeExists := rl.limiters["active-client"]
		rl.mu.RUnlock()

		if !staleExists && activeExists {
			break // desired state reached
		}
		if !activeExists {
			t.Fatal("cleanup wrongly evicted the recently-used active-client")
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup did not evict stale-client within deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// FIX 4: the per-key map must stay bounded by maxEntries even when many distinct
// keys are inserted between cleanup ticks.
func TestRateLimiterMapStaysBounded(t *testing.T) {
	rl := NewRateLimiter(1e9, 1) // huge global rate so global limiter never blocks
	rl.maxEntries = 10

	for i := 0; i < 1000; i++ {
		key := "client-" + strconv.Itoa(i)
		_ = rl.Allow(key)

		rl.mu.RLock()
		n := len(rl.limiters)
		rl.mu.RUnlock()
		if n > rl.maxEntries {
			t.Fatalf("map exceeded maxEntries: got %d, want <= %d", n, rl.maxEntries)
		}
	}

	rl.mu.RLock()
	final := len(rl.limiters)
	rl.mu.RUnlock()
	if final > rl.maxEntries {
		t.Fatalf("final map size %d exceeds maxEntries %d", final, rl.maxEntries)
	}
}

// 4.1: global and per-key limits must be independently configurable. Here the
// global aggregate is capped tighter than any single key's own bucket.
func TestGlobalVsPerKeyIndependence(t *testing.T) {
	// Per-key: generous (rps 1000, burst 100). Global: tight (rps 1, burst 3).
	rl := NewRateLimiterWithOptions(Options{
		Algorithm:   AlgorithmTokenBucket,
		PerKeyRPS:   1000,
		PerKeyBurst: 100,
		GlobalRPS:   1,
		GlobalBurst: 3,
	})

	// Spread requests across distinct keys so no per-key bucket is exhausted;
	// only the global aggregate should throttle.
	allowed := 0
	for i := 0; i < 10; i++ {
		key := "client-" + strconv.Itoa(i)
		if ok, _ := rl.AllowKey(key); ok {
			allowed++
		}
	}
	if allowed != 3 {
		t.Fatalf("global aggregate should cap at burst=3 across keys, allowed=%d", allowed)
	}
}

// 4.1: a single key must be capped by its own per-key bucket even when the
// global limiter has plenty of headroom.
func TestPerKeyCapsEachKey(t *testing.T) {
	rl := NewRateLimiterWithOptions(Options{
		PerKeyRPS:   1,
		PerKeyBurst: 2,
		GlobalRPS:   1000,
		GlobalBurst: 1000,
	})

	// key A gets 2 then throttles.
	if ok, _ := rl.AllowKey("A"); !ok {
		t.Fatal("A req1 should pass")
	}
	if ok, _ := rl.AllowKey("A"); !ok {
		t.Fatal("A req2 should pass")
	}
	if ok, _ := rl.AllowKey("A"); ok {
		t.Fatal("A req3 should be throttled by per-key bucket")
	}
	// key B has its own independent budget.
	if ok, _ := rl.AllowKey("B"); !ok {
		t.Fatal("B req1 should pass on its own per-key bucket")
	}
}

// 4.6: GCRA allows a burst up to `burst` then throttles, and refills at ~rps.
func TestGCRABurstThenThrottle(t *testing.T) {
	rl := NewRateLimiterWithOptions(Options{
		Algorithm:   AlgorithmGCRA,
		PerKeyRPS:   100,
		PerKeyBurst: 5,
		GlobalRPS:   1e9, // global never blocks
		GlobalBurst: 1e9,
	})

	allowed := 0
	for i := 0; i < 20; i++ {
		if ok, _ := rl.AllowKey("gcra-client"); ok {
			allowed++
		}
	}
	// Burst is 5; a couple extra may slip through as emission time elapses during
	// the tight loop, but it must be close to the burst and far below 20.
	if allowed < 5 || allowed > 8 {
		t.Fatalf("GCRA burst: expected ~5 allowed, got %d", allowed)
	}

	// After waiting one emission (~10ms at 100rps), one more should be allowed.
	time.Sleep(15 * time.Millisecond)
	if ok, _ := rl.AllowKey("gcra-client"); !ok {
		t.Fatal("GCRA should admit a steady-rate request after one emission window")
	}
}

// 4.6: GCRA sustains approximately rps over time.
func TestGCRASteadyRate(t *testing.T) {
	g := newGCRA(200, 1) // burst 1: pure steady rate, 5ms spacing
	now := time.Now()

	if ok, _ := g.reserve(now); !ok {
		t.Fatal("first event should pass")
	}
	// Immediately after, no burst allowance => throttled.
	if ok, retry := g.reserve(now); ok {
		t.Fatal("second immediate event should be throttled at burst=1")
	} else if retry <= 0 {
		t.Fatalf("throttled event should report positive retry-after, got %v", retry)
	}
	// After one emission window it is allowed again.
	if ok, _ := g.reserve(now.Add(5 * time.Millisecond)); !ok {
		t.Fatal("event after one emission window should pass")
	}
}

// 4.3: a rule sub-limiter throttles its route independently from the default
// per-key bucket.
func TestRuleLimiterThrottlesIndependently(t *testing.T) {
	rl := NewRateLimiterWithOptions(Options{
		PerKeyRPS:   1000,
		PerKeyBurst: 1000,
		GlobalRPS:   1000,
		GlobalBurst: 1000,
	})
	rl.AddRule("login", 1, 2) // tight rule budget

	// The rule budget for key A is burst=2.
	if ok, _ := rl.AllowRule("login", "A"); !ok {
		t.Fatal("rule req1 should pass")
	}
	if ok, _ := rl.AllowRule("login", "A"); !ok {
		t.Fatal("rule req2 should pass")
	}
	if ok, _ := rl.AllowRule("login", "A"); ok {
		t.Fatal("rule req3 should be throttled by the rule sub-limiter")
	}
	// The default per-key bucket for the same key is untouched and generous.
	if ok, _ := rl.AllowKey("A"); !ok {
		t.Fatal("default per-key bucket should not be affected by the rule limiter")
	}
	// A different key has its own rule budget.
	if ok, _ := rl.AllowRule("login", "B"); !ok {
		t.Fatal("rule budget should be per-key: B should pass")
	}
}

// 4.4: throttled requests report a sane positive Retry-After duration.
func TestRetryAfterPositiveWhenThrottled(t *testing.T) {
	rl := NewRateLimiter(1, 1) // rps 1, burst 1

	if ok, _ := rl.AllowKey("client"); !ok {
		t.Fatal("first request should pass")
	}
	ok, retry := rl.AllowKey("client")
	if ok {
		t.Fatal("second immediate request should be throttled")
	}
	if retry <= 0 {
		t.Fatalf("retry-after should be positive when throttled, got %v", retry)
	}
	// At 1 rps the wait should be on the order of ~1s, comfortably under 2s.
	if retry > 2*time.Second {
		t.Fatalf("retry-after unreasonably large: %v", retry)
	}
}

// Back-compat: the shipped Allow(key) error API still works and reports global
// vs per-ip scope in the message.
func TestAllowBackCompatErrorScopes(t *testing.T) {
	// Global tighter than per-key: exhausting the global reports "global".
	rl := NewRateLimiterWithOptions(Options{PerKeyRPS: 100, PerKeyBurst: 100, GlobalRPS: 1, GlobalBurst: 1})
	if err := rl.Allow("A"); err != nil {
		t.Fatalf("first request should pass: %v", err)
	}
	if err := rl.Allow("B"); err == nil {
		t.Fatal("global exhaustion should return an error")
	} else if want := "global"; !contains(err.Error(), want) {
		t.Fatalf("expected global scope in error, got %q", err.Error())
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// UpdateRates sets global and per-key limits independently and rebuilds existing
// entries.
func TestUpdateRatesIndependent(t *testing.T) {
	rl := NewRateLimiter(10, 5)
	_ = rl.Allow("existing")

	rl.UpdateRates(20, 50, 3, 7)

	entry := rl.limiters["existing"]
	if entry.limiter.Burst() != 50 {
		t.Errorf("per-key burst not updated: got %d want 50", entry.limiter.Burst())
	}
	if float64(entry.limiter.Limit()) != 20 {
		t.Errorf("per-key rate not updated: got %v want 20", entry.limiter.Limit())
	}
	if rl.globalBurst != 7 || rl.globalRPS != 3 {
		t.Errorf("global limits not updated independently: rps=%v burst=%d", rl.globalRPS, rl.globalBurst)
	}
}
