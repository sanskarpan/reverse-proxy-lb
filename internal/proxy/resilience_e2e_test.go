package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/circuit"
	"reverse-proxy-lb/internal/config"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file drives the §3 resilience features end-to-end through a REAL network
// stack: real httptest backend servers, the proxy fronted by a real
// httptest.NewServer listener, and requests issued with a real *http.Client over
// TCP. Nothing is stubbed on the request path; each scenario exercises the
// production wiring the server package uses (circuit breaker, per-try timeout,
// retry budget, hedging, bulkhead) and asserts on the proxy's own counters,
// circuit-breaker state, and backend hit counts.
//
// Scenarios:
//   - Rolling-mode circuit: high error rate over the window trips once
//     MinRequests is reached; a low error rate does NOT trip.
//   - Per-try timeout: a stalling backend is abandoned and the request still
//     succeeds via failover to a healthy backend.
//   - Retry budget: under a dead backend the number of retries stays bounded by
//     the configured Budget.
//   - Hedged requests: a slow+fast pair with hedging on for a GET returns the
//     FAST backend's body, written exactly once (run under -race).
//   - Bulkhead: MaxConns=1 with concurrent in-flight requests yields 503s for the
//     excess and increments the rejection counter.

// ----------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------

// hitBackend is a real httptest backend that counts hits and can be driven into
// error/slow/stall behavior. handler, when set, fully owns the response.
func newHitBackend(handler http.HandlerFunc) (*httptest.Server, *int64) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		if handler != nil {
			handler(w, r)
			return
		}
		io.WriteString(w, "ok")
	}))
	return srv, &hits
}

// frontFor stands the proxy up behind a real listener and returns the front URL
// plus a client with a bounded timeout so a hung test fails fast.
func frontFor(t *testing.T, p *Proxy) (string, *http.Client, func()) {
	t.Helper()
	front := httptest.NewServer(p)
	client := &http.Client{Timeout: 10 * time.Second}
	return front.URL, client, front.Close
}

// eventually polls cond until it is true or the deadline elapses.
func eventually(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// ----------------------------------------------------------------------------
// Rolling-mode circuit breaker
// ----------------------------------------------------------------------------

// A backend whose error rate over the rolling window exceeds the threshold once
// MinRequests is reached must trip (go unhealthy / stop receiving traffic). The
// proxy is configured to trip on 5xx so the deterministic error responses drive
// the window without needing transport failures.
func TestE2E_RollingCircuit_HighErrorRateTrips(t *testing.T) {
	// Backend always returns 500 -> 100% error rate over the window.
	bad, badHits := newHitBackend(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	defer bad.Close()

	rr := balancer.NewRoundRobin()
	be := balancer.NewBackend(config.BackendConfig{URL: bad.URL, MaxConns: 100})
	rr.Add(be)

	cb := circuit.NewCircuitBreakerWithConfig(circuit.Config{
		Mode:               circuit.ModeRolling,
		SuccessThreshold:   2,
		Timeout:            30 * time.Second,
		RollingWindow:      2 * time.Second,
		ErrorRateThreshold: 0.5,
		MinRequests:        10,
	})
	p := New(rr, cb, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})
	p.SetTripOn([]string{"connect", "timeout", "5xx"})

	front, client, closeFront := frontFor(t, p)
	defer closeFront()

	// Drive enough requests to satisfy MinRequests and trip on error rate.
	for i := 0; i < 30; i++ {
		resp, err := client.Get(front + "/")
		if err != nil {
			t.Fatalf("request %d error: %v", i, err)
		}
		resp.Body.Close()
		if cb.GetState(be) == circuit.StateOpen {
			break
		}
	}

	if !eventually(t, time.Second, func() bool { return cb.GetState(be) == circuit.StateOpen }) {
		t.Fatalf("rolling circuit did not trip on a 100%% error rate past MinRequests (state=%v)", cb.GetState(be))
	}
	if be.IsHealthy() {
		t.Error("tripped backend must be marked unhealthy (stops receiving traffic)")
	}

	// Once open, further requests must be rejected without reaching the backend.
	hitsAtTrip := atomic.LoadInt64(badHits)
	for i := 0; i < 10; i++ {
		resp, err := client.Get(front + "/")
		if err != nil {
			t.Fatalf("post-trip request %d error: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("post-trip request %d: expected 503 (no healthy backend), got %d", i, resp.StatusCode)
		}
	}
	if got := atomic.LoadInt64(badHits) - hitsAtTrip; got != 0 {
		t.Errorf("tripped backend still received %d requests after opening", got)
	}
}

// A backend with a LOW error rate (below the threshold) must NOT trip even after
// many requests have flowed through the rolling window.
func TestE2E_RollingCircuit_LowErrorRateDoesNotTrip(t *testing.T) {
	// 1 in 5 requests errors -> 20% error rate, below the 50% threshold.
	var n int64
	backend, _ := newHitBackend(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&n, 1)%5 == 0 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		io.WriteString(w, "ok")
	})
	defer backend.Close()

	rr := balancer.NewRoundRobin()
	be := balancer.NewBackend(config.BackendConfig{URL: backend.URL, MaxConns: 100})
	rr.Add(be)

	cb := circuit.NewCircuitBreakerWithConfig(circuit.Config{
		Mode:               circuit.ModeRolling,
		SuccessThreshold:   2,
		Timeout:            30 * time.Second,
		RollingWindow:      5 * time.Second,
		ErrorRateThreshold: 0.5,
		MinRequests:        10,
	})
	p := New(rr, cb, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})
	p.SetTripOn([]string{"connect", "timeout", "5xx"})

	front, client, closeFront := frontFor(t, p)
	defer closeFront()

	for i := 0; i < 100; i++ {
		resp, err := client.Get(front + "/")
		if err != nil {
			t.Fatalf("request %d error: %v", i, err)
		}
		resp.Body.Close()
		if cb.GetState(be) != circuit.StateClosed {
			t.Fatalf("request %d: circuit tripped at a 20%% error rate (state=%v); must stay closed below threshold",
				i, cb.GetState(be))
		}
	}
	if !be.IsHealthy() {
		t.Error("backend below the error-rate threshold must remain healthy")
	}
}

// ----------------------------------------------------------------------------
// Per-try timeout + failover
// ----------------------------------------------------------------------------

// A backend that stalls beyond PerTryTimeout must be abandoned, and because a
// second healthy backend exists, the request must still succeed via failover.
func TestE2E_PerTryTimeout_AbandonsAndFailsOver(t *testing.T) {
	stallReleased := make(chan struct{})
	var once sync.Once
	stall, stallHits := newHitBackend(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done(): // per-try timeout cancels the attempt
		case <-time.After(5 * time.Second):
		}
		once.Do(func() { close(stallReleased) })
	})
	defer stall.Close()

	fast, fastHits := newHitBackend(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "fast-ok")
	})
	defer fast.Close()

	rr := balancer.NewRoundRobin()
	// Stall backend is added first so round-robin makes it the primary.
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: stall.URL, MaxConns: 100}))
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: fast.URL, MaxConns: 100}))

	p := New(rr, nil, config.RetryConfig{PerTryTimeout: 150 * time.Millisecond}, "round_robin", nil, nil, config.UpstreamConfig{})

	front, client, closeFront := frontFor(t, p)
	defer closeFront()

	start := time.Now()
	resp, err := client.Get(front + "/")
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 via failover after per-try timeout, got %d", resp.StatusCode)
	}
	if string(body) != "fast-ok" {
		t.Errorf("expected fast backend body, got %q", body)
	}
	if elapsed > 3*time.Second {
		t.Errorf("per-try timeout did not abandon the stalling backend promptly: took %v", elapsed)
	}
	if atomic.LoadInt64(stallHits) < 1 {
		t.Error("expected the stall backend to have been attempted first")
	}
	if atomic.LoadInt64(fastHits) < 1 {
		t.Error("expected failover to reach the fast backend")
	}
	// The stalling backend should observe cancellation from the per-try timeout.
	if !eventually(t, 2*time.Second, func() bool {
		select {
		case <-stallReleased:
			return true
		default:
			return false
		}
	}) {
		t.Error("stall backend did not observe per-try context cancellation")
	}
}

// ----------------------------------------------------------------------------
// Retry budget
// ----------------------------------------------------------------------------

// Under a dead backend, a tiny Budget must cap the total number of retries: the
// floor allows a handful, but the running retries/requests ratio then blocks the
// rest, so retries stay well below the unbounded MaxAttempts*requests ceiling and
// the budget-denied counter increments.
func TestE2E_RetryBudget_CapsRetries(t *testing.T) {
	rr := balancer.NewRoundRobin()
	// 127.0.0.1:1 refuses connections -> every attempt is a connect error retry.
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:1", MaxConns: 100}))

	const maxAttempts = 40
	retry := config.RetryConfig{MaxAttempts: maxAttempts, MaxBackoff: time.Millisecond, Budget: 0.05}
	p := New(rr, nil, retry, "round_robin", nil, nil, config.UpstreamConfig{})

	front, client, closeFront := frontFor(t, p)
	defer closeFront()

	const reqs = 8
	for i := 0; i < reqs; i++ {
		resp, err := client.Get(front + "/")
		if err != nil {
			t.Fatalf("request %d error: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("request %d: expected 502 from a dead backend, got %d", i, resp.StatusCode)
		}
	}

	retries := p.GetMetrics().GetPrometheusMetrics().TotalRetries
	// Without a budget the ceiling would be reqs*maxAttempts = 8*40 = 320 retries.
	// The budget must keep the count far below that. The floor (retryBudgetFloor)
	// permits a constant burst, after which the ratio gate applies.
	unbounded := reqs * maxAttempts
	if int(retries) >= unbounded {
		t.Errorf("retry budget did not cap retries: got %v, unbounded ceiling is %d", retries, unbounded)
	}
	// Concretely: floor is retryBudgetFloor and the ratio bound is Budget; the cap
	// is comfortably under half the unbounded ceiling.
	if int(retries) > unbounded/2 {
		t.Errorf("retry budget too loose: got %v retries, expected << %d", retries, unbounded/2)
	}
	if p.GetBudgetDenied() == 0 {
		t.Errorf("expected the retry budget to deny some retries (retries=%v), got 0 denials", retries)
	}
}

// ----------------------------------------------------------------------------
// Hedged requests (run under -race for double-write/reservation bugs)
// ----------------------------------------------------------------------------

// With a slow primary and a fast secondary and hedging enabled for a GET, the
// client must receive the FAST backend's response, exactly one response is
// written to the client, and the hedge-win counter increments. Reservations must
// all be released. Running this under -race catches double-write and
// double-release bugs.
func TestE2E_Hedging_ReturnsFastAndWritesOnce(t *testing.T) {
	slow, _ := newHitBackend(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
		}
		io.WriteString(w, "slow")
	})
	defer slow.Close()
	fast, _ := newHitBackend(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "fast")
	})
	defer fast.Close()

	rr := balancer.NewRoundRobin()
	slowBE := balancer.NewBackend(config.BackendConfig{URL: slow.URL, MaxConns: 100})
	fastBE := balancer.NewBackend(config.BackendConfig{URL: fast.URL, MaxConns: 100})
	// Slow first so it is the primary; the hedge races the fast one.
	rr.Add(slowBE)
	rr.Add(fastBE)

	retry := config.RetryConfig{
		Hedge: config.HedgeConfig{Enabled: true, Delay: 50 * time.Millisecond, MaxExtra: 1},
	}
	p := New(rr, nil, retry, "round_robin", nil, nil, config.UpstreamConfig{})

	front, client, closeFront := frontFor(t, p)
	defer closeFront()

	start := time.Now()
	resp, err := client.Get(front + "/")
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK || string(body) != "fast" {
		t.Fatalf("expected fast backend response, got %d/%q", resp.StatusCode, body)
	}
	// Content-Length must reflect a single 4-byte body ("fast"), i.e. exactly one
	// response was written to the client (no double-write concatenation).
	if resp.ContentLength != -1 && resp.ContentLength != int64(len("fast")) {
		t.Errorf("expected a single 'fast' body written once, got Content-Length=%d", resp.ContentLength)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("hedging did not race the fast backend: took %v", elapsed)
	}
	if p.GetHedgeWins() != 1 {
		t.Errorf("expected exactly one hedge win, got %d", p.GetHedgeWins())
	}
	if p.GetHedgedCount() < 1 {
		t.Errorf("expected at least one hedge attempt launched, got %d", p.GetHedgedCount())
	}
	// All reservations must drain (each attempt releases its own).
	if !eventually(t, 2*time.Second, func() bool {
		return slowBE.GetActiveConns() == 0 && fastBE.GetActiveConns() == 0
	}) {
		t.Errorf("reservation leaked: slow=%d fast=%d", slowBE.GetActiveConns(), fastBE.GetActiveConns())
	}
}

// ----------------------------------------------------------------------------
// Bulkhead
// ----------------------------------------------------------------------------

// With MaxConns=1 and multiple concurrent in-flight requests, only one may be
// admitted to the single backend at a time; the excess must be rejected with 503
// (bulkhead) rather than piling onto the backend, and the rejection counter must
// increment. A single-backend topology guarantees a capacity-blocked candidate
// yields 503 (not a failover elsewhere).
func TestE2E_Bulkhead_ExcessGets503(t *testing.T) {
	// The backend blocks until released so concurrent requests stay in-flight,
	// forcing the connection cap to bind.
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer closeRelease()

	var maxObservedInFlight int64
	var inFlight int64
	backend, _ := newHitBackend(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt64(&inFlight, 1)
		for {
			prev := atomic.LoadInt64(&maxObservedInFlight)
			if cur <= prev || atomic.CompareAndSwapInt64(&maxObservedInFlight, prev, cur) {
				break
			}
		}
		<-release
		atomic.AddInt64(&inFlight, -1)
		io.WriteString(w, "ok")
	})
	defer backend.Close()

	rr := balancer.NewRoundRobin()
	be := balancer.NewBackend(config.BackendConfig{URL: backend.URL, MaxConns: 1})
	rr.Add(be)
	p := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})

	front, client, closeFront := frontFor(t, p)
	defer closeFront()

	const concurrent = 8
	var wg sync.WaitGroup
	codes := make([]int, concurrent)
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			resp, err := client.Get(front + "/")
			if err != nil {
				codes[idx] = -1
				return
			}
			codes[idx] = resp.StatusCode
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}(i)
	}

	// Wait until the excess requests have been rejected (503) while one is parked
	// in the backend holding the single slot.
	got503 := eventually(t, 3*time.Second, func() bool {
		return p.GetRejections() >= 1
	})
	if !got503 {
		closeRelease()
		wg.Wait()
		t.Fatalf("bulkhead did not reject any excess request (rejections=%d)", p.GetRejections())
	}

	// Release the parked request(s) and let everything finish.
	closeRelease()
	wg.Wait()

	var ok, rejected int
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusServiceUnavailable:
			rejected++
		}
	}
	if rejected == 0 {
		t.Errorf("expected some concurrent requests to be rejected with 503, got codes=%v", codes)
	}
	if ok == 0 {
		t.Errorf("expected at least one request to succeed, got codes=%v", codes)
	}
	if p.GetRejections() == 0 {
		t.Error("expected the bulkhead rejection counter to increment")
	}
	// The backend must never have seen more than MaxConns=1 concurrent request.
	if m := atomic.LoadInt64(&maxObservedInFlight); m > 1 {
		t.Errorf("bulkhead breached: backend saw %d concurrent in-flight requests, cap is 1", m)
	}
}
