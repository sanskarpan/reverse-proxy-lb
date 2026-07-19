package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/circuit"
	"reverse-proxy-lb/internal/config"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// captureWriter must forward Flush/Hijack so streaming/SSE and WebSocket upgrades keep
// working through the proxy. This compile-time check fails if the methods are removed.
var (
	_ http.Flusher  = (*captureWriter)(nil)
	_ http.Hijacker = (*captureWriter)(nil)
)

// verify captureWriter delegates Hijack to the underlying writer.
type hijackableRW struct {
	http.ResponseWriter
	hijacked bool
}

func (h *hijackableRW) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}

func TestCaptureWriterDelegatesHijack(t *testing.T) {
	base := &hijackableRW{ResponseWriter: httptest.NewRecorder()}
	cw := &captureWriter{ResponseWriter: base}
	if _, _, err := cw.Hijack(); err != nil {
		t.Fatalf("unexpected hijack error: %v", err)
	}
	if !base.hijacked {
		t.Error("Hijack was not delegated to the underlying ResponseWriter")
	}
	if !cw.wrote {
		t.Error("Hijack should mark the response as started")
	}
}

// ID 1: a dead backend must actually retry and record circuit-breaker failures,
// instead of being silently recorded as a success (the original bug).
func TestDeadBackendRetriesAndTripsCircuit(t *testing.T) {
	rr := balancer.NewRoundRobin()
	be := balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:1", MaxConns: 100})
	rr.Add(be)

	cb := circuit.NewCircuitBreaker(1, 2, 30*time.Second) // opens after 1 failure
	retry := config.RetryConfig{MaxAttempts: 2, MaxBackoff: 1 * time.Millisecond}
	p := New(rr, cb, retry, "round_robin", nil, nil, config.UpstreamConfig{})

	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	m := p.GetMetrics().GetPrometheusMetrics()
	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", rec.Code)
	}
	if m.TotalRetries != 2 {
		t.Errorf("expected 2 retries on a dead backend, got %v", m.TotalRetries)
	}
	if m.TotalErrors < 1 {
		t.Errorf("expected the failed request to be counted as an error, got %v", m.TotalErrors)
	}
	if cb.GetState(be) != circuit.StateOpen {
		t.Errorf("expected circuit to be Open after failures, got %v", cb.GetState(be))
	}
	if be.IsHealthy() {
		t.Error("expected backend marked unhealthy after circuit opened")
	}
	if got := be.GetActiveConns(); got != 0 {
		t.Errorf("connection reservation leaked: ActiveConns=%d, want 0", got)
	}
}

// ID 12 + ID 1: when the primary backend is down but a healthy one exists, the
// request must fail over to the healthy backend and succeed.
func TestFailoverToHealthyBackend(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello-from-live")
	}))
	defer live.Close()

	rr := balancer.NewRoundRobin()
	dead := balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:1", MaxConns: 100})
	good := balancer.NewBackend(config.BackendConfig{URL: live.URL, MaxConns: 100})
	rr.Add(dead)
	rr.Add(good)

	retry := config.RetryConfig{MaxAttempts: 0}
	p := New(rr, nil, retry, "round_robin", nil, nil, config.UpstreamConfig{})

	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 via failover, got %d", rec.Code)
	}
	if body := rec.Body.String(); body != "hello-from-live" {
		t.Errorf("expected live backend body, got %q", body)
	}
	if dead.GetActiveConns() != 0 || good.GetActiveConns() != 0 {
		t.Errorf("reservations leaked: dead=%d good=%d", dead.GetActiveConns(), good.GetActiveConns())
	}
}

// ID 9: Proxy.ServeHTTP must not itself count requests (the middleware does that);
// otherwise every accepted request is double counted.
func TestProxyDoesNotDoubleCountRequests(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer live.Close()

	rr := balancer.NewRoundRobin()
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: live.URL, MaxConns: 100}))
	p := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})

	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	p.ServeHTTP(httptest.NewRecorder(), req)

	if got := p.GetMetrics().GetPrometheusMetrics().TotalRequests; got != 0 {
		t.Errorf("proxy should not increment TotalRequests, got %v", got)
	}
}

// ID 11: the X-Forwarded-For chain must be appended to (not overwritten), and
// X-Real-IP set to the resolved client IP.
func TestForwardedHeaders(t *testing.T) {
	var gotXFF, gotReal string
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
		gotReal = r.Header.Get("X-Real-IP")
	}))
	defer live.Close()

	rr := balancer.NewRoundRobin()
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: live.URL, MaxConns: 100}))
	p := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})

	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.Header.Set("X-Forwarded-For", "1.1.1.1")
	req.RemoteAddr = "2.2.2.2:1234"
	p.ServeHTTP(httptest.NewRecorder(), req)

	if !strings.Contains(gotXFF, "1.1.1.1") || !strings.Contains(gotXFF, "2.2.2.2") {
		t.Errorf("expected XFF chain to include 1.1.1.1 and 2.2.2.2, got %q", gotXFF)
	}
	if gotReal != "2.2.2.2" {
		t.Errorf("expected X-Real-IP 2.2.2.2 (untrusted peer), got %q", gotReal)
	}
}

// Streaming/SSE responses must pass through the proxy intact (regression guard for the
// captureWriter Flush forwarding). Uses a real front listener so the ResponseWriter
// handed to captureWriter is a genuine flushable/hijackable writer.
func TestStreamingResponsePassesThrough(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("backend ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 3; i++ {
			io.WriteString(w, "data: chunk\n\n")
			fl.Flush()
		}
	}))
	defer backend.Close()

	rr := balancer.NewRoundRobin()
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: backend.URL, MaxConns: 100}))
	p := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})

	front := httptest.NewServer(p)
	defer front.Close()

	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if got := strings.Count(string(body), "data: chunk"); got != 3 {
		t.Errorf("expected 3 streamed chunks through the proxy, got %d (body=%q)", got, body)
	}
}

// SPEC §10: the proxy must be able to talk to https:// backends, verifying the
// backend certificate against a configured CA — and must FAIL closed when the cert
// is untrusted (no silent InsecureSkipVerify default).
func TestBackendTLS(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "secure-ok")
	}))
	defer backend.Close()

	newProxy := func(tlsCfg *tls.Config) *Proxy {
		rr := balancer.NewRoundRobin()
		rr.Add(balancer.NewBackend(config.BackendConfig{URL: backend.URL, MaxConns: 100}))
		return New(rr, nil, config.RetryConfig{}, "round_robin", nil, tlsCfg, config.UpstreamConfig{})
	}
	do := func(p *Proxy) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
		req.RemoteAddr = "203.0.113.9:5555"
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	// 1) Trust the backend's cert via a CA pool -> success.
	pool := x509.NewCertPool()
	pool.AddCert(backend.Certificate())
	if rec := do(newProxy(&tls.Config{RootCAs: pool})); rec.Code != http.StatusOK || rec.Body.String() != "secure-ok" {
		t.Errorf("trusted CA: expected 200/secure-ok, got %d/%q", rec.Code, rec.Body.String())
	}

	// 2) InsecureSkipVerify -> success (opt-in escape hatch).
	if rec := do(newProxy(&tls.Config{InsecureSkipVerify: true})); rec.Code != http.StatusOK {
		t.Errorf("insecure: expected 200, got %d", rec.Code)
	}

	// 3) Default verification (nil config) against a self-signed cert -> MUST fail.
	if rec := do(newProxy(nil)); rec.Code == http.StatusOK {
		t.Error("default verification should reject an untrusted self-signed backend cert")
	}
}

// FIX 2: a non-idempotent request (POST) that fails on the primary backend with a
// pure connection-establishment error must still fail over to a healthy backend and
// succeed, because a connect error means the dead backend never processed the request.
func TestPostFailsOverOnConnectError(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "post-ok")
	}))
	defer live.Close()

	rr := balancer.NewRoundRobin()
	dead := balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:1", MaxConns: 100})
	good := balancer.NewBackend(config.BackendConfig{URL: live.URL, MaxConns: 100})
	rr.Add(dead)
	rr.Add(good)

	retry := config.RetryConfig{MaxAttempts: 0}
	p := New(rr, nil, retry, "round_robin", nil, nil, config.UpstreamConfig{})

	req := httptest.NewRequest(http.MethodPost, "http://proxy/", strings.NewReader("payload"))
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 via connect-error failover for POST, got %d", rec.Code)
	}
	if body := rec.Body.String(); body != "post-ok" {
		t.Errorf("expected live backend body, got %q", body)
	}
	if dead.GetActiveConns() != 0 || good.GetActiveConns() != 0 {
		t.Errorf("reservations leaked: dead=%d good=%d", dead.GetActiveConns(), good.GetActiveConns())
	}
}

// FIX 2: same-backend retries must only happen for idempotent requests. A GET to a
// dead backend should retry MaxAttempts times; a POST to the same dead backend must
// make a single attempt (zero retries) to avoid double-apply.
func TestSameBackendRetryOnlyForIdempotent(t *testing.T) {
	run := func(method string) float64 {
		rr := balancer.NewRoundRobin()
		be := balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:1", MaxConns: 100})
		rr.Add(be)

		retry := config.RetryConfig{MaxAttempts: 3, MaxBackoff: 1 * time.Millisecond}
		p := New(rr, nil, retry, "round_robin", nil, nil, config.UpstreamConfig{})

		var body io.Reader
		if method == http.MethodPost {
			body = strings.NewReader("payload")
		}
		req := httptest.NewRequest(method, "http://proxy/", body)
		req.RemoteAddr = "203.0.113.9:5555"
		p.ServeHTTP(httptest.NewRecorder(), req)

		return p.GetMetrics().GetPrometheusMetrics().TotalRetries
	}

	if got := run(http.MethodPost); got != 0 {
		t.Errorf("POST must not do same-backend retries, got %v retries", got)
	}
	if got := run(http.MethodGet); got != 3 {
		t.Errorf("GET must retry MaxAttempts times, got %v retries", got)
	}
}

// FIX 3: the shared upstream transport must carry the granular timeouts so a slow
// backend cannot pin front-end goroutines/connections indefinitely.
func TestTransportHasGranularTimeouts(t *testing.T) {
	rr := balancer.NewRoundRobin()
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:1", MaxConns: 100}))
	p := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})

	tr, ok := p.transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", p.transport)
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Error("expected non-zero ResponseHeaderTimeout")
	}
	if tr.TLSHandshakeTimeout == 0 {
		t.Error("expected non-zero TLSHandshakeTimeout")
	}
	if tr.ExpectContinueTimeout == 0 {
		t.Error("expected non-zero ExpectContinueTimeout")
	}
	if tr.DialContext == nil {
		t.Error("expected DialContext to be set with a bounded dial timeout")
	}
}

// fakeBalancer records which selection path the proxy used and captures the
// latency/outcome feedback it received. It implements Balancer plus the optional
// KeyedBalancer, LatencyObserver and OutcomeObserver capabilities so the proxy's
// type-assertion wiring can be asserted.
type fakeBalancer struct {
	backend *balancer.Backend

	nextForKeyCalls []string
	nextCalls       int
	latencyCalls    int
	lastLatency     time.Duration
	outcomeCalls    int
	lastOutcome     bool
}

func (f *fakeBalancer) Next() (*balancer.Backend, error) {
	f.nextCalls++
	f.backend.IncrConn()
	return f.backend, nil
}

func (f *fakeBalancer) NextForKey(key string) (*balancer.Backend, error) {
	f.nextForKeyCalls = append(f.nextForKeyCalls, key)
	f.backend.IncrConn()
	return f.backend, nil
}

func (f *fakeBalancer) Add(*balancer.Backend)    {}
func (f *fakeBalancer) Remove(*balancer.Backend) {}
func (f *fakeBalancer) All() []*balancer.Backend { return []*balancer.Backend{f.backend} }
func (f *fakeBalancer) GetHealthy() []*balancer.Backend {
	if f.backend.IsHealthy() {
		return []*balancer.Backend{f.backend}
	}
	return nil
}
func (f *fakeBalancer) UpdateWeight(*balancer.Backend, int) {}

func (f *fakeBalancer) ObserveLatency(b *balancer.Backend, d time.Duration) {
	f.latencyCalls++
	f.lastLatency = d
}

func (f *fakeBalancer) ObserveOutcome(b *balancer.Backend, ok bool) {
	f.outcomeCalls++
	f.lastOutcome = ok
}

// The proxy must route through KeyedBalancer.NextForKey when the balancer
// implements it (consistent_hash, ip_hash), passing the client IP as the key.
func TestKeyedBalancerRoutesViaNextForKey(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "keyed-ok")
	}))
	defer live.Close()

	fb := &fakeBalancer{backend: balancer.NewBackend(config.BackendConfig{URL: live.URL, MaxConns: 100})}
	p := New(fb, nil, config.RetryConfig{}, "consistent_hash", nil, nil, config.UpstreamConfig{})

	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if fb.nextCalls != 0 {
		t.Errorf("expected Next() not to be called for a KeyedBalancer, got %d", fb.nextCalls)
	}
	if len(fb.nextForKeyCalls) != 1 || fb.nextForKeyCalls[0] != "203.0.113.9" {
		t.Errorf("expected NextForKey called with client IP, got %v", fb.nextForKeyCalls)
	}
	if fb.backend.GetActiveConns() != 0 {
		t.Errorf("reservation leaked: ActiveConns=%d", fb.backend.GetActiveConns())
	}
}

// A real consistent-hash balancer must route the same client IP to the same
// backend across requests via the NextForKey path.
func TestConsistentHashStableRouting(t *testing.T) {
	var hits [2]int
	mk := func(idx int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[idx]++
			io.WriteString(w, "ok")
		}))
	}
	s0, s1 := mk(0), mk(1)
	defer s0.Close()
	defer s1.Close()

	ch := balancer.NewConsistentHash(100, 1.25)
	ch.Add(balancer.NewBackend(config.BackendConfig{URL: s0.URL, MaxConns: 100}))
	ch.Add(balancer.NewBackend(config.BackendConfig{URL: s1.URL, MaxConns: 100}))
	p := New(ch, nil, config.RetryConfig{}, "consistent_hash", nil, nil, config.UpstreamConfig{})

	const n = 6
	for i := 0; i < n; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
		req.RemoteAddr = "198.51.100.7:4444"
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, rec.Code)
		}
	}
	// All n requests from one IP must have landed on exactly one backend.
	if hits[0] != 0 && hits[1] != 0 {
		t.Errorf("consistent hash split one client across backends: %v", hits)
	}
	if hits[0]+hits[1] != n {
		t.Errorf("expected %d total hits, got %v", n, hits)
	}
}

// With sticky sessions enabled, the first response sets the affinity cookie and a
// subsequent request carrying that cookie must pin to the same backend even when
// the balancer would otherwise round-robin to a different one.
func TestStickyCookiePinsClient(t *testing.T) {
	var hitsA, hitsB int
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hitsA++ }))
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hitsB++ }))
	defer a.Close()
	defer b.Close()

	rr := balancer.NewRoundRobin()
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: a.URL, MaxConns: 100}))
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: b.URL, MaxConns: 100}))
	p := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})
	p.SetSticky(config.StickyConfig{Enabled: true, Cookie: "rplb_affinity"})

	// First request: no cookie -> balancer picks; response sets the cookie.
	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	var affinity *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "rplb_affinity" {
			affinity = c
		}
	}
	if affinity == nil || affinity.Value == "" {
		t.Fatal("expected an affinity cookie to be set on the first response")
	}
	firstA, firstB := hitsA, hitsB

	// Follow-up requests carrying the cookie must all pin to the same backend
	// that served the first request, regardless of round-robin ordering.
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
		req.RemoteAddr = "203.0.113.9:5555"
		req.AddCookie(affinity)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("pinned request %d: expected 200, got %d", i, rec.Code)
		}
	}

	// Exactly one backend should have absorbed all 5 follow-ups.
	deltaA, deltaB := hitsA-firstA, hitsB-firstB
	if !((deltaA == 5 && deltaB == 0) || (deltaB == 5 && deltaA == 0)) {
		t.Errorf("sticky cookie did not pin client: deltaA=%d deltaB=%d", deltaA, deltaB)
	}
}

// The proxy must feed a LatencyObserver on success and an OutcomeObserver after
// each attempt.
func TestObserversAreInvoked(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer live.Close()

	fb := &fakeBalancer{backend: balancer.NewBackend(config.BackendConfig{URL: live.URL, MaxConns: 100})}
	p := New(fb, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})

	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	p.ServeHTTP(httptest.NewRecorder(), req)

	if fb.latencyCalls != 1 {
		t.Errorf("expected ObserveLatency called once on success, got %d", fb.latencyCalls)
	}
	if fb.lastLatency < 0 {
		t.Errorf("expected non-negative observed latency, got %v", fb.lastLatency)
	}
	if fb.outcomeCalls != 1 || !fb.lastOutcome {
		t.Errorf("expected ObserveOutcome(true) once, got calls=%d ok=%v", fb.outcomeCalls, fb.lastOutcome)
	}
}

// On a failing upstream, the OutcomeObserver must be told ok=false and the
// LatencyObserver must NOT be fed (a failed attempt should not poison latency).
func TestOutcomeObserverOnFailure(t *testing.T) {
	fb := &fakeBalancer{backend: balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:1", MaxConns: 100})}
	p := New(fb, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})

	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if fb.outcomeCalls != 1 || fb.lastOutcome {
		t.Errorf("expected ObserveOutcome(false) once, got calls=%d ok=%v", fb.outcomeCalls, fb.lastOutcome)
	}
	if fb.latencyCalls != 0 {
		t.Errorf("expected ObserveLatency not called on failure, got %d", fb.latencyCalls)
	}
}

// ID 2: connection pooling — repeated requests must reuse the same cached
// ReverseProxy instance per backend rather than allocating one per request.
func TestReverseProxyIsCachedPerBackend(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer live.Close()

	be := balancer.NewBackend(config.BackendConfig{URL: live.URL, MaxConns: 100})
	rr := balancer.NewRoundRobin()
	rr.Add(be)
	p := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})

	rp1, err := p.proxyFor(be)
	if err != nil {
		t.Fatal(err)
	}
	rp2, err := p.proxyFor(be)
	if err != nil {
		t.Fatal(err)
	}
	if rp1 != rp2 {
		t.Error("expected the per-backend ReverseProxy to be cached and reused")
	}
	// §5.2: each per-backend ReverseProxy now owns its OWN transport (so pool limits
	// apply per backend), so it must be a real *http.Transport and NOT the shared
	// representative p.transport.
	if _, ok := rp1.Transport.(*http.Transport); !ok {
		t.Errorf("expected a per-backend *http.Transport, got %T", rp1.Transport)
	}
	if rp1.Transport == p.transport {
		t.Error("expected each backend to own its own transport, not the shared p.transport")
	}
}

// §3.1 Failure classification: a backend that returns 5xx must be counted as a
// circuit failure ONLY when "5xx" is in the trip set. A 5xx response is written to
// the client (not retried).
func TestFiveXXTripsCircuitOnlyWhenConfigured(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer backend.Close()

	run := func(tripOn []string) (int, circuit.State, *balancer.Backend) {
		rr := balancer.NewRoundRobin()
		be := balancer.NewBackend(config.BackendConfig{URL: backend.URL, MaxConns: 100})
		rr.Add(be)
		cb := circuit.NewCircuitBreaker(1, 2, 30*time.Second) // opens after 1 failure
		p := New(rr, cb, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})
		p.SetTripOn(tripOn)

		req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
		req.RemoteAddr = "203.0.113.9:5555"
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec.Code, cb.GetState(be), be
	}

	// Default trip set {connect,timeout}: a 5xx is NOT a circuit failure.
	code, state, _ := run(nil)
	if code != http.StatusInternalServerError {
		t.Errorf("default: expected 500 passed through, got %d", code)
	}
	if state != circuit.StateClosed {
		t.Errorf("default: 5xx must not trip circuit, got %v", state)
	}

	// trip_on includes 5xx: a single 5xx opens the circuit.
	code, state, _ = run([]string{"connect", "timeout", "5xx"})
	if code != http.StatusInternalServerError {
		t.Errorf("5xx-trip: expected 500 passed through (never retried), got %d", code)
	}
	if state != circuit.StateOpen {
		t.Errorf("5xx-trip: expected circuit Open after a 5xx, got %v", state)
	}
}

// §3.5 Per-try timeout: a slow backend attempt must be abandoned once
// PerTryTimeout elapses and classified as a timeout (surfacing 502 with no
// failover backend).
func TestPerTryTimeoutAbandonsSlowBackend(t *testing.T) {
	released := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done(): // client/per-try cancelled
		case <-time.After(5 * time.Second):
		}
		close(released)
	}))
	defer slow.Close()

	rr := balancer.NewRoundRobin()
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: slow.URL, MaxConns: 100}))
	// No retries; per-try timeout bounds the single attempt.
	p := New(rr, nil, config.RetryConfig{PerTryTimeout: 100 * time.Millisecond}, "round_robin", nil, nil, config.UpstreamConfig{})

	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()

	start := time.Now()
	p.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("per-try timeout did not abandon the slow backend: took %v", elapsed)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502 after per-try timeout, got %d", rec.Code)
	}
	// Ensure the backend goroutine observed cancellation (context propagated).
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Error("backend did not observe context cancellation")
	}
}

// §3.4 Full-jitter backoff: every computed backoff must stay within
// [0, min(cap, base*2^(n-1))] and be capped by MaxBackoff. With a seeded rand it
// is deterministic and never exceeds the cap.
func TestFullJitterBackoffStaysWithinCap(t *testing.T) {
	rr := balancer.NewRoundRobin()
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:1", MaxConns: 100}))
	cap := 250 * time.Millisecond
	p := New(rr, nil, config.RetryConfig{MaxBackoff: cap}, "round_robin", nil, nil, config.UpstreamConfig{})
	p.SetRand(rand.New(rand.NewSource(42)))

	for attempt := 1; attempt <= 12; attempt++ {
		for i := 0; i < 200; i++ {
			d := p.calculateBackoff(attempt)
			if d < 0 {
				t.Fatalf("attempt %d: negative backoff %v", attempt, d)
			}
			if d > cap {
				t.Fatalf("attempt %d: backoff %v exceeded cap %v", attempt, d, cap)
			}
		}
	}

	// Uncapped: ceiling for attempt 1 is 1s, so values must be within [0,1s].
	p2 := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})
	p2.SetRand(rand.New(rand.NewSource(7)))
	for i := 0; i < 500; i++ {
		if d := p2.calculateBackoff(1); d < 0 || d > time.Second {
			t.Fatalf("attempt 1 uncapped: backoff %v out of [0,1s]", d)
		}
	}

	// Determinism: same seed => same sequence.
	pa := New(rr, nil, config.RetryConfig{MaxBackoff: cap}, "round_robin", nil, nil, config.UpstreamConfig{})
	pa.SetRand(rand.New(rand.NewSource(99)))
	pb := New(rr, nil, config.RetryConfig{MaxBackoff: cap}, "round_robin", nil, nil, config.UpstreamConfig{})
	pb.SetRand(rand.New(rand.NewSource(99)))
	for attempt := 1; attempt <= 5; attempt++ {
		if pa.calculateBackoff(attempt) != pb.calculateBackoff(attempt) {
			t.Fatalf("seeded backoff not deterministic at attempt %d", attempt)
		}
	}
}

// §3.3 Retry budget: once the retries/requests ratio exceeds Budget (past the
// small floor), further retries are denied and counted.
func TestRetryBudgetCapsRetries(t *testing.T) {
	rr := balancer.NewRoundRobin()
	be := balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:1", MaxConns: 100})
	rr.Add(be)

	// A high MaxAttempts so the budget (not MaxAttempts) is the limiter. Budget is
	// tiny so once past the floor, retries are denied.
	retry := config.RetryConfig{MaxAttempts: 50, MaxBackoff: 1 * time.Millisecond, Budget: 0.01}
	p := New(rr, nil, retry, "round_robin", nil, nil, config.UpstreamConfig{})
	p.SetRand(rand.New(rand.NewSource(1)))

	// Drive several dead-backend GETs. Each request would want 50 retries, but the
	// budget must cap the total.
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
		req.RemoteAddr = "203.0.113.9:5555"
		p.ServeHTTP(httptest.NewRecorder(), req)
	}

	retries := p.GetMetrics().GetPrometheusMetrics().TotalRetries
	if p.GetBudgetDenied() == 0 {
		t.Errorf("expected some budget-denied retries, got 0 (retries=%v)", retries)
	}
	// The floor allows retryBudgetFloor retries; beyond that the ratio (retries/reqs)
	// must stay under Budget. With 5 requests and Budget 0.01 the cap is well below
	// the 5*50=250 unbounded retries.
	if retries > 40 {
		t.Errorf("retry budget did not cap retries, got %v", retries)
	}
}

// §3.3 Budget=0 means unlimited (current behavior): a dead-backend GET retries
// MaxAttempts times with no denials.
func TestRetryBudgetUnlimitedByDefault(t *testing.T) {
	rr := balancer.NewRoundRobin()
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:1", MaxConns: 100}))
	retry := config.RetryConfig{MaxAttempts: 3, MaxBackoff: 1 * time.Millisecond} // Budget 0
	p := New(rr, nil, retry, "round_robin", nil, nil, config.UpstreamConfig{})

	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	p.ServeHTTP(httptest.NewRecorder(), req)

	if got := p.GetMetrics().GetPrometheusMetrics().TotalRetries; got != 3 {
		t.Errorf("Budget=0 must be unlimited: expected 3 retries, got %v", got)
	}
	if got := p.GetBudgetDenied(); got != 0 {
		t.Errorf("Budget=0 must not deny retries, got %d", got)
	}
}

// §3.8 Bulkhead: when every candidate backend is at its connection cap, the
// request is rejected with 503 and the rejection counter increments.
func TestBulkheadRejectsWith503(t *testing.T) {
	rr := balancer.NewRoundRobin()
	be := balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:1", MaxConns: 1})
	rr.Add(be)
	p := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})

	// Simulate the backend already saturated: one in-flight connection occupies the
	// only slot. selectBackend reserves a second, pushing ActiveConns over MaxConns.
	be.IncrConn() // pretend an existing in-flight request holds the single slot
	defer be.DecrConn()

	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when backend at capacity, got %d", rec.Code)
	}
	if p.GetRejections() == 0 {
		t.Error("expected the bulkhead rejection counter to increment")
	}
	if got := be.GetActiveConns(); got != 1 {
		t.Errorf("reservation leaked: ActiveConns=%d, want 1 (the pre-existing hold)", got)
	}
}

// §3.6 Hedged requests: with hedging enabled and a slow primary + fast secondary,
// the fast backend's response wins, is written exactly once, and the hedge-win
// counter increments.
func TestHedgingReturnsFastBackendAndWritesOnce(t *testing.T) {
	var slowWrites, fastWrites int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
		}
		atomic.AddInt32(&slowWrites, 1)
		io.WriteString(w, "slow")
	}))
	defer slow.Close()
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fastWrites, 1)
		io.WriteString(w, "fast")
	}))
	defer fast.Close()

	rr := balancer.NewRoundRobin()
	// Primary is the slow one (added first => round-robin picks it first).
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: slow.URL, MaxConns: 100}))
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: fast.URL, MaxConns: 100}))

	retry := config.RetryConfig{
		Hedge: config.HedgeConfig{Enabled: true, Delay: 50 * time.Millisecond, MaxExtra: 1},
	}
	p := New(rr, nil, retry, "round_robin", nil, nil, config.UpstreamConfig{})

	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()

	start := time.Now()
	p.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if elapsed > 1500*time.Millisecond {
		t.Fatalf("hedging did not race the fast backend: took %v", elapsed)
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "fast" {
		t.Fatalf("expected fast backend response, got %d/%q", rec.Code, rec.Body.String())
	}
	if p.GetHedgeWins() != 1 {
		t.Errorf("expected exactly one hedge win, got %d", p.GetHedgeWins())
	}
	if p.GetHedgedCount() < 1 {
		t.Errorf("expected at least one hedge attempt launched, got %d", p.GetHedgedCount())
	}
	// Reservations must all be released.
	for _, b := range rr.All() {
		if b.GetActiveConns() != 0 {
			t.Errorf("reservation leaked on %s: ActiveConns=%d", b.URL, b.GetActiveConns())
		}
	}
}

// §3.6 Hedging is opt-in and idempotent-only: a POST must not be hedged and must
// take the standard single-path (here, failover on connect error to the live one).
func TestHedgingSkippedForNonIdempotent(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "post-ok")
	}))
	defer live.Close()

	rr := balancer.NewRoundRobin()
	dead := balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:1", MaxConns: 100})
	good := balancer.NewBackend(config.BackendConfig{URL: live.URL, MaxConns: 100})
	rr.Add(dead)
	rr.Add(good)

	retry := config.RetryConfig{Hedge: config.HedgeConfig{Enabled: true, Delay: 10 * time.Millisecond, MaxExtra: 1}}
	p := New(rr, nil, retry, "round_robin", nil, nil, config.UpstreamConfig{})

	req := httptest.NewRequest(http.MethodPost, "http://proxy/", strings.NewReader("payload"))
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "post-ok" {
		t.Fatalf("expected non-hedged connect-error failover for POST, got %d/%q", rec.Code, rec.Body.String())
	}
	if p.GetHedgeWins() != 0 {
		t.Errorf("POST must not be hedged, got %d hedge wins", p.GetHedgeWins())
	}
	if dead.GetActiveConns() != 0 || good.GetActiveConns() != 0 {
		t.Errorf("reservations leaked: dead=%d good=%d", dead.GetActiveConns(), good.GetActiveConns())
	}
}

// §3.1 classification helper: transport deadline errors classify as "timeout",
// dial errors as "connect".
func TestClassifyError(t *testing.T) {
	if got := classifyError(context.Background(), nil); got != "" {
		t.Errorf("nil error should classify as empty, got %q", got)
	}
	if got := classifyError(context.Background(), context.DeadlineExceeded); got != "timeout" {
		t.Errorf("deadline should classify as timeout, got %q", got)
	}
	dialErr := &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	if got := classifyError(context.Background(), dialErr); got != "connect" {
		t.Errorf("dial error should classify as connect, got %q", got)
	}
}

// §3.6 Hedging with only one healthy backend must fall back to the standard
// single-path (no extra backend to hedge against) and not leak/double-release the
// primary reservation.
func TestHedgingSingleBackendFallsBack(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "solo")
	}))
	defer live.Close()

	rr := balancer.NewRoundRobin()
	be := balancer.NewBackend(config.BackendConfig{URL: live.URL, MaxConns: 100})
	rr.Add(be)

	retry := config.RetryConfig{Hedge: config.HedgeConfig{Enabled: true, Delay: 10 * time.Millisecond, MaxExtra: 1}}
	p := New(rr, nil, retry, "round_robin", nil, nil, config.UpstreamConfig{})

	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "solo" {
		t.Fatalf("expected single-backend fallback success, got %d/%q", rec.Code, rec.Body.String())
	}
	if p.GetHedgeWins() != 0 {
		t.Errorf("no hedging possible with one backend, got %d wins", p.GetHedgeWins())
	}
	if be.GetActiveConns() != 0 {
		t.Errorf("reservation leaked/double-released: ActiveConns=%d", be.GetActiveConns())
	}
}

// §5.1: the per-backend transport must reflect the config-driven upstream timeouts
// and pool sizes, and fall back to the hardcoded §0 constants when a field is zero.
func TestUpstreamConfigDrivesTransportTimeouts(t *testing.T) {
	up := config.UpstreamConfig{
		DialTimeout:           2 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 7 * time.Second,
		ExpectContinueTimeout: 400 * time.Millisecond,
		IdleConnTimeout:       11 * time.Second,
		MaxIdleConns:          5,
		MaxIdleConnsPerHost:   3,
		MaxConnsPerHost:       4,
	}
	be := balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:1", MaxConns: 100})
	rr := balancer.NewRoundRobin()
	rr.Add(be)
	p := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, up)

	rp, err := p.proxyFor(be)
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := rp.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", rp.Transport)
	}
	if tr.TLSHandshakeTimeout != 3*time.Second {
		t.Errorf("TLSHandshakeTimeout = %v, want 3s", tr.TLSHandshakeTimeout)
	}
	if tr.ResponseHeaderTimeout != 7*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 7s", tr.ResponseHeaderTimeout)
	}
	if tr.ExpectContinueTimeout != 400*time.Millisecond {
		t.Errorf("ExpectContinueTimeout = %v, want 400ms", tr.ExpectContinueTimeout)
	}
	if tr.IdleConnTimeout != 11*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 11s", tr.IdleConnTimeout)
	}
	if tr.MaxIdleConns != 5 {
		t.Errorf("MaxIdleConns = %d, want 5", tr.MaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != 3 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 3", tr.MaxIdleConnsPerHost)
	}
	if tr.MaxConnsPerHost != 4 {
		t.Errorf("MaxConnsPerHost = %d, want 4", tr.MaxConnsPerHost)
	}

	// §0 default fallback: a zero-value UpstreamConfig still yields the hardened
	// non-zero timeouts on the per-backend transport.
	p2 := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})
	rp2, err := p2.proxyFor(be)
	if err != nil {
		t.Fatal(err)
	}
	tr2 := rp2.Transport.(*http.Transport)
	if tr2.ResponseHeaderTimeout == 0 || tr2.TLSHandshakeTimeout == 0 ||
		tr2.ExpectContinueTimeout == 0 || tr2.IdleConnTimeout == 0 {
		t.Error("zero-value upstream config must fall back to the hardened §0 timeouts")
	}
	if tr2.MaxIdleConns == 0 || tr2.MaxIdleConnsPerHost == 0 {
		t.Error("zero-value upstream config must fall back to the default pool sizes")
	}
	if tr2.MaxConnsPerHost != 0 {
		t.Errorf("MaxConnsPerHost default must be 0 (unlimited), got %d", tr2.MaxConnsPerHost)
	}
}

// §5.2: each per-backend ReverseProxy owns its OWN *http.Transport so that
// MaxIdleConnsPerHost / MaxConnsPerHost apply per backend rather than being shared.
// Two different backends must therefore get distinct transport instances.
func TestPerBackendTransportsAreDistinct(t *testing.T) {
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer a.Close()
	defer b.Close()

	beA := balancer.NewBackend(config.BackendConfig{URL: a.URL, MaxConns: 100})
	beB := balancer.NewBackend(config.BackendConfig{URL: b.URL, MaxConns: 100})
	rr := balancer.NewRoundRobin()
	rr.Add(beA)
	rr.Add(beB)
	p := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})

	rpA, err := p.proxyFor(beA)
	if err != nil {
		t.Fatal(err)
	}
	rpB, err := p.proxyFor(beB)
	if err != nil {
		t.Fatal(err)
	}
	if rpA.Transport == nil || rpB.Transport == nil {
		t.Fatal("expected both backends to have a transport")
	}
	if rpA.Transport == rpB.Transport {
		t.Error("expected distinct per-backend transports, got a shared one")
	}
}

// §5.2 + TLS: the per-backend transport must preserve the configured backendTLS
// (TLSClientConfig) on every backend's own transport.
func TestPerBackendTransportPreservesTLS(t *testing.T) {
	tlsCfg := &tls.Config{InsecureSkipVerify: true}
	be := balancer.NewBackend(config.BackendConfig{URL: "https://127.0.0.1:1", MaxConns: 100})
	rr := balancer.NewRoundRobin()
	rr.Add(be)
	p := New(rr, nil, config.RetryConfig{}, "round_robin", nil, tlsCfg, config.UpstreamConfig{})

	rp, err := p.proxyFor(be)
	if err != nil {
		t.Fatal(err)
	}
	tr := rp.Transport.(*http.Transport)
	if tr.TLSClientConfig != tlsCfg {
		t.Error("per-backend transport must preserve the configured backendTLS")
	}
}

// §5.3: with HTTP2 enabled, an http (h2c) backend must use an http2.Transport while an
// https backend keeps the standard *http.Transport with ForceAttemptHTTP2 set.
func TestHTTP2SelectsTransportPerScheme(t *testing.T) {
	up := config.UpstreamConfig{HTTP2: true}
	beH2C := balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:1", MaxConns: 100})
	beTLS := balancer.NewBackend(config.BackendConfig{URL: "https://127.0.0.1:2", MaxConns: 100})
	rr := balancer.NewRoundRobin()
	rr.Add(beH2C)
	rr.Add(beTLS)
	p := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, up)

	rpH2C, err := p.proxyFor(beH2C)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rpH2C.Transport.(*http2.Transport); !ok {
		t.Errorf("h2c backend must use *http2.Transport, got %T", rpH2C.Transport)
	}

	rpTLS, err := p.proxyFor(beTLS)
	if err != nil {
		t.Fatal(err)
	}
	trTLS, ok := rpTLS.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("https backend must use *http.Transport, got %T", rpTLS.Transport)
	}
	if !trTLS.ForceAttemptHTTP2 {
		t.Error("https backend with HTTP2 enabled must set ForceAttemptHTTP2")
	}

	// With HTTP2 disabled, an http backend must NOT use http2.Transport.
	p2 := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})
	rp, err := p2.proxyFor(beH2C)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rp.Transport.(*http.Transport); !ok {
		t.Errorf("HTTP2 disabled: http backend must use *http.Transport, got %T", rp.Transport)
	}
}

// §5.8: cancelling the client request context must cancel the in-flight upstream call
// (the per-try/upstream context derives from r.Context()). The backend blocks until
// its own request context is Done; the proxy must return promptly after client cancel.
func TestClientCancelAbortsUpstream(t *testing.T) {
	unblocked := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // block until the upstream request context is cancelled
		close(unblocked)
	}))
	defer backend.Close()

	rr := balancer.NewRoundRobin()
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: backend.URL, MaxConns: 100}))
	// No per-try timeout: only the client cancel can abort the upstream call.
	p := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil).WithContext(ctx)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		p.ServeHTTP(rec, req)
		close(done)
	}()

	// Give the request time to reach the (blocking) backend, then cancel the client.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("client cancel did not abort the in-flight upstream request")
	}
	select {
	case <-unblocked:
	case <-time.After(2 * time.Second):
		t.Fatal("backend did not observe upstream context cancellation on client disconnect")
	}
}

// fakeRouter is a minimal proxy.Router that always returns the configured balancer,
// recording each Route call so the proxy's per-request routing wiring can be asserted.
type fakeRouter struct {
	b     balancer.Balancer
	calls int
}

func (fr *fakeRouter) Route(*http.Request) balancer.Balancer {
	fr.calls++
	return fr.b
}

// With a router installed, selection, service and feedback must all use the routed
// balancer's group — never the proxy's default p.balancer. The default balancer here
// points at a dead backend to prove it is NOT consulted.
func TestRouterSelectsRoutedGroup(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "routed-ok")
	}))
	defer live.Close()

	// Default group: a dead backend that must never be hit.
	def := balancer.NewRoundRobin()
	deadDefault := balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:1", MaxConns: 100})
	def.Add(deadDefault)
	p := New(def, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})

	// Routed group: a fake balancer wrapping the live backend, also capturing feedback.
	fb := &fakeBalancer{backend: balancer.NewBackend(config.BackendConfig{URL: live.URL, MaxConns: 100})}
	fr := &fakeRouter{b: fb}
	p.SetRouter(fr)

	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "routed-ok" {
		t.Fatalf("expected routed group to serve the request, got %d/%q", rec.Code, rec.Body.String())
	}
	if fr.calls == 0 {
		t.Error("expected the router to be consulted for the request")
	}
	// The routed (keyed) balancer must have been asked to select via NextForKey, and
	// the default balancer must not have been touched.
	if len(fb.nextForKeyCalls) != 1 {
		t.Errorf("expected routed balancer NextForKey called once, got %v", fb.nextForKeyCalls)
	}
	if deadDefault.GetActiveConns() != 0 {
		t.Errorf("default group must not be selected/reserved when routed: ActiveConns=%d", deadDefault.GetActiveConns())
	}
	// Feedback (latency + outcome) must have been fed to the ROUTED balancer.
	if fb.latencyCalls != 1 {
		t.Errorf("expected ObserveLatency on the routed balancer, got %d", fb.latencyCalls)
	}
	if fb.outcomeCalls != 1 || !fb.lastOutcome {
		t.Errorf("expected ObserveOutcome(true) on the routed balancer, got calls=%d ok=%v", fb.outcomeCalls, fb.lastOutcome)
	}
	if fb.backend.GetActiveConns() != 0 {
		t.Errorf("routed reservation leaked: ActiveConns=%d", fb.backend.GetActiveConns())
	}
}

// Failover must pick alternates from the SAME routed group. Here the routed group's
// primary is dead and its secondary is live; a healthy backend that lives ONLY in the
// default group must never be used as a failover target.
func TestRouterFailoverStaysWithinGroup(t *testing.T) {
	routedLive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "routed-alt")
	}))
	defer routedLive.Close()
	var defaultHits int32
	defaultLive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&defaultHits, 1)
		io.WriteString(w, "default")
	}))
	defer defaultLive.Close()

	// Default group is entirely live but must not be consulted for a routed request.
	def := balancer.NewRoundRobin()
	def.Add(balancer.NewBackend(config.BackendConfig{URL: defaultLive.URL, MaxConns: 100}))
	p := New(def, nil, config.RetryConfig{MaxAttempts: 0}, "round_robin", nil, nil, config.UpstreamConfig{})

	// Routed group: dead primary + live secondary, so success requires in-group failover.
	routed := balancer.NewRoundRobin()
	routedDead := balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:1", MaxConns: 100})
	routedGood := balancer.NewBackend(config.BackendConfig{URL: routedLive.URL, MaxConns: 100})
	routed.Add(routedDead)
	routed.Add(routedGood)
	p.SetRouter(&fakeRouter{b: routed})

	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "routed-alt" {
		t.Fatalf("expected in-group failover to the routed secondary, got %d/%q", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&defaultHits); got != 0 {
		t.Errorf("failover crossed into the default group: defaultHits=%d", got)
	}
	if routedDead.GetActiveConns() != 0 || routedGood.GetActiveConns() != 0 {
		t.Errorf("routed reservations leaked: dead=%d good=%d", routedDead.GetActiveConns(), routedGood.GetActiveConns())
	}
}

// A failing upstream in the routed group must report ok=false to the ROUTED
// balancer's OutcomeObserver (not the default), and must not feed its LatencyObserver.
func TestRouterOutcomeObserverOnFailure(t *testing.T) {
	// Default observer must never be told anything for a routed request.
	defObs := &fakeBalancer{backend: balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:2", MaxConns: 100})}
	p := New(defObs, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})

	routed := &fakeBalancer{backend: balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:1", MaxConns: 100})}
	p.SetRouter(&fakeRouter{b: routed})

	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if routed.outcomeCalls != 1 || routed.lastOutcome {
		t.Errorf("expected ObserveOutcome(false) once on the routed balancer, got calls=%d ok=%v", routed.outcomeCalls, routed.lastOutcome)
	}
	if routed.latencyCalls != 0 {
		t.Errorf("expected no ObserveLatency on failure for the routed balancer, got %d", routed.latencyCalls)
	}
	if defObs.outcomeCalls != 0 || defObs.latencyCalls != 0 {
		t.Errorf("default balancer must not receive feedback for a routed request: outcome=%d latency=%d", defObs.outcomeCalls, defObs.latencyCalls)
	}
}

// A nil router (never SetRouter) must preserve the exact single-balancer behavior:
// selection and feedback go through p.balancer.
func TestNoRouterUsesDefaultBalancer(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "default-ok")
	}))
	defer live.Close()

	fb := &fakeBalancer{backend: balancer.NewBackend(config.BackendConfig{URL: live.URL, MaxConns: 100})}
	p := New(fb, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})
	// No SetRouter call.

	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "default-ok" {
		t.Fatalf("expected default balancer to serve the request, got %d/%q", rec.Code, rec.Body.String())
	}
	if fb.latencyCalls != 1 || fb.outcomeCalls != 1 {
		t.Errorf("expected default balancer to receive feedback, got latency=%d outcome=%d", fb.latencyCalls, fb.outcomeCalls)
	}
}

// §5.7: the WebSocket wrapper enforces an idle read timeout (a silent peer trips the
// read deadline) and a cumulative max-message byte cap (a Read past the cap closes the
// connection and errors).
func TestWSConnEnforcesIdleTimeoutAndMaxBytes(t *testing.T) {
	// Idle timeout: with no data ever written to one end, a Read on the wrapped conn
	// must time out (be closed) rather than block forever.
	c1, c2 := net.Pipe()
	defer c2.Close()
	w := newWSConn(c1, 50*time.Millisecond, 0)
	buf := make([]byte, 16)
	start := time.Now()
	_, err := w.Read(buf)
	if err == nil {
		t.Error("expected an idle-timeout error on a silent connection")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("idle read blocked too long (%v); deadline not armed", elapsed)
	}

	// Max bytes: reading more than maxBytes total must error and close the conn.
	srv, cli := net.Pipe()
	defer srv.Close()
	go func() {
		srv.Write([]byte("0123456789")) // 10 bytes, cap is 4
		srv.Close()
	}()
	w2 := newWSConn(cli, 0, 4)
	var total int
	var readErr error
	for i := 0; i < 5; i++ {
		n, e := w2.Read(make([]byte, 8))
		total += n
		if e != nil {
			readErr = e
			break
		}
	}
	if readErr == nil {
		t.Error("expected a max-message-bytes error once the cap was exceeded")
	}
	if total <= 4 {
		t.Errorf("expected to read past the 4-byte cap before erroring, read %d", total)
	}
}

// §9.1: with the canary at weight 100, EVERY request must be served from the canary
// balancer's group and never from the default (stable) group. The default group here
// points at a live backend to prove it is not consulted.
func TestCanaryWeight100RoutesAllToCanary(t *testing.T) {
	var stableHits int32
	stable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&stableHits, 1)
		io.WriteString(w, "stable")
	}))
	defer stable.Close()
	canaryLive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "canary-ok")
	}))
	defer canaryLive.Close()

	stableBal := &fakeBalancer{backend: balancer.NewBackend(config.BackendConfig{URL: stable.URL, MaxConns: 100})}
	p := New(stableBal, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})

	canaryBal := &fakeBalancer{backend: balancer.NewBackend(config.BackendConfig{URL: canaryLive.URL, MaxConns: 100})}
	p.SetCanary(canaryBal, 100)
	p.SetRand(rand.New(rand.NewSource(1)))

	const n = 20
	for i := 0; i < n; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
		req.RemoteAddr = "203.0.113.9:5555"
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != "canary-ok" {
			t.Fatalf("request %d: expected canary to serve, got %d/%q", i, rec.Code, rec.Body.String())
		}
	}
	if got := atomic.LoadInt32(&stableHits); got != 0 {
		t.Errorf("weight 100 leaked traffic to the stable group: stableHits=%d", got)
	}
	// Every request must have selected + observed on the canary balancer.
	if canaryBal.nextCalls+len(canaryBal.nextForKeyCalls) != n {
		t.Errorf("expected %d canary selections, got next=%d nextForKey=%d", n, canaryBal.nextCalls, len(canaryBal.nextForKeyCalls))
	}
	if canaryBal.outcomeCalls != n || canaryBal.latencyCalls != n {
		t.Errorf("expected canary feedback per request, got outcome=%d latency=%d", canaryBal.outcomeCalls, canaryBal.latencyCalls)
	}
	if stableBal.nextCalls != 0 || len(stableBal.nextForKeyCalls) != 0 {
		t.Errorf("stable balancer must not be selected at weight 100: next=%d nextForKey=%d", stableBal.nextCalls, len(stableBal.nextForKeyCalls))
	}
	if canaryBal.backend.GetActiveConns() != 0 {
		t.Errorf("canary reservation leaked: ActiveConns=%d", canaryBal.backend.GetActiveConns())
	}
}

// §9.1: with the canary at weight 0, NO request may reach the canary group; all
// traffic flows through the normal stable path exactly as if no canary were set.
func TestCanaryWeight0RoutesNoneToCanary(t *testing.T) {
	stable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "stable-ok")
	}))
	defer stable.Close()

	stableBal := &fakeBalancer{backend: balancer.NewBackend(config.BackendConfig{URL: stable.URL, MaxConns: 100})}
	p := New(stableBal, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})

	var canaryHits int32
	canaryLive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&canaryHits, 1)
		io.WriteString(w, "canary")
	}))
	defer canaryLive.Close()
	canaryBal := &fakeBalancer{backend: balancer.NewBackend(config.BackendConfig{URL: canaryLive.URL, MaxConns: 100})}
	p.SetCanary(canaryBal, 0)
	p.SetRand(rand.New(rand.NewSource(1)))

	const n = 20
	for i := 0; i < n; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
		req.RemoteAddr = "203.0.113.9:5555"
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != "stable-ok" {
			t.Fatalf("request %d: expected stable to serve at weight 0, got %d/%q", i, rec.Code, rec.Body.String())
		}
	}
	if got := atomic.LoadInt32(&canaryHits); got != 0 {
		t.Errorf("weight 0 leaked traffic to the canary group: canaryHits=%d", got)
	}
	if canaryBal.nextCalls != 0 || len(canaryBal.nextForKeyCalls) != 0 || canaryBal.outcomeCalls != 0 {
		t.Errorf("canary balancer must be untouched at weight 0: next=%d nextForKey=%d outcome=%d",
			canaryBal.nextCalls, len(canaryBal.nextForKeyCalls), canaryBal.outcomeCalls)
	}
	if stableBal.outcomeCalls != n {
		t.Errorf("expected all %d requests observed on the stable balancer, got %d", n, stableBal.outcomeCalls)
	}
}

// §9.1: once a request is routed to the canary pool, in-group failover must stay
// within the canary group — never crossing into the stable group. Here the canary
// group's primary is dead and its secondary is live; the stable group is entirely live
// but must never be used as a failover target.
func TestCanaryFailoverStaysWithinCanaryGroup(t *testing.T) {
	canaryLive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "canary-alt")
	}))
	defer canaryLive.Close()
	var stableHits int32
	stable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&stableHits, 1)
		io.WriteString(w, "stable")
	}))
	defer stable.Close()

	// Stable group is entirely live but must not be consulted for a canary request.
	stableBal := balancer.NewRoundRobin()
	stableBal.Add(balancer.NewBackend(config.BackendConfig{URL: stable.URL, MaxConns: 100}))
	p := New(stableBal, nil, config.RetryConfig{MaxAttempts: 0}, "round_robin", nil, nil, config.UpstreamConfig{})

	// Canary group: dead primary + live secondary, so success requires in-group failover.
	canaryBal := balancer.NewRoundRobin()
	canaryDead := balancer.NewBackend(config.BackendConfig{URL: "http://127.0.0.1:1", MaxConns: 100})
	canaryGood := balancer.NewBackend(config.BackendConfig{URL: canaryLive.URL, MaxConns: 100})
	canaryBal.Add(canaryDead)
	canaryBal.Add(canaryGood)
	p.SetCanary(canaryBal, 100)
	p.SetRand(rand.New(rand.NewSource(1)))

	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "canary-alt" {
		t.Fatalf("expected in-group failover to the canary secondary, got %d/%q", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&stableHits); got != 0 {
		t.Errorf("canary failover crossed into the stable group: stableHits=%d", got)
	}
	if canaryDead.GetActiveConns() != 0 || canaryGood.GetActiveConns() != 0 {
		t.Errorf("canary reservations leaked: dead=%d good=%d", canaryDead.GetActiveConns(), canaryGood.GetActiveConns())
	}
}

// A nil canary (never SetCanary) must preserve the exact default behavior: all traffic
// flows through p.balancer.
func TestNoCanaryUsesDefaultBalancer(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "default-ok")
	}))
	defer live.Close()

	fb := &fakeBalancer{backend: balancer.NewBackend(config.BackendConfig{URL: live.URL, MaxConns: 100})}
	p := New(fb, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})
	// No SetCanary call.

	req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "default-ok" {
		t.Fatalf("expected default balancer to serve the request, got %d/%q", rec.Code, rec.Body.String())
	}
	if fb.outcomeCalls != 1 || fb.latencyCalls != 1 {
		t.Errorf("expected default balancer to receive feedback, got outcome=%d latency=%d", fb.outcomeCalls, fb.latencyCalls)
	}
}
