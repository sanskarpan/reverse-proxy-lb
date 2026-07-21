package server

// coverage_boost2_test.go supplements coverage_boost_test.go with additional
// tests for reloadConfig route branches, Stop nil-guard paths, DNS discovery,
// and setupProxy/setupBalancer variant paths.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"reverse-proxy-lb/internal/config"
)

// ---- reloadConfig route-reload branch ----

// TestReloadConfig_RoutesWithRouter exercises the live route-reload path:
// when a router is installed and the route table changes, UpdateRoutes is called.
func TestReloadConfig_RoutesWithRouter(t *testing.T) {
	t.Parallel()
	def := newIDBackend("DEFAULT", nil)
	defer def.close()
	api := newIDBackend("API", nil)
	defer api.close()

	initial := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 19095
backends:
  - url: %q
    weight: 1
load_balancer:
  algorithm: "round_robin"
  health_check:
    enabled: false
routes:
  - name: "api"
    path_prefix: "/api"
    algorithm: "round_robin"
    backends:
      - url: %q
        weight: 1
circuit_breaker:
  enabled: false
rate_limiter:
  enabled: false
metrics:
  enabled: false
compression:
  enabled: false
`, def.url, api.url)

	f, err := os.CreateTemp(t.TempDir(), "reload-routes-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(initial)
	f.Close()

	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	s := New(cfg, f.Name())

	if s.router == nil {
		t.Fatal("expected router to be installed")
	}

	// Now write a config with a different route prefix to trigger UpdateRoutes.
	updated := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 19095
backends:
  - url: %q
    weight: 1
load_balancer:
  algorithm: "round_robin"
  health_check:
    enabled: false
routes:
  - name: "api-v2"
    path_prefix: "/v2"
    algorithm: "round_robin"
    backends:
      - url: %q
        weight: 1
circuit_breaker:
  enabled: false
rate_limiter:
  enabled: false
metrics:
  enabled: false
compression:
  enabled: false
`, def.url, api.url)

	if err := os.WriteFile(f.Name(), []byte(updated), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Should not panic; router.UpdateRoutes is called inside.
	s.reloadConfig()
}

// TestReloadConfig_RoutesNoRouterWarning exercises the warning path in reloadConfig
// when the config gains routes but no router was initially installed.
func TestReloadConfig_RoutesNoRouterWarning(t *testing.T) {
	t.Parallel()
	def := newIDBackend("DEFAULT", nil)
	defer def.close()
	api := newIDBackend("API", nil)
	defer api.close()

	// Initial config: NO routes (so s.router is nil).
	initial := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 19096
backends:
  - url: %q
    weight: 1
load_balancer:
  algorithm: "round_robin"
  health_check:
    enabled: false
circuit_breaker:
  enabled: false
rate_limiter:
  enabled: false
metrics:
  enabled: false
compression:
  enabled: false
`, def.url)

	f, err := os.CreateTemp(t.TempDir(), "reload-norouter-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(initial)
	f.Close()

	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	s := New(cfg, f.Name())

	if s.router != nil {
		t.Fatal("expected no router in initial config")
	}

	// Write updated config that adds a route.
	updated := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 19096
backends:
  - url: %q
    weight: 1
load_balancer:
  algorithm: "round_robin"
  health_check:
    enabled: false
routes:
  - name: "new-api"
    path_prefix: "/api"
    algorithm: "round_robin"
    backends:
      - url: %q
        weight: 1
circuit_breaker:
  enabled: false
rate_limiter:
  enabled: false
metrics:
  enabled: false
compression:
  enabled: false
`, def.url, api.url)

	if err := os.WriteFile(f.Name(), []byte(updated), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Should log a warning and return cleanly.
	s.reloadConfig()
}

// ---- setupDiscovery DNS path ----

// TestSetupDiscovery_WithDNS verifies that when a DNS discovery target is
// configured, s.discoverer is non-nil after New().
func TestSetupDiscovery_WithDNS(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host: "127.0.0.1",
			Port: 19097,
		},
		Backends: []config.BackendConfig{
			{URL: be.URL, Weight: 1},
		},
		LoadBalancer: config.LoadBalancerConfig{
			Algorithm: "round_robin",
		},
		Discovery: config.DiscoveryConfig{
			DNS: []config.DNSTarget{
				{
					Name:     "localhost",
					Port:     8080,
					Interval: 60_000_000_000, // 1 minute — won't fire during test
				},
			},
		},
		Logging: config.LoggingConfig{Level: "error"},
	}

	s := New(cfg, "")
	if s.discoverer == nil {
		t.Error("expected discoverer to be non-nil when DNS targets are configured")
	}
	// Stop the discoverer so its goroutine doesn't outlive the test.
	s.discoverer.Stop()
}

// ---- setupBalancer unknown algorithm fallback ----

// TestSetupBalancer_UnknownAlgorithmFallback verifies that New() falls back to
// round-robin when an unknown algorithm is specified (rather than panicking).
func TestSetupBalancer_UnknownAlgorithmFallback(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host: "127.0.0.1",
			Port: 19098,
		},
		Backends: []config.BackendConfig{
			{URL: be.URL, Weight: 1},
		},
		LoadBalancer: config.LoadBalancerConfig{
			Algorithm: "no_such_algorithm",
		},
		Logging: config.LoggingConfig{Level: "error"},
	}

	// Should not panic; falls back to round robin.
	s := New(cfg, "")
	if s.balancer == nil {
		t.Error("expected balancer to be non-nil even with unknown algorithm")
	}
	if len(s.balancer.All()) == 0 {
		t.Error("expected at least one backend after fallback")
	}
}

// ---- setupProxy circuit-breaker rolling mode ----

// TestSetupProxy_CircuitBreakerRollingMode exercises the rolling circuit-breaker
// mode code path in setupProxy.
func TestSetupProxy_CircuitBreakerRollingMode(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host: "127.0.0.1",
			Port: 19099,
		},
		Backends: []config.BackendConfig{
			{URL: be.URL, Weight: 1},
		},
		LoadBalancer: config.LoadBalancerConfig{
			Algorithm: "round_robin",
		},
		CircuitBreaker: config.CircuitBreakerConfig{
			Enabled:          true,
			Mode:             "rolling",
			FailureThreshold: 5,
			SuccessThreshold: 2,
		},
		Logging: config.LoggingConfig{Level: "error"},
	}

	s := New(cfg, "")
	if s.circuitBreaker == nil {
		t.Error("expected circuit breaker to be non-nil when enabled")
	}
}

// ---- Stop covers health-checker teardown ----

// TestStop_WithHealthCheckers exercises the health-checker stop path in Stop().
func TestStop_WithHealthCheckers(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	cfg := baseConfig("round_robin", []config.BackendConfig{{URL: be.URL, Weight: 1}})
	cfg.LoadBalancer.HealthCheck = config.HealthCheckConfig{
		Enabled:  true,
		Path:     "/healthz",
		Interval: 60_000_000_000,
		Timeout:  5_000_000_000,
	}
	s := New(cfg, "")

	if len(s.healthChks) == 0 {
		t.Fatal("expected health checkers to be set up")
	}

	// Stop() must stop the health checkers cleanly. Since we did not call Start(),
	// httpServer.Shutdown completes immediately.
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop() returned error: %v", err)
	}
}

// ---- handleAdminBackends deduplication ----

// TestHandleAdminBackends_Deduplication verifies that a backend URL that appears
// in both the default group and a route group is only listed once.
func TestHandleAdminBackends_Deduplication(t *testing.T) {
	t.Parallel()
	shared := newIDBackend("SHARED", nil)
	defer shared.close()

	cfg := baseConfig("round_robin", backendCfgs(shared))
	cfg.Routes = []config.RouteConfig{
		{
			Name:       "api",
			PathPrefix: "/api",
			Algorithm:  "round_robin",
			Backends:   backendCfgs(shared), // same URL as default group
		},
	}
	s := New(cfg, "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/backends", nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}

	var out []adminBackend
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Even though shared.url appears in two groups, the response should list it only once.
	count := 0
	for _, b := range out {
		if b.URL == shared.url {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected shared backend to appear once, got %d", count)
	}
}

// ---- routeGroupsEqual ----

func TestRouteGroupsEqual(t *testing.T) {
	t.Parallel()
	be1 := config.BackendConfig{URL: "http://a:1", Weight: 1}
	be2 := config.BackendConfig{URL: "http://b:2", Weight: 1}

	tests := []struct {
		name string
		a, b []config.RouteConfig
		want bool
	}{
		{
			name: "identical",
			a: []config.RouteConfig{
				{PathPrefix: "/api", Algorithm: "round_robin", Backends: []config.BackendConfig{be1}},
			},
			b: []config.RouteConfig{
				{PathPrefix: "/api", Algorithm: "round_robin", Backends: []config.BackendConfig{be1}},
			},
			want: true,
		},
		{
			name: "different path prefix",
			a:    []config.RouteConfig{{PathPrefix: "/api", Algorithm: "round_robin"}},
			b:    []config.RouteConfig{{PathPrefix: "/v2", Algorithm: "round_robin"}},
			want: false,
		},
		{
			name: "different algorithm",
			a:    []config.RouteConfig{{PathPrefix: "/api", Algorithm: "round_robin"}},
			b:    []config.RouteConfig{{PathPrefix: "/api", Algorithm: "least_connections"}},
			want: false,
		},
		{
			name: "different backends",
			a:    []config.RouteConfig{{PathPrefix: "/api", Backends: []config.BackendConfig{be1}}},
			b:    []config.RouteConfig{{PathPrefix: "/api", Backends: []config.BackendConfig{be2}}},
			want: false,
		},
		{
			name: "different length",
			a:    []config.RouteConfig{{PathPrefix: "/api"}},
			b:    []config.RouteConfig{{PathPrefix: "/api"}, {PathPrefix: "/v2"}},
			want: false,
		},
		{
			name: "both empty",
			a:    []config.RouteConfig{},
			b:    []config.RouteConfig{},
			want: true,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := routeGroupsEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("routeGroupsEqual = %v, want %v", got, tc.want)
			}
		})
	}
}
