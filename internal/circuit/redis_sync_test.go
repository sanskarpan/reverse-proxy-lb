package circuit

import (
	"context"
	"strconv"
	"testing"
	"time"

	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/config"
)

// ---------------------------------------------------------------------------
// Minimal mock implementations of the redisClient interface
// ---------------------------------------------------------------------------

// mockRedisResult implements redisResult and holds a pre-canned value/error.
type mockRedisResult struct {
	val interface{}
	err error
}

func (r *mockRedisResult) Result() (interface{}, error) { return r.val, r.err }

// mockStringMapResult implements redisStringMapResult.
type mockStringMapResult struct {
	val map[string]string
	err error
}

func (r *mockStringMapResult) Result() (map[string]string, error) { return r.val, r.err }

// mockStringSliceResult implements redisStringSliceResult.
type mockStringSliceResult struct {
	val []string
	err error
}

func (r *mockStringSliceResult) Result() ([]string, error) { return r.val, r.err }

// mockRedisClient is a controllable in-memory implementation of redisClient.
// It stores state in a nested map: key -> field -> value (mirrors a Redis hash).
type mockRedisClient struct {
	// hashes stores per-key field maps, simulating Redis hashes.
	hashes map[string]map[string]string

	// evalErr, if set, causes Eval to return an error.
	evalErr error

	// evalCalls records each Eval invocation for assertion in tests.
	evalCalls []evalCall
}

type evalCall struct {
	Script string
	Keys   []string
	Args   []interface{}
}

func newMockRedisClient() *mockRedisClient {
	return &mockRedisClient{
		hashes: make(map[string]map[string]string),
	}
}

// argToString converts an Eval arg to a string for the mock implementation.
func argToString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return ""
	}
}

// Eval simulates the Lua sync script: it sets the caller's fields and returns
// HGETALL of the resulting hash. If evalErr is set it returns that instead.
func (m *mockRedisClient) Eval(ctx context.Context, script string, keys []string, args ...interface{}) redisResult {
	m.evalCalls = append(m.evalCalls, evalCall{Script: script, Keys: keys, Args: args})
	if m.evalErr != nil {
		return &mockRedisResult{err: m.evalErr}
	}
	if len(keys) == 0 || len(args) < 4 {
		return &mockRedisResult{val: []interface{}{}}
	}
	key := keys[0]
	replicaID := argToString(args[0])
	state := argToString(args[1])
	failures := argToString(args[2])
	// args[3] is TTL — not enforced by the mock.

	if m.hashes[key] == nil {
		m.hashes[key] = make(map[string]string)
	}
	now := strconv.FormatInt(time.Now().Unix(), 10)
	m.hashes[key][replicaID+":state"] = state
	m.hashes[key][replicaID+":failures"] = failures
	m.hashes[key][replicaID+":updated"] = now

	// Return HGETALL as flat []interface{}.
	var flat []interface{}
	for k, v := range m.hashes[key] {
		flat = append(flat, k, v)
	}
	return &mockRedisResult{val: flat}
}

func (m *mockRedisClient) HGetAll(ctx context.Context, key string) redisStringMapResult {
	h := m.hashes[key]
	cp := make(map[string]string, len(h))
	for k, v := range h {
		cp[k] = v
	}
	return &mockStringMapResult{val: cp}
}

func (m *mockRedisClient) Keys(ctx context.Context, pattern string) redisStringSliceResult {
	var keys []string
	for k := range m.hashes {
		keys = append(keys, k)
	}
	return &mockStringSliceResult{val: keys}
}

func (m *mockRedisClient) Close() error { return nil }

// injectRemoteState seeds the mock with another replica's state so that the
// next Eval will return it in the HGETALL result.
func (m *mockRedisClient) injectRemoteState(key, replicaID, state string, updatedAt time.Time) {
	if m.hashes[key] == nil {
		m.hashes[key] = make(map[string]string)
	}
	m.hashes[key][replicaID+":state"] = state
	m.hashes[key][replicaID+":failures"] = "0"
	m.hashes[key][replicaID+":updated"] = strconv.FormatInt(updatedAt.Unix(), 10)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestCircuitBreaker() *CircuitBreaker {
	return NewCircuitBreaker(3, 2, 10*time.Second)
}

func newTestBackend(url string) *balancer.Backend {
	return balancer.NewBackend(config.BackendConfig{URL: url})
}

// newTestSyncer builds a RedisSyncer wired to the given mock with deterministic
// IDs so tests can reason about Redis key patterns.
func newTestSyncer(cb *CircuitBreaker, mock *mockRedisClient) *RedisSyncer {
	s := &RedisSyncer{
		cb:        cb,
		client:    mock,
		replicaID: "test-replica",
		prefix:    "rplb:cb",
		ttl:       30 * time.Second,
		interval:  1 * time.Second,
		backends:  make(map[*balancer.Backend]string),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	cb.SetOnStateChange(func(b *balancer.Backend, from, to State) {
		if to == StateOpen {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			s.pushOne(ctx, b)
		}
	})
	return s
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestRedisSync_PushesStateOnOpen verifies that opening the circuit immediately
// calls Eval with state="open" via the onStateChange hook.
func TestRedisSync_PushesStateOnOpen(t *testing.T) {
	cb := newTestCircuitBreaker()
	mock := newMockRedisClient()
	backend := newTestBackend("http://localhost:9001")

	syncer := newTestSyncer(cb, mock)
	syncer.Track(backend)

	// Trip the circuit by recording failures.
	cb.RecordFailure(backend)
	cb.RecordFailure(backend)
	cb.RecordFailure(backend) // threshold = 3; circuit opens here

	// The onStateChange hook should have triggered a pushOne synchronously.
	// Allow a brief moment for any goroutines involved.
	time.Sleep(50 * time.Millisecond)

	if len(mock.evalCalls) == 0 {
		t.Fatal("expected Eval to be called when circuit opened, but got 0 calls")
	}

	// Find the Eval call where state == "open".
	found := false
	for _, call := range mock.evalCalls {
		if len(call.Args) >= 2 {
			stateArg, _ := call.Args[1].(string)
			if stateArg == "open" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("no Eval call with state=open found; calls: %+v", mock.evalCalls)
	}

	// Cleanup.
	close(syncer.stop)
}

// TestRedisSync_PullsOpenFromReplica verifies that when another replica has
// written "open" into Redis, calling sync() opens the local circuit.
func TestRedisSync_PullsOpenFromReplica(t *testing.T) {
	cb := newTestCircuitBreaker()
	mock := newMockRedisClient()
	backend := newTestBackend("http://localhost:9002")

	syncer := newTestSyncer(cb, mock)
	syncer.Track(backend)

	// Inject a remote "open" state from a different replica.
	keyHash := backendKey(backend.URL)
	redisKey := "rplb:cb:" + keyHash
	mock.injectRemoteState(redisKey, "other-replica", "open", time.Now())

	// Run one sync cycle.
	syncer.sync()

	if cb.GetState(backend) != StateOpen {
		t.Errorf("expected local circuit to be OPEN after remote replica reported open, got %v", cb.GetState(backend))
	}
	if backend.IsHealthy() {
		t.Error("expected backend to be marked unhealthy after circuit forced open")
	}

	close(syncer.stop)
}

// TestRedisSync_FallsBackOnError verifies that when Redis returns an error the
// local circuit continues operating normally (no panic, state unchanged).
func TestRedisSync_FallsBackOnError(t *testing.T) {
	cb := newTestCircuitBreaker()
	mock := newMockRedisClient()
	backend := newTestBackend("http://localhost:9003")

	// Simulate Redis being unavailable.
	mock.evalErr = errRedisUnavailable

	syncer := newTestSyncer(cb, mock)
	syncer.Track(backend)

	// Record one failure so the circuit is not at zero state.
	cb.RecordFailure(backend)

	// sync should log a warning but not crash.
	syncer.sync()

	// Local state is unaffected by the Redis failure.
	if cb.GetState(backend) != StateClosed {
		t.Errorf("expected circuit to stay closed after Redis error, got %v", cb.GetState(backend))
	}
	if !backend.IsHealthy() {
		t.Error("backend should still be healthy after Redis error with only 1 failure")
	}

	close(syncer.stop)
}

// TestRedisSync_KeyExpiry verifies that stale remote state (updated timestamp
// older than TTL) is ignored and does not force the local circuit open.
func TestRedisSync_KeyExpiry(t *testing.T) {
	cb := newTestCircuitBreaker()
	mock := newMockRedisClient()
	backend := newTestBackend("http://localhost:9004")

	syncer := newTestSyncer(cb, mock)
	syncer.Track(backend)

	// Inject remote "open" with a timestamp far in the past (expired).
	keyHash := backendKey(backend.URL)
	redisKey := "rplb:cb:" + keyHash
	staleTime := time.Now().Add(-2 * syncer.ttl) // 2x TTL in the past
	mock.injectRemoteState(redisKey, "old-replica", "open", staleTime)

	// Run one sync cycle.
	syncer.sync()

	// Stale state should be ignored; local circuit must stay closed.
	if cb.GetState(backend) != StateClosed {
		t.Errorf("expected local circuit to remain CLOSED when remote state is stale, got %v", cb.GetState(backend))
	}

	close(syncer.stop)
}

// TestRedisSync_LocalRecoveryNotBlockedByRemoteOpen verifies that once the local
// circuit has closed naturally (via RecordSuccess reaching the success threshold),
// a subsequent sync tick that reads a remote replica's still-open entry does NOT
// re-force the local circuit back to OPEN (Bug 3).
func TestRedisSync_LocalRecoveryNotBlockedByRemoteOpen(t *testing.T) {
	// Use a controllable clock so we can advance time without real sleeps.
	now := time.Now()
	clk := func() time.Time { return now }

	cb := NewCircuitBreakerWithConfig(Config{
		Mode:             ModeConsecutive,
		FailureThreshold: 3,
		SuccessThreshold: 2,
		Timeout:          10 * time.Second,
	})
	cb.setClock(clk)

	mock := newMockRedisClient()
	backend := newTestBackend("http://localhost:9005")

	syncer := newTestSyncer(cb, mock)
	syncer.Track(backend)

	// Trip the local circuit.
	cb.RecordFailure(backend)
	cb.RecordFailure(backend)
	cb.RecordFailure(backend) // circuit opens here
	if cb.GetState(backend) != StateOpen {
		t.Fatal("expected circuit to be OPEN after 3 failures")
	}

	// Record the timestamp when "other-replica" was open (before our recovery).
	remoteOpenedAt := now
	keyHash := backendKey(backend.URL)
	redisKey := "rplb:cb:" + keyHash
	mock.injectRemoteState(redisKey, "other-replica", "open", remoteOpenedAt)

	// Advance the clock past the circuit timeout so Allow() transitions to HalfOpen.
	now = now.Add(15 * time.Second)

	// Allow() transitions to half-open; two successes close the circuit.
	if err := cb.Allow(backend); err != nil {
		t.Fatalf("Allow in half-open failed: %v", err)
	}
	cb.RecordSuccess(backend)
	if err := cb.Allow(backend); err != nil {
		t.Fatalf("Allow second probe failed: %v", err)
	}
	cb.RecordSuccess(backend) // successThreshold=2 → circuit closes

	if cb.GetState(backend) != StateClosed {
		t.Fatalf("expected circuit to be CLOSED after recovery, got %v", cb.GetState(backend))
	}

	// Now run a sync tick: the remote replica is still "open" in the mock (its
	// entry has not been updated), but the local circuit has already recovered.
	// The remote entry's updated timestamp is BEFORE our closedAt, so it must be
	// ignored and the local circuit must remain CLOSED.
	syncer.sync()

	if cb.GetState(backend) != StateClosed {
		t.Errorf("local recovery was blocked by remote-still-open entry: expected CLOSED, got %v", cb.GetState(backend))
	}
	if !backend.IsHealthy() {
		t.Error("backend should be healthy after local circuit recovery")
	}

	close(syncer.stop)
}

// errRedisUnavailable is a sentinel error returned by the mock when Redis is down.
var errRedisUnavailable = _errRedisUnavailable("redis: connection refused")

type _errRedisUnavailable string

func (e _errRedisUnavailable) Error() string { return string(e) }
