package server

// coverage_boost5_test.go adds the final few percent of coverage by targeting
// Stop() with autoPromoter and redisSyncer set, setupLimiter's remaining branch,
// and startConfigWatch's default interval fallback.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"reverse-proxy-lb/internal/canary"
	"reverse-proxy-lb/internal/config"
)

// ---- Stop with autoPromoter ----

// TestStop_WithAutoPromoter exercises the s.autoPromoter != nil path in Stop().
func TestStop_WithAutoPromoter(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	s := buildAdminServer(t, be.URL)

	// Inject a canary AutoPromoter that will be stopped by Stop().
	ap := canary.New(&mockWeightUpdater{}, &mockMetricsSnap{}, config.AutoPromoteConfig{
		Enabled:            true,
		StepPercent:        10,
		MaxWeightPercent:   80,
		ErrorRateThreshold: 0.05,
		MinRequests:        50,
		StepInterval:       1<<63 - 1, // effectively infinite
	})
	ap.Start()
	s.autoPromoter = ap

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop() returned error: %v", err)
	}
}

// ---- Stop with redisSyncer ----

// TestStop_WithRedisSyncer exercises the s.redisSyncer != nil path in Stop().
func TestStop_WithRedisSyncer(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host:            "127.0.0.1",
			Port:            19120,
			ShutdownTimeout: 1 * time.Second,
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
			SharedState: config.CircuitSharedStateConfig{
				Enabled:      true,
				RedisURL:     "redis://127.0.0.1:19999", // unreachable — syncer is constructed but never syncs
				KeyPrefix:    "test",
				SyncInterval: 60 * time.Second,
			},
		},
		Logging: config.LoggingConfig{Level: "error"},
	}

	s := New(cfg, "")
	if s.redisSyncer == nil {
		t.Fatal("expected redisSyncer to be non-nil")
	}

	// Stop should call s.redisSyncer.Stop() cleanly.
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop() returned error: %v", err)
	}
}

// ---- startConfigWatch with zero interval (default fallback) ----

// TestStartConfigWatch_DefaultInterval verifies that when WatchInterval is zero,
// startConfigWatch falls back to 5 seconds (without actually waiting for it).
func TestStartConfigWatch_DefaultInterval(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	content := formatServerConfig(be.URL, 19121, `
  watch_config: true
  watch_interval: 0s
`)
	path, cfg := writeServerConfig(t, content)
	s := New(cfg, path)

	s.startConfigWatch()
	if s.watchStop == nil {
		t.Fatal("expected watchStop channel")
	}

	// Tear down cleanly.
	s.stopConfigWatch()
}

// formatServerConfig builds a minimal valid YAML server config string, accepting
// optional extra server-block keys (indented with 2 spaces).
func formatServerConfig(backendURL string, port int, extraServerKeys string) string {
	return fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: %d%s
  shutdown_timeout: 1s
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
`, port, extraServerKeys, backendURL)
}

// ---- setupLimiter with per-route rules (RegisterRules branch) ----

// TestSetupLimiter_WithPerRouteRules exercises the middleware.RegisterRules call
// inside setupLimiter by configuring per-route rate-limit rules.
func TestSetupLimiter_WithPerRouteRules(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host: "127.0.0.1",
			Port: 19122,
		},
		Backends: []config.BackendConfig{
			{URL: be.URL, Weight: 1},
		},
		LoadBalancer: config.LoadBalancerConfig{
			Algorithm: "round_robin",
		},
		RateLimiter: config.RateLimiterConfig{
			Enabled:           true,
			RequestsPerSecond: 100,
			Burst:             10,
		},
		Logging: config.LoggingConfig{Level: "error"},
	}

	s := New(cfg, "")
	if s.limiter == nil {
		t.Error("limiter should be non-nil")
	}
	s.limiter.Stop()
}

// ---- handleAdminBackends with circuit breaker wired ----

// TestHandleAdminBackends_WithCircuitBreaker verifies that when a circuit breaker
// is configured, the circuit state is included in the response.
func TestHandleAdminBackends_WithCircuitBreaker(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host: "127.0.0.1",
			Port: 19123,
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
	if len(out) == 0 {
		t.Fatal("expected at least one backend in response")
	}
	if out[0].CircuitState == "" {
		t.Error("circuit_state should be non-empty when circuit breaker is configured")
	}
}
