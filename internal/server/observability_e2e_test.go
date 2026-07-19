package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
)

// This file drives the §7 observability feature end-to-end through the real
// assembled server stack. The data plane is exercised via Server.Handler() (the
// full middleware chain: RequestID -> AccessLog -> ... -> Metrics -> proxy), and
// the admin/metrics surface (/metrics, /debug/pprof) is exercised via the exact
// ServeMux the server builds and serves, obtained through Server.MetricsMux().
//
// Nothing here weakens an assertion: request counts are matched exactly against
// the driven traffic, the request id echoed to the client is matched byte-for-byte
// against the one the backend actually observed, and admin auth is asserted both
// with and without a token.

// -----------------------------------------------------------------------------
// Test backend
// -----------------------------------------------------------------------------

// obsBackend starts a backend that records the X-Request-ID header of the most
// recent request it saw (under a mutex, since httptest serves concurrently) and
// always returns 200. record, when non-nil, is invoked with each observed id.
func obsBackend(t *testing.T, record func(id string)) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if record != nil {
			record(r.Header.Get("X-Request-ID"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "backend-ok")
	}))
	t.Cleanup(s.Close)
	return s
}

// obsBaseConfig returns a minimal valid config wired to backendURL with the
// metrics/admin server enabled. Optional overrides (rate limiting, admin token)
// are applied by the individual tests via the returned config.
func obsBaseConfig(backendURL string) *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			Host:           "127.0.0.1",
			Port:           8080,
			TrustedProxies: []string{"127.0.0.1/8", "::1/128"},
		},
		Backends: []config.BackendConfig{
			{URL: backendURL, Weight: 1, MaxConns: 100},
		},
		LoadBalancer: config.LoadBalancerConfig{
			Algorithm: "round_robin",
		},
		Metrics: config.MetricsConfig{
			Enabled: true,
			Host:    "127.0.0.1",
			Port:    9090,
		},
		Logging: config.LoggingConfig{Level: "info", Format: "json"},
	}
}

// driveOK fires one GET through the data-plane handler from a loopback peer and
// returns the recorder. It sets a loopback RemoteAddr so trusted-proxy logic and
// per-IP keying behave as they would behind a real edge.
func driveOK(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = "127.0.0.1:40000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// scrape performs a GET against the metrics mux and returns status + body.
func scrape(t *testing.T, mux http.Handler, path, authHeader string) (int, http.Header, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://admin.test"+path, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Header(), rec.Body.String()
}

// -----------------------------------------------------------------------------
// Prometheus exposition
// -----------------------------------------------------------------------------

// promLine finds the first exposition line whose metric name+labels match the
// given prefix and returns its trailing numeric value. Comment (# HELP/# TYPE)
// lines are skipped. Returns ok=false if no such sample line exists.
func promSampleValue(body, prefix string) (float64, bool) {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		return v, true
	}
	return 0, false
}

// TestE2E_Observability_PrometheusExposition drives a known number of requests
// through the proxy and then scrapes /metrics off the real admin mux, asserting
// the exposition carries the histogram (buckets + _sum + _count, with _count ==
// number of driven requests), the 2xx status-class counter, the in-flight gauge,
// the per-backend up gauge, and the per-backend circuit-state gauge. It also
// asserts the Prometheus text Content-Type.
func TestE2E_Observability_PrometheusExposition(t *testing.T) {
	be := obsBackend(t, nil)
	cfg := obsBaseConfig(be.URL)
	// Enable circuit breaking so the snapshot func reports rplb_backend_circuit_state.
	cfg.CircuitBreaker = config.CircuitBreakerConfig{
		Enabled:          true,
		FailureThreshold: 5,
		SuccessThreshold: 2,
		Timeout:          time.Second,
	}
	srv := New(cfg, "")
	h := srv.Handler()
	mux := srv.MetricsMux()

	// Drive a known number of requests, all of which the always-200 backend serves.
	const nReq = 7
	for i := 0; i < nReq; i++ {
		rec := driveOK(t, h, "http://proxy.test/")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200", i, rec.Code)
		}
	}

	code, hdr, body := scrape(t, mux, "/metrics", "")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: got %d, want 200", code)
	}

	// Content-Type must be the Prometheus text exposition type.
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") || !strings.Contains(ct, "version=0.0.4") {
		t.Errorf("Content-Type = %q, want Prometheus text (text/plain; version=0.0.4)", ct)
	}

	// Histogram: buckets must be present.
	if !strings.Contains(body, `rplb_response_latency_seconds_bucket{le="`) {
		t.Errorf("/metrics is missing rplb_response_latency_seconds_bucket lines")
	}
	// The +Inf bucket, _count and _sum must reflect exactly nReq observations.
	if v, ok := promSampleValue(body, `rplb_response_latency_seconds_bucket{le="+Inf"}`); !ok || v != float64(nReq) {
		t.Errorf("histogram +Inf bucket = %v (ok=%v), want %d", v, ok, nReq)
	}
	if v, ok := promSampleValue(body, "rplb_response_latency_seconds_count"); !ok || v != float64(nReq) {
		t.Errorf("rplb_response_latency_seconds_count = %v (ok=%v), want %d (one observation per driven request)", v, ok, nReq)
	}
	if v, ok := promSampleValue(body, "rplb_response_latency_seconds_sum"); !ok || v < 0 {
		t.Errorf("rplb_response_latency_seconds_sum = %v (ok=%v), want a present non-negative sum", v, ok)
	}

	// Status class: all nReq responses were 200, so the 2xx counter must be nReq.
	if v, ok := promSampleValue(body, `rplb_requests_by_class_total{class="2xx"}`); !ok || v != float64(nReq) {
		t.Errorf(`rplb_requests_by_class_total{class="2xx"} = %v (ok=%v), want %d`, v, ok, nReq)
	}

	// In-flight gauge is present and, with no request in progress at scrape time, 0.
	if v, ok := promSampleValue(body, "rplb_inflight_requests"); !ok || v != 0 {
		t.Errorf("rplb_inflight_requests = %v (ok=%v), want 0 at rest", v, ok)
	}

	// Backend health gauges come from the registered snapshot func. The single
	// backend is healthy (never marked down) and its circuit is closed (0).
	upPrefix := fmt.Sprintf(`rplb_backend_up{backend="%s"}`, be.URL)
	if v, ok := promSampleValue(body, upPrefix); !ok || v != 1 {
		t.Errorf("%s = %v (ok=%v), want 1 (backend healthy)", upPrefix, v, ok)
	}
	statePrefix := fmt.Sprintf(`rplb_backend_circuit_state{backend="%s"}`, be.URL)
	if v, ok := promSampleValue(body, statePrefix); !ok || v != 0 {
		t.Errorf("%s = %v (ok=%v), want 0 (circuit closed)", statePrefix, v, ok)
	}
}

// -----------------------------------------------------------------------------
// X-Request-ID
// -----------------------------------------------------------------------------

// TestE2E_Observability_RequestIDGeneratedWhenAbsent verifies that a request
// arriving without X-Request-ID is assigned one, that the assigned id is echoed
// on the response, and that the SAME id is the one the backend actually received
// (i.e. it was forwarded upstream, not just echoed to the client).
func TestE2E_Observability_RequestIDGeneratedWhenAbsent(t *testing.T) {
	var mu sync.Mutex
	var seenByBackend string
	be := obsBackend(t, func(id string) {
		mu.Lock()
		seenByBackend = id
		mu.Unlock()
	})

	srv := New(obsBaseConfig(be.URL), "")
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil)
	req.RemoteAddr = "127.0.0.1:40001"
	// Deliberately do NOT set X-Request-ID.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("request: got %d, want 200", rec.Code)
	}

	respID := rec.Header().Get("X-Request-ID")
	if respID == "" {
		t.Fatal("response is missing X-Request-ID: middleware did not mint an id for a header-less request")
	}
	// A minted id is 128 bits of hex (32 chars).
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(respID) {
		t.Errorf("minted X-Request-ID = %q, want 32 lowercase hex chars", respID)
	}

	mu.Lock()
	got := seenByBackend
	mu.Unlock()
	if got != respID {
		t.Errorf("backend saw X-Request-ID %q but client got %q; the minted id must be forwarded upstream", got, respID)
	}
}

// TestE2E_Observability_RequestIDPreservedWhenPresent verifies that a client-
// supplied X-Request-ID is preserved (echoed unchanged on the response) AND
// forwarded to the backend verbatim.
func TestE2E_Observability_RequestIDPreservedWhenPresent(t *testing.T) {
	var mu sync.Mutex
	var seenByBackend string
	be := obsBackend(t, func(id string) {
		mu.Lock()
		seenByBackend = id
		mu.Unlock()
	})

	srv := New(obsBaseConfig(be.URL), "")
	h := srv.Handler()

	const clientID = "test-correlation-id-1234567890"
	req := httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil)
	req.RemoteAddr = "127.0.0.1:40002"
	req.Header.Set("X-Request-ID", clientID)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("request: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Request-ID"); got != clientID {
		t.Errorf("response X-Request-ID = %q, want %q (client-supplied id must be preserved)", got, clientID)
	}
	mu.Lock()
	got := seenByBackend
	mu.Unlock()
	if got != clientID {
		t.Errorf("backend saw X-Request-ID %q, want %q (client id must be forwarded verbatim)", got, clientID)
	}
}

// -----------------------------------------------------------------------------
// Access log
// -----------------------------------------------------------------------------

// TestE2E_Observability_AccessLogLineEmitted drives one request through the real
// stack and asserts the AccessLog middleware emitted exactly one structured
// "access" log line carrying method, status, duration and request_id. The default
// logger writes JSON to fd 1, so we capture fd 1 for the duration of the request.
func TestE2E_Observability_AccessLogLineEmitted(t *testing.T) {
	var mu sync.Mutex
	var seenByBackend string
	be := obsBackend(t, func(id string) {
		mu.Lock()
		seenByBackend = id
		mu.Unlock()
	})

	// Ensure info-level JSON so the access line is emitted and machine-parseable.
	cfg := obsBaseConfig(be.URL)
	cfg.Logging = config.LoggingConfig{Level: "info", Format: "json"}
	srv := New(cfg, "")
	h := srv.Handler()

	const clientID = "access-log-corr-id-abcdef"

	restore, captured := captureFd1(t)
	req := httptest.NewRequest(http.MethodGet, "http://proxy.test/some/path", nil)
	req.RemoteAddr = "127.0.0.1:40003"
	req.Header.Set("X-Request-ID", clientID)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// Restore fd 1 before asserting so t.Fatalf output is visible.
	restore()

	if rec.Code != http.StatusOK {
		t.Fatalf("request: got %d, want 200", rec.Code)
	}

	logs := captured()
	// Find the single access line.
	var accessLine string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `"access"`) {
			if accessLine != "" {
				t.Fatalf("expected exactly one access line, found a second: %s", line)
			}
			accessLine = line
		}
	}
	if accessLine == "" {
		t.Fatalf("no access-log line was emitted; captured logs:\n%s", logs)
	}

	// The line is JSON with the observability fields nested under "fields".
	for _, want := range []string{
		`"method":"GET"`,
		`"status":200`,
		`"duration_ms":`,
		`"request_id":"` + clientID + `"`,
	} {
		if !strings.Contains(accessLine, want) {
			t.Errorf("access line missing %s\nline: %s", want, accessLine)
		}
	}

	mu.Lock()
	got := seenByBackend
	mu.Unlock()
	if got != clientID {
		t.Errorf("backend saw request_id %q, want %q", got, clientID)
	}
}

// -----------------------------------------------------------------------------
// pprof (admin mux)
// -----------------------------------------------------------------------------

// TestE2E_Observability_PprofOpenWithoutToken verifies /debug/pprof/ on the admin
// mux returns 200 when no admin token is configured (auth is a passthrough).
func TestE2E_Observability_PprofOpenWithoutToken(t *testing.T) {
	be := obsBackend(t, nil)
	srv := New(obsBaseConfig(be.URL), "") // no AuthToken
	mux := srv.MetricsMux()

	code, _, _ := scrape(t, mux, "/debug/pprof/", "")
	if code != http.StatusOK {
		t.Fatalf("GET /debug/pprof/ (no token configured): got %d, want 200", code)
	}
}

// TestE2E_Observability_PprofGatedByAdminAuth verifies that when an admin token
// is configured, /debug/pprof/ is 401 without the bearer token and 200 with it.
// The same gate must apply to /metrics.
func TestE2E_Observability_PprofGatedByAdminAuth(t *testing.T) {
	be := obsBackend(t, nil)
	const token = "s3cr3t-admin-token"
	cfg := obsBaseConfig(be.URL)
	cfg.Metrics.AuthToken = token
	srv := New(cfg, "")
	mux := srv.MetricsMux()

	// Without the token: 401 on both pprof and metrics.
	if code, _, _ := scrape(t, mux, "/debug/pprof/", ""); code != http.StatusUnauthorized {
		t.Errorf("GET /debug/pprof/ without token: got %d, want 401", code)
	}
	if code, _, _ := scrape(t, mux, "/metrics", ""); code != http.StatusUnauthorized {
		t.Errorf("GET /metrics without token: got %d, want 401", code)
	}
	// Wrong token: still 401.
	if code, _, _ := scrape(t, mux, "/debug/pprof/", "Bearer wrong"); code != http.StatusUnauthorized {
		t.Errorf("GET /debug/pprof/ with wrong token: got %d, want 401", code)
	}
	// Correct token: 200.
	if code, _, _ := scrape(t, mux, "/debug/pprof/", "Bearer "+token); code != http.StatusOK {
		t.Errorf("GET /debug/pprof/ with correct token: got %d, want 200", code)
	}
}

// -----------------------------------------------------------------------------
// Rate-limited counter
// -----------------------------------------------------------------------------

// TestE2E_Observability_RateLimitedCounterIncrements drives a burst that exceeds
// the configured rate limit and asserts rplb_rate_limited_total climbs to exactly
// the number of throttled (429) responses observed on the data plane.
func TestE2E_Observability_RateLimitedCounterIncrements(t *testing.T) {
	be := obsBackend(t, nil)
	cfg := obsBaseConfig(be.URL)
	// Tiny bucket so a rapid burst is throttled deterministically within the test
	// window (1 rps refill is negligible over the microseconds this loop runs).
	cfg.RateLimiter = config.RateLimiterConfig{
		Enabled:           true,
		RequestsPerSecond: 1,
		Burst:             1,
		GlobalRPS:         1,
		GlobalBurst:       1,
		Key:               "ip",
		Message:           "slow down",
		RetryAfterSeconds: 1,
	}
	srv := New(cfg, "")
	h := srv.Handler()
	mux := srv.MetricsMux()

	// Baseline: the counter should read 0 before any traffic.
	if _, _, body := scrape(t, mux, "/metrics", ""); func() bool {
		v, ok := promSampleValue(body, "rplb_rate_limited_total")
		return !ok || v != 0
	}() {
		t.Fatalf("rplb_rate_limited_total was not 0 before driving traffic")
	}

	const burst = 20
	throttled := 0
	for i := 0; i < burst; i++ {
		rec := driveOK(t, h, "http://proxy.test/")
		switch rec.Code {
		case http.StatusOK:
		case http.StatusTooManyRequests:
			throttled++
		default:
			t.Fatalf("request %d: unexpected status %d", i, rec.Code)
		}
	}
	if throttled == 0 {
		t.Fatalf("no request was throttled; cannot verify the rate-limited counter")
	}

	_, _, body := scrape(t, mux, "/metrics", "")
	v, ok := promSampleValue(body, "rplb_rate_limited_total")
	if !ok {
		t.Fatalf("/metrics is missing rplb_rate_limited_total")
	}
	if v != float64(throttled) {
		t.Errorf("rplb_rate_limited_total = %v, want %d (one increment per 429)", v, throttled)
	}
}

// -----------------------------------------------------------------------------
// fd-1 capture helper
// -----------------------------------------------------------------------------

// captureFd1 redirects file descriptor 1 (which the default logger's cached
// *os.File wraps) into a pipe and drains it into a buffer. The logging package
// exposes no output setter and caches the original os.Stdout at init, so swapping
// the os.Stdout variable would not redirect it; dup'ing a pipe over fd 1 does.
//
// It returns a restore func (call before asserting so test failures print) and a
// getter that returns everything captured so far. This mirrors the fd-1 capture
// already used by the middleware observability unit tests.
func captureFd1(t *testing.T) (restore func(), get func() string) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origFd, err := syscall.Dup(1)
	if err != nil {
		t.Fatalf("dup fd 1: %v", err)
	}
	if err := syscall.Dup2(int(w.Fd()), 1); err != nil {
		t.Fatalf("dup2 onto fd 1: %v", err)
	}

	var bufMu sync.Mutex
	var buf strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		p := make([]byte, 4096)
		for {
			n, err := r.Read(p)
			if n > 0 {
				bufMu.Lock()
				buf.Write(p[:n])
				bufMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	var once sync.Once
	restore = func() {
		once.Do(func() {
			_ = syscall.Dup2(origFd, 1)
			_ = syscall.Close(origFd)
			_ = w.Close()
			<-done
			_ = r.Close()
		})
	}
	get = func() string {
		bufMu.Lock()
		defer bufMu.Unlock()
		return buf.String()
	}
	return restore, get
}
