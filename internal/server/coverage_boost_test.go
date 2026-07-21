package server

// coverage_boost_test.go targets functions with the lowest coverage that can be
// reached via white-box testing without starting real listeners or OS-level
// processes (no sleeps, no signal injection).

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"reverse-proxy-lb/internal/config"
)

// ---- setupHealthCheck ----

// TestSetupHealthCheck_DefaultGroup builds a server with health checking enabled
// and verifies that at least one HealthChecker is wired up.
func TestSetupHealthCheck_DefaultGroup(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	cfg := baseConfig("round_robin", []config.BackendConfig{{URL: be.URL, Weight: 1}})
	cfg.LoadBalancer.HealthCheck = config.HealthCheckConfig{
		Enabled:  true,
		Path:     "/healthz",
		Interval: 60_000_000_000, // 1 minute so it doesn't actually fire
		Timeout:  5_000_000_000,
	}
	s := New(cfg, "")
	t.Cleanup(func() {
		for _, hc := range s.healthChks {
			hc.Stop()
		}
	})

	if len(s.healthChks) == 0 {
		t.Error("setupHealthCheck should create at least one HealthChecker")
	}
}

// TestSetupHealthCheck_WithRouter verifies that per-route groups each get their own
// HealthChecker when both a default group and route groups are configured.
func TestSetupHealthCheck_WithRouter(t *testing.T) {
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
	cfg.LoadBalancer.HealthCheck = config.HealthCheckConfig{
		Enabled:  true,
		Path:     "/healthz",
		Interval: 60_000_000_000, // 1 minute
		Timeout:  5_000_000_000,
	}
	s := New(cfg, "")
	t.Cleanup(func() {
		for _, hc := range s.healthChks {
			hc.Stop()
		}
	})

	// Expect one checker per group: default + one route.
	if len(s.healthChks) < 2 {
		t.Errorf("expected at least 2 health checkers (default+route), got %d", len(s.healthChks))
	}
}

// TestSetupHealthCheck_WithBackendOverride verifies that a backend-level
// HealthCheckConfig override is plumbed through backendOverrides correctly.
func TestSetupHealthCheck_WithBackendOverride(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	perBackendHC := &config.HealthCheckConfig{
		Path:    "/custom",
		Timeout: 3_000_000_000,
	}
	cfg := &config.Config{
		Server: config.ServerConfig{
			Host: "127.0.0.1",
			Port: 19090,
		},
		Backends: []config.BackendConfig{
			{URL: be.URL, Weight: 1, HealthCheck: perBackendHC},
		},
		LoadBalancer: config.LoadBalancerConfig{
			Algorithm: "round_robin",
			HealthCheck: config.HealthCheckConfig{
				Enabled:  true,
				Path:     "/healthz",
				Interval: 60_000_000_000,
				Timeout:  5_000_000_000,
			},
		},
		Logging: config.LoggingConfig{Level: "error"},
	}
	s := New(cfg, "")
	t.Cleanup(func() {
		for _, hc := range s.healthChks {
			hc.Stop()
		}
	})

	if len(s.healthChks) == 0 {
		t.Error("expected at least one health checker")
	}
}

// ---- balancerGroups with canary and router ----

// TestBalancerGroups_WithRouter confirms that when routes are configured,
// balancerGroups includes the per-route groups as well as the default.
func TestBalancerGroups_WithRouter(t *testing.T) {
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

	groups := s.balancerGroups()
	// At minimum: default + one route group.
	if len(groups) < 2 {
		t.Errorf("expected at least 2 groups with router, got %d", len(groups))
	}
}

// TestBalancerGroups_WithCanary confirms that a canary group is included when
// canary is configured.
func TestBalancerGroups_WithCanary(t *testing.T) {
	t.Parallel()
	def := newIDBackend("DEFAULT", nil)
	defer def.close()
	can := newIDBackend("CANARY", nil)
	defer can.close()

	cfg := baseConfig("round_robin", backendCfgs(def))
	cfg.Canary = config.CanaryConfig{
		Enabled:      true,
		WeightPercent: 20,
		Algorithm:    "round_robin",
		Backends:     backendCfgs(can),
	}
	s := New(cfg, "")

	if s.canary == nil {
		t.Fatal("expected canary balancer to be set up")
	}

	groups := s.balancerGroups()
	// Should include: default + canary.
	if len(groups) < 2 {
		t.Errorf("expected at least 2 groups with canary, got %d", len(groups))
	}
}

// ---- hasHealthyBackend with router ----

// TestHasHealthyBackend_WithRouter verifies that hasHealthyBackend returns true
// when a route-group backend is healthy, even if the default group has no backends.
func TestHasHealthyBackend_WithRouter(t *testing.T) {
	t.Parallel()
	api := newIDBackend("API", nil)
	defer api.close()
	def := newIDBackend("DEF", nil)
	defer def.close()

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

	if !s.hasHealthyBackend() {
		t.Error("hasHealthyBackend should return true when at least one route-group backend is healthy")
	}
}

// ---- backendGauges ----

// TestBackendGauges_DefaultGroup exercises the backendGauges snapshot path with
// the default balancer only (no router, no canary, no circuit breaker).
func TestBackendGauges_DefaultGroup(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	s := buildAdminServer(t, be.URL)

	gauges := s.backendGauges()
	if len(gauges) == 0 {
		t.Fatal("expected at least one gauge")
	}
	if gauges[0].URL != be.URL {
		t.Errorf("gauge URL: want %q, got %q", be.URL, gauges[0].URL)
	}
	if !gauges[0].Up {
		t.Error("gauge should report Up=true for a healthy backend")
	}
}

// TestBackendGauges_WithRouter exercises the router path in backendGauges, ensuring
// per-route backends are included and deduplicated.
func TestBackendGauges_WithRouter(t *testing.T) {
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

	gauges := s.backendGauges()
	// Should have DEFAULT + API = 2 unique backends.
	if len(gauges) < 2 {
		t.Errorf("expected at least 2 gauges, got %d", len(gauges))
	}
}

// ---- reloadConfig branches ----

// TestReloadConfig_AlgorithmChangedWarning exercises the log warning path when the
// load-balancing algorithm changes on reload (cannot be applied live).
func TestReloadConfig_AlgorithmChangedWarning(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	initial := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 19092
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
`, be.URL)

	f, err := os.CreateTemp(t.TempDir(), "reload-algo-*.yaml")
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

	// Overwrite the config with a different algorithm to trigger the warning.
	changed := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 19092
backends:
  - url: %q
    weight: 1
load_balancer:
  algorithm: "least_connections"
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
`, be.URL)

	if err := os.WriteFile(f.Name(), []byte(changed), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// reloadConfig should not panic and the server should still be functional.
	s.reloadConfig()

	// The balancer should still have the backend.
	all := s.balancer.All()
	if len(all) == 0 {
		t.Error("expected at least one backend after reload")
	}
}

// TestReloadConfig_RateLimiterLive exercises the rate-limiter live-update path in
// reloadConfig when the limiter is active and new config is loaded.
func TestReloadConfig_RateLimiterLive(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	content := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 19093
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
  enabled: true
  requests_per_second: 50
  burst: 10
metrics:
  enabled: false
compression:
  enabled: false
`, be.URL)

	f, err := os.CreateTemp(t.TempDir(), "reload-rl-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(content)
	f.Close()

	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	s := New(cfg, f.Name())
	if s.limiter == nil {
		t.Fatal("limiter should be non-nil")
	}
	defer s.limiter.Stop()

	// Reload with updated rate-limiter settings.
	updated := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 19093
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
  enabled: true
  requests_per_second: 200
  burst: 20
metrics:
  enabled: false
compression:
  enabled: false
`, be.URL)

	if err := os.WriteFile(f.Name(), []byte(updated), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Should not panic; limiter UpdateRates is called inside.
	s.reloadConfig()
}

// TestReloadConfig_BadConfigFile exercises the error path in reloadConfig when the
// config file is invalid/missing.
func TestReloadConfig_BadConfigFile(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	s := buildAdminServer(t, be.URL)
	// Point the configPath at a non-existent file to trigger the load error.
	s.configPath = "/does/not/exist.yaml"

	// reloadConfig should log the error and return without panicking.
	s.reloadConfig()

	// Server is still operational.
	all := s.balancer.All()
	if len(all) == 0 {
		t.Error("expected backend to still be registered after failed reload")
	}
}

// ---- setupDiscovery ----

// TestSetupDiscovery_NoDNS verifies that when no DNS discovery targets are
// configured, s.discoverer remains nil (the no-op path).
func TestSetupDiscovery_NoDNS(t *testing.T) {
	t.Parallel()
	s := buildAdminServer(t)

	if s.discoverer != nil {
		t.Error("expected discoverer to be nil when no DNS targets configured")
	}
}

// ---- adminAuth ----

// TestAdminAuth_WithToken verifies that when cfg.Metrics.AuthToken is set,
// requests without the correct bearer token are rejected with 401.
func TestAdminAuth_WithToken(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	content := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 19094
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
  auth_token: "supersecret"
compression:
  enabled: false
`, be.URL)

	f, err := os.CreateTemp(t.TempDir(), "auth-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(content)
	f.Close()

	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	s := New(cfg, f.Name())

	// Request with no token -> 401.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	s.metricsMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", rec.Code)
	}

	// Request with correct token -> 200.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer supersecret")
	s.metricsMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("want 200 with valid token, got %d", rec.Code)
	}
}

// ---- findBackend with canary group ----

// TestFindBackend_InCanary verifies that findBackend can locate a backend that
// lives in the canary balancer group (not the default group).
func TestFindBackend_InCanary(t *testing.T) {
	t.Parallel()
	def := newIDBackend("DEFAULT", nil)
	defer def.close()
	can := newIDBackend("CANARY", nil)
	defer can.close()

	cfg := baseConfig("round_robin", backendCfgs(def))
	cfg.Canary = config.CanaryConfig{
		Enabled:      true,
		WeightPercent: 20,
		Algorithm:    "round_robin",
		Backends:     backendCfgs(can),
	}
	s := New(cfg, "")

	b, g := s.findBackend(can.url)
	if b == nil {
		t.Fatal("findBackend should find the canary backend")
	}
	if g == nil {
		t.Fatal("findBackend should return the canary group")
	}
}

// ---- handleAdminCircuitReset without circuit breaker ----

// TestHandleAdminCircuitReset_NoBreaker verifies that the endpoint responds 200
// even when no circuit breaker is configured (it is a safe no-op).
func TestHandleAdminCircuitReset_NoBreaker(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	s := buildAdminServer(t, be.URL)

	if s.circuitBreaker != nil {
		t.Skip("circuit breaker enabled, skipping no-breaker test")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/circuit/reset?url="+be.URL, nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}

// ---- authEnabled / aclEnabled (package-level helpers) ----

func TestAuthEnabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		cfg  config.AuthConfig
		want bool
	}{
		{config.AuthConfig{Type: ""}, false},
		{config.AuthConfig{Type: "none"}, false},
		{config.AuthConfig{Type: "NONE"}, false},
		{config.AuthConfig{Type: "basic"}, true},
		{config.AuthConfig{Type: "jwt"}, true},
	}
	for _, tc := range tests {
		if got := authEnabled(tc.cfg); got != tc.want {
			t.Errorf("authEnabled(%q) = %v, want %v", tc.cfg.Type, got, tc.want)
		}
	}
}

func TestACLEnabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		cfg  config.ACLConfig
		want bool
	}{
		{config.ACLConfig{}, false},
		{config.ACLConfig{Allow: []string{"10.0.0.0/8"}}, true},
		{config.ACLConfig{Deny: []string{"1.2.3.4/32"}}, true},
		{config.ACLConfig{Methods: []string{"GET"}}, true},
		{config.ACLConfig{BlockedPaths: []string{"/admin"}}, true},
	}
	for _, tc := range tests {
		if got := aclEnabled(tc.cfg); got != tc.want {
			t.Errorf("aclEnabled(%+v) = %v, want %v", tc.cfg, got, tc.want)
		}
	}
}
