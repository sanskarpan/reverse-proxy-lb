package circuit

import (
	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/config"
	"sync"
	"testing"
	"time"
)

func TestCircuitBreakerClosedState(t *testing.T) {
	cb := NewCircuitBreaker(3, 2, 10*time.Second)

	backend := balancer.NewBackend(config.BackendConfig{URL: "http://localhost:8001"})

	err := cb.Allow(backend)
	if err != nil {
		t.Errorf("Expected no error in closed state, got %v", err)
	}
}

func TestCircuitBreakerOpenState(t *testing.T) {
	cb := NewCircuitBreaker(3, 2, 10*time.Second)

	backend := balancer.NewBackend(config.BackendConfig{URL: "http://localhost:8001"})

	for i := 0; i < 3; i++ {
		cb.RecordFailure(backend)
	}

	err := cb.Allow(backend)
	if err == nil {
		t.Error("Expected error in open state")
	}

	if backend.IsHealthy() {
		t.Error("Backend should be marked unhealthy")
	}
}

func TestCircuitBreakerHalfOpenState(t *testing.T) {
	cb := NewCircuitBreaker(3, 2, 1*time.Second)

	backend := balancer.NewBackend(config.BackendConfig{URL: "http://localhost:8001"})

	for i := 0; i < 3; i++ {
		cb.RecordFailure(backend)
	}

	time.Sleep(1500 * time.Millisecond)

	err := cb.Allow(backend)
	if err != nil {
		t.Errorf("Expected no error in half-open state, got %v", err)
	}

	state := cb.GetState(backend)
	if state != StateHalfOpen {
		t.Errorf("Expected state HalfOpen, got %v", state)
	}
}

func TestCircuitBreakerRecovery(t *testing.T) {
	cb := NewCircuitBreaker(3, 2, 500*time.Millisecond)

	backend := balancer.NewBackend(config.BackendConfig{URL: "http://localhost:8001"})

	for i := 0; i < 3; i++ {
		cb.RecordFailure(backend)
	}

	time.Sleep(600 * time.Millisecond)
	cb.Allow(backend)

	for i := 0; i < 2; i++ {
		cb.RecordSuccess(backend)
	}

	state := cb.GetState(backend)
	if state != StateClosed {
		t.Errorf("Expected state Closed after recovery, got %v", state)
	}

	if !backend.IsHealthy() {
		t.Error("Backend should be marked healthy")
	}
}

func TestCircuitBreakerReset(t *testing.T) {
	cb := NewCircuitBreaker(3, 2, 10*time.Second)

	backend := balancer.NewBackend(config.BackendConfig{URL: "http://localhost:8001"})

	for i := 0; i < 3; i++ {
		cb.RecordFailure(backend)
	}

	cb.Reset(backend)

	err := cb.Allow(backend)
	if err != nil {
		t.Errorf("Expected no error after reset, got %v", err)
	}
}

// ID 5: while half-open, only a bounded number of probe requests may pass through,
// preventing a thundering herd against a recovering backend.
func TestCircuitBreakerHalfOpenProbeLimit(t *testing.T) {
	cb := NewCircuitBreaker(3, 2, 1*time.Millisecond) // successThreshold 2 => 2 probes

	backend := balancer.NewBackend(config.BackendConfig{URL: "http://localhost:8001"})
	for i := 0; i < 3; i++ {
		cb.RecordFailure(backend)
	}
	time.Sleep(5 * time.Millisecond)

	// First probe transitions to half-open and is admitted.
	if err := cb.Allow(backend); err != nil {
		t.Fatalf("first probe should be admitted, got %v", err)
	}
	// Second probe still within the limit (successThreshold=2).
	if err := cb.Allow(backend); err != nil {
		t.Fatalf("second probe should be admitted, got %v", err)
	}
	// Third concurrent probe must be rejected.
	if err := cb.Allow(backend); err == nil {
		t.Error("expected probe limit to reject the third half-open request")
	}
}

// A single failure on a never-seen backend must NOT open the circuit before the
// failure threshold is reached.
func TestCircuitBreakerSingleFailureDoesNotOpen(t *testing.T) {
	cb := NewCircuitBreaker(3, 2, 10*time.Second)
	backend := balancer.NewBackend(config.BackendConfig{URL: "http://localhost:8001"})

	cb.RecordFailure(backend)
	if cb.GetState(backend) != StateClosed {
		t.Errorf("expected Closed after 1 failure (threshold 3), got %v", cb.GetState(backend))
	}
	if !backend.IsHealthy() {
		t.Error("backend should still be healthy after a single failure")
	}
}

// fakeClock is a controllable time source for deterministic rolling-window tests.
type fakeClock struct {
	t time.Time
}

func (f *fakeClock) now() time.Time { return f.t }
func (f *fakeClock) advance(d time.Duration) {
	f.t = f.t.Add(d)
}

func newRollingBreaker(clk *fakeClock, minReq int, rate float64, window, timeout time.Duration, successThreshold int) *CircuitBreaker {
	cb := NewCircuitBreakerWithConfig(Config{
		Mode:               ModeRolling,
		FailureThreshold:   1000, // irrelevant in rolling mode
		SuccessThreshold:   successThreshold,
		Timeout:            timeout,
		RollingWindow:      window,
		ErrorRateThreshold: rate,
		MinRequests:        minReq,
	})
	cb.setClock(clk.now)
	return cb
}

// A single failure must never trip a rolling breaker before MinRequests is met.
func TestRollingSingleFailureDoesNotTrip(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	cb := newRollingBreaker(clk, 20, 0.5, 10*time.Second, time.Second, 2)
	backend := balancer.NewBackend(config.BackendConfig{URL: "http://localhost:8001"})

	cb.RecordFailure(backend)
	if cb.GetState(backend) != StateClosed {
		t.Fatalf("expected Closed after 1 failure below MinRequests, got %v", cb.GetState(backend))
	}
}

// Below the error-rate threshold the breaker stays closed even past MinRequests.
func TestRollingStaysClosedBelowRate(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	cb := newRollingBreaker(clk, 20, 0.5, 10*time.Second, time.Second, 2)
	backend := balancer.NewBackend(config.BackendConfig{URL: "http://localhost:8001"})

	// 30 requests, 10 failures => rate 0.33 < 0.5. Spread across window.
	for i := 0; i < 30; i++ {
		if i%3 == 0 {
			cb.RecordFailure(backend)
		} else {
			cb.RecordSuccess(backend)
		}
		clk.advance(100 * time.Millisecond)
	}
	if cb.GetState(backend) != StateClosed {
		t.Fatalf("expected Closed below error-rate threshold, got %v", cb.GetState(backend))
	}
}

// Above MinRequests and above the rate threshold the breaker trips open.
func TestRollingTripsAboveRateAndMinRequests(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	cb := newRollingBreaker(clk, 20, 0.5, 10*time.Second, time.Second, 2)
	backend := balancer.NewBackend(config.BackendConfig{URL: "http://localhost:8001"})

	// 10 successes then failures. Stay closed until MinRequests and rate exceed.
	for i := 0; i < 10; i++ {
		cb.RecordSuccess(backend)
		clk.advance(50 * time.Millisecond)
	}
	if cb.GetState(backend) != StateClosed {
		t.Fatalf("expected Closed after 10 successes, got %v", cb.GetState(backend))
	}
	// Now 15 failures: total 25 >= 20, failures 15/25 = 0.6 > 0.5 => trip.
	tripped := false
	for i := 0; i < 15; i++ {
		cb.RecordFailure(backend)
		clk.advance(50 * time.Millisecond)
		if cb.GetState(backend) == StateOpen {
			tripped = true
			break
		}
	}
	if !tripped {
		t.Fatalf("expected rolling breaker to trip Open, state=%v", cb.GetState(backend))
	}
	if backend.IsHealthy() {
		t.Error("backend should be unhealthy after rolling trip")
	}
}

// An old burst of failures must age out of the window so it no longer keeps the
// circuit tripping once clean traffic dominates.
func TestRollingWindowAgesOutOldFailures(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	cb := newRollingBreaker(clk, 20, 0.5, 10*time.Second, time.Second, 2)
	backend := balancer.NewBackend(config.BackendConfig{URL: "http://localhost:8001"})

	// Old burst: many failures, but not enough total requests to trip yet.
	for i := 0; i < 15; i++ {
		cb.RecordFailure(backend)
		clk.advance(100 * time.Millisecond)
	}
	if cb.GetState(backend) != StateClosed {
		t.Fatalf("precondition: expected Closed (15 < MinRequests 20), got %v", cb.GetState(backend))
	}

	// Advance well past the window so that old burst ages out entirely.
	clk.advance(20 * time.Second)

	// Now 25 clean successes. Old failures are gone => still Closed.
	for i := 0; i < 25; i++ {
		cb.RecordSuccess(backend)
		clk.advance(50 * time.Millisecond)
	}
	if cb.GetState(backend) != StateClosed {
		t.Fatalf("expected Closed after old failures aged out, got %v", cb.GetState(backend))
	}
}

// The state-change hook must fire across the full closed->open->half-open->closed
// lifecycle (using the simpler consecutive mode to drive transitions).
func TestStateChangeHookFullLifecycle(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	cb := NewCircuitBreakerWithConfig(Config{
		Mode:             ModeConsecutive,
		FailureThreshold: 3,
		SuccessThreshold: 2,
		Timeout:          time.Second,
	})
	cb.setClock(clk.now)

	type tr struct{ from, to State }
	var transitions []tr
	var mu sync.Mutex
	cb.SetOnStateChange(func(_ *balancer.Backend, from, to State) {
		mu.Lock()
		transitions = append(transitions, tr{from, to})
		mu.Unlock()
	})

	backend := balancer.NewBackend(config.BackendConfig{URL: "http://localhost:8001"})

	// closed -> open
	for i := 0; i < 3; i++ {
		cb.RecordFailure(backend)
	}
	// open -> half-open (after timeout, via Allow)
	clk.advance(2 * time.Second)
	if err := cb.Allow(backend); err != nil {
		t.Fatalf("expected admission into half-open, got %v", err)
	}
	// half-open -> closed (successThreshold successes)
	for i := 0; i < 2; i++ {
		cb.RecordSuccess(backend)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []tr{
		{StateClosed, StateOpen},
		{StateOpen, StateHalfOpen},
		{StateHalfOpen, StateClosed},
	}
	if len(transitions) != len(want) {
		t.Fatalf("expected %d transitions, got %d: %+v", len(want), len(transitions), transitions)
	}
	for i, w := range want {
		if transitions[i] != w {
			t.Errorf("transition %d: expected %v->%v, got %v->%v", i, w.from, w.to, transitions[i].from, transitions[i].to)
		}
	}
}

// The hook must also fire on a half-open probe failure re-opening the circuit.
func TestStateChangeHookHalfOpenReopen(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	cb := NewCircuitBreakerWithConfig(Config{
		Mode:             ModeConsecutive,
		FailureThreshold: 1,
		SuccessThreshold: 1,
		Timeout:          time.Second,
	})
	cb.setClock(clk.now)

	var lastFrom, lastTo State
	var fired int
	var mu sync.Mutex
	cb.SetOnStateChange(func(_ *balancer.Backend, from, to State) {
		mu.Lock()
		lastFrom, lastTo = from, to
		fired++
		mu.Unlock()
	})

	backend := balancer.NewBackend(config.BackendConfig{URL: "http://localhost:8001"})
	cb.RecordFailure(backend) // closed -> open
	clk.advance(2 * time.Second)
	if err := cb.Allow(backend); err != nil { // open -> half-open
		t.Fatalf("expected half-open admission, got %v", err)
	}
	cb.RecordFailure(backend) // half-open -> open

	mu.Lock()
	defer mu.Unlock()
	if fired != 3 {
		t.Fatalf("expected 3 transitions, got %d", fired)
	}
	if lastFrom != StateHalfOpen || lastTo != StateOpen {
		t.Errorf("expected last transition half-open->open, got %v->%v", lastFrom, lastTo)
	}
}
