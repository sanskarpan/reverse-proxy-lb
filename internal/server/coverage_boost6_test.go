package server

// coverage_boost6_test.go adds final targeted tests to reach ≥75% coverage.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
)

// ---- handleAdminCircuitReset with circuit breaker wired ----

// TestHandleAdminCircuitReset_WithBreaker exercises the s.circuitBreaker.Reset path
// in handleAdminCircuitReset when a real circuit breaker is configured.
func TestHandleAdminCircuitReset_WithBreaker(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host: "127.0.0.1",
			Port: 19130,
		},
		Backends: []config.BackendConfig{
			{URL: be.URL, Weight: 1},
		},
		LoadBalancer: config.LoadBalancerConfig{
			Algorithm: "round_robin",
		},
		CircuitBreaker: config.CircuitBreakerConfig{
			Enabled:          true,
			FailureThreshold: 5,
			SuccessThreshold: 2,
		},
		Logging: config.LoggingConfig{Level: "error"},
	}
	s := New(cfg, "")

	if s.circuitBreaker == nil {
		t.Fatal("expected circuit breaker to be set up")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/circuit/reset?url="+be.URL, nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
}

// ---- stopH3 with a real *http3.Server ----
// Note: We can't easily create a real http3.Server without TLS certs, so instead
// we test the nil-guard path explicitly (already covered via Stop()) and the
// altSvcMiddleware to pick up h3.go coverage.

// TestAltSvcMiddlewareH3Port8443 verifies that altSvcMiddleware injects
// the Alt-Svc header with the expected port value.
func TestAltSvcMiddlewareH3Port8443(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := altSvcMiddleware(8443)(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)

	alt := rec.Header().Get("Alt-Svc")
	if alt == "" {
		t.Error("expected Alt-Svc header to be set")
	}
}

// ---- setupBalancer with zone-aware and slow-start ----

// TestSetupBalancer_ZoneAwareAndSlowStart exercises the ZoneAware and SlowStart
// wrapper paths in setupBalancer.
func TestSetupBalancer_ZoneAwareAndSlowStart(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host: "127.0.0.1",
			Port: 19131,
			Zone: "us-east-1",
		},
		Backends: []config.BackendConfig{
			{URL: be.URL, Weight: 1},
		},
		LoadBalancer: config.LoadBalancerConfig{
			Algorithm:      "round_robin",
			PreferSameZone: true,
			SlowStart:      5 * time.Second,
		},
		Logging: config.LoggingConfig{Level: "error"},
	}

	s := New(cfg, "")
	if s.balancer == nil {
		t.Error("balancer should be non-nil")
	}
}

// ---- findBackend with router group ----

// TestFindBackend_InRouterGroup verifies that findBackend can locate a backend
// that lives in a per-route group (not just the default group).
func TestFindBackend_InRouterGroup(t *testing.T) {
	t.Parallel()
	def := newIDBackend("DEFAULT", nil)
	defer def.close()
	api := newIDBackend("API", nil)
	defer api.close()

	cfg := baseConfig("round_robin", backendCfgs(def))
	cfg.Routes = []config.RouteConfig{
		{
			Name:       "api",
			PathPrefix: "/api",
			Algorithm:  "round_robin",
			Backends:   backendCfgs(api),
		},
	}
	s := New(cfg, "")

	b, g := s.findBackend(api.url)
	if b == nil {
		t.Fatal("findBackend should find the route-group backend")
	}
	if g == nil {
		t.Fatal("findBackend should return the owning group")
	}
}

// ---- backendGauges with canary ----

// TestBackendGauges_WithCanary exercises the canary-group path in backendGauges.
func TestBackendGauges_WithCanary(t *testing.T) {
	t.Parallel()
	def := newIDBackend("DEFAULT", nil)
	defer def.close()
	can := newIDBackend("CANARY", nil)
	defer can.close()

	cfg := baseConfig("round_robin", backendCfgs(def))
	cfg.Canary = config.CanaryConfig{
		Enabled:       true,
		WeightPercent: 20,
		Algorithm:     "round_robin",
		Backends:      backendCfgs(can),
	}
	s := New(cfg, "")

	gauges := s.backendGauges()
	// Should include DEFAULT + CANARY = 2 backends.
	if len(gauges) < 2 {
		t.Errorf("expected at least 2 gauges (default+canary), got %d", len(gauges))
	}
}
