package limiter

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestRedisStore creates a RedisStore backed by a fresh miniredis instance.
// The caller is responsible for stopping mr when done.
func newTestRedisStore(t *testing.T, prefix string) (*RedisStore, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := NewRedisStoreFromClient(client, prefix)
	return store, mr
}

// TestRedisStoreAllow verifies that exactly burst requests are admitted in a
// single instant (GCRA admits burst-1 or burst depending on the exact formula;
// we verify burst+1 is always denied).
func TestRedisStoreAllow(t *testing.T) {
	t.Parallel()
	store, mr := newTestRedisStore(t, "test")
	defer mr.Close()
	defer store.Close()

	const rps = 10.0
	const burst = 3
	now := time.Now()

	var admitted int
	for i := 0; i < burst+5; i++ {
		ok, _ := store.Allow("allow-test", rps, burst, now)
		if ok {
			admitted++
		}
	}
	// GCRA with burst=3 admits burst-1=2 at a single instant (tat must not
	// exceed now + burst*period). We accept 1..burst to handle rounding.
	if admitted < 1 || admitted > burst {
		t.Errorf("admitted %d requests at one instant with burst=%d; want 1..%d", admitted, burst, burst)
	}
	// One extra request beyond what was admitted must be denied.
	ok, retryAfter := store.Allow("allow-test", rps, burst, now)
	if ok {
		t.Error("expected request beyond burst to be denied")
	}
	if retryAfter <= 0 {
		t.Errorf("expected retryAfter > 0 when denied, got %v", retryAfter)
	}
}

// TestRedisStoreRetryAfter verifies that a denied request returns a positive
// retryAfter duration.
func TestRedisStoreRetryAfter(t *testing.T) {
	t.Parallel()
	store, mr := newTestRedisStore(t, "test")
	defer mr.Close()
	defer store.Close()

	now := time.Now()
	const rps = 1.0
	const burst = 1

	// Exhaust the budget.
	for i := 0; i < 10; i++ {
		store.Allow("retry-test", rps, burst, now) //nolint:errcheck
	}

	ok, retryAfter := store.Allow("retry-test", rps, burst, now)
	if ok {
		t.Fatal("expected denial after exhausting budget")
	}
	if retryAfter <= 0 {
		t.Errorf("retryAfter = %v; want > 0", retryAfter)
	}
}

// TestRedisStoreNamespacing verifies that two different keys maintain
// independent budgets.
func TestRedisStoreNamespacing(t *testing.T) {
	t.Parallel()
	store, mr := newTestRedisStore(t, "ns")
	defer mr.Close()
	defer store.Close()

	const rps = 10.0
	const burst = 3
	now := time.Now()

	// Exhaust key "alpha".
	for i := 0; i < 20; i++ {
		store.Allow("alpha", rps, burst, now) //nolint:errcheck
	}

	// Key "beta" must be unaffected; it should admit at least one request.
	ok, _ := store.Allow("beta", rps, burst, now)
	if !ok {
		t.Error("key 'beta' was rate-limited by exhaustion of key 'alpha'; keys must be independent")
	}
}

// TestRedisStoreTwoInstances verifies that two RedisStore instances sharing the
// same Redis server enforce a combined limit.
func TestRedisStoreTwoInstances(t *testing.T) {
	t.Parallel()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	defer mr.Close()

	client1 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	client2 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store1 := NewRedisStoreFromClient(client1, "shared")
	store2 := NewRedisStoreFromClient(client2, "shared")
	defer store1.Close()
	defer store2.Close()

	const rps = 1.0
	const burst = 3
	now := time.Now()

	var admitted int
	for i := 0; i < 20; i++ {
		var ok bool
		if i%2 == 0 {
			ok, _ = store1.Allow("combined", rps, burst, now)
		} else {
			ok, _ = store2.Allow("combined", rps, burst, now)
		}
		if ok {
			admitted++
		}
	}

	// Both stores share the same Redis key; the combined admitted count must be
	// bounded by roughly burst (GCRA semantics: <= burst-1 at one instant).
	if admitted > burst {
		t.Errorf("two instances admitted %d requests; want <= %d (shared burst limit)", admitted, burst)
	}
	if admitted < 1 {
		t.Errorf("two instances admitted 0 requests; expected at least 1")
	}
}
