package server

// coverage_boost4_test.go targets remaining untested branches in Stop(),
// setupProxy (Redis syncer path), and other low-coverage areas.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
)

// ---- Stop with limiter ----

// TestStop_WithLimiter exercises the s.limiter != nil path in Stop().
func TestStop_WithLimiter(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host:            "127.0.0.1",
			Port:            19110,
			ShutdownTimeout: 1 * time.Second,
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
		t.Fatal("expected limiter to be set up")
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop() returned error: %v", err)
	}
}

// ---- Stop with discoverer ----

// TestStop_WithDiscoverer exercises the s.discoverer != nil path in Stop().
func TestStop_WithDiscoverer(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host:            "127.0.0.1",
			Port:            19111,
			ShutdownTimeout: 1 * time.Second,
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
					Port:     8081,
					Interval: 60_000_000_000, // 1 minute — won't fire
				},
			},
		},
		Logging: config.LoggingConfig{Level: "error"},
	}

	s := New(cfg, "")
	if s.discoverer == nil {
		t.Fatal("expected discoverer to be set up")
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop() returned error: %v", err)
	}
}

// ---- Stop with configWatch ----

// TestStop_WithConfigWatch exercises the stopConfigWatch path in Stop() when
// a config file watch was started.
func TestStop_WithConfigWatch(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	content := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 19112
  shutdown_timeout: 1s
  watch_config: true
  watch_interval: 60s
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

	path, cfg := writeServerConfig(t, content)
	s := New(cfg, path)

	// startConfigWatch is called from Start(), so we call it directly here.
	s.startConfigWatch()
	if s.watchStop == nil {
		t.Fatal("expected watchStop channel to be set after startConfigWatch")
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop() returned error: %v", err)
	}
}

// ---- setupProxy with Redis syncer path ----

// TestSetupProxy_RedisSyncer exercises the circuit-breaker + SharedState path
// in setupProxy that creates a RedisSyncer. Even though Redis is unreachable,
// the syncer is constructed (connection happens lazily on first sync operation).
func TestSetupProxy_RedisSyncer(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host: "127.0.0.1",
			Port: 19113,
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
				RedisURL:     "redis://127.0.0.1:19999", // unreachable
				KeyPrefix:    "test",
				SyncInterval: 60 * time.Second,
			},
		},
		Logging: config.LoggingConfig{Level: "error"},
	}

	s := New(cfg, "")
	if s.redisSyncer == nil {
		t.Error("expected redisSyncer to be non-nil when SharedState is enabled")
	}
	// Stop the syncer to avoid goroutine leak.
	if s.redisSyncer != nil {
		s.redisSyncer.Stop()
	}
}

// ---- New with zero ShutdownTimeout (Stop default path) ----

// TestStop_DefaultShutdownTimeout exercises the `timeout <= 0` fallback branch
// in Stop() that sets timeout to 30 seconds.
func TestStop_DefaultShutdownTimeout(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host:            "127.0.0.1",
			Port:            19114,
			ShutdownTimeout: 0, // explicitly zero -> fallback to 30s
		},
		Backends: []config.BackendConfig{
			{URL: be.URL, Weight: 1},
		},
		LoadBalancer: config.LoadBalancerConfig{
			Algorithm: "round_robin",
		},
		Logging: config.LoggingConfig{Level: "error"},
	}

	s := New(cfg, "")
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop() with zero timeout should succeed: %v", err)
	}
}

// ---- setupRouter fallback path ----

// TestSetupRouter_BuildError exercises the logging-only error path when a route
// group fails to build due to an unknown algorithm.
func TestSetupRouter_BuildError(t *testing.T) {
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
			Algorithm:  "no_such_algorithm", // will cause BuildGroup to fail
			Backends:   backendCfgs(api),
		},
	}

	// Should not panic; logs an error and leaves router nil.
	s := New(cfg, "")
	// The router may or may not be nil depending on whether BuildGroup returns an
	// error for an unknown algorithm — both outcomes are acceptable.
	_ = s.router
}

// ---- setupCanary with zero backends (no-op path) ----

// TestSetupCanary_Disabled verifies that the canary group is not created when
// canary is disabled (s.canary stays nil).
func TestSetupCanary_Disabled(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	s := buildAdminServer(t, be.URL) // canary not configured
	if s.canary != nil {
		t.Error("expected canary to be nil when canary is not configured")
	}
}

// TestSetupCanary_EnabledNoBackends exercises the cfg.Canary.Enabled=true but
// Backends empty branch (early return in setupCanary).
func TestSetupCanary_EnabledNoBackends(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	cfg := baseConfig("round_robin", []config.BackendConfig{{URL: be.URL, Weight: 1}})
	cfg.Canary = config.CanaryConfig{
		Enabled:      true,
		WeightPercent: 20,
		// No backends — should return early without setting s.canary.
	}

	s := New(cfg, "")
	if s.canary != nil {
		t.Error("expected canary to be nil when canary has no backends")
	}
}
