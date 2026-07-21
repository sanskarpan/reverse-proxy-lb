package server

// coverage_boost3_test.go targets the remaining low-coverage branches:
// - reloadConfig canary weight-update and topology-change paths
// - setupLimiter Redis-failure fallback
// - setupProxy with sticky sessions
// - setupHealthCheck with canary
// - hasHealthyBackend with canary group

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"reverse-proxy-lb/internal/config"
)

// writeServerConfig is a helper that writes a YAML server config to a temp file
// and returns its path and the loaded *config.Config.
func writeServerConfig(t *testing.T, yaml string) (string, *config.Config) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "srv-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(yaml); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return f.Name(), cfg
}

// ---- reloadConfig canary weight-update path ----

// TestReloadConfig_CanaryWeightUpdate exercises the "only weight changed" fast
// path in reloadConfig that calls proxy.UpdateCanaryWeight.
func TestReloadConfig_CanaryWeightUpdate(t *testing.T) {
	t.Parallel()
	def := newIDBackend("DEFAULT", nil)
	defer def.close()
	can := newIDBackend("CANARY", nil)
	defer can.close()

	initial := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 19100
backends:
  - url: %q
    weight: 1
load_balancer:
  algorithm: "round_robin"
  health_check:
    enabled: false
canary:
  enabled: true
  weight_percent: 10
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
`, def.url, can.url)

	path, cfg := writeServerConfig(t, initial)
	s := New(cfg, path)

	if s.canary == nil {
		t.Fatal("expected canary to be set up")
	}

	// Write a config with only the weight changed (same backends, same algorithm).
	updated := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 19100
backends:
  - url: %q
    weight: 1
load_balancer:
  algorithm: "round_robin"
  health_check:
    enabled: false
canary:
  enabled: true
  weight_percent: 30
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
`, def.url, can.url)

	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Should call proxy.UpdateCanaryWeight(30) internally.
	s.reloadConfig()
}

// TestReloadConfig_CanaryTopologyChange exercises the "topology changed"
// warning path in reloadConfig (different backends — requires restart).
func TestReloadConfig_CanaryTopologyChange(t *testing.T) {
	t.Parallel()
	def := newIDBackend("DEFAULT", nil)
	defer def.close()
	can1 := newIDBackend("CANARY1", nil)
	defer can1.close()
	can2 := newIDBackend("CANARY2", nil)
	defer can2.close()

	initial := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 19101
backends:
  - url: %q
    weight: 1
load_balancer:
  algorithm: "round_robin"
  health_check:
    enabled: false
canary:
  enabled: true
  weight_percent: 10
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
`, def.url, can1.url)

	path, cfg := writeServerConfig(t, initial)
	s := New(cfg, path)

	// Write a config with a different canary backend (topology change).
	updated := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 19101
backends:
  - url: %q
    weight: 1
load_balancer:
  algorithm: "round_robin"
  health_check:
    enabled: false
canary:
  enabled: true
  weight_percent: 10
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
`, def.url, can2.url)

	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Should log a warning about topology changes requiring restart.
	s.reloadConfig()
}

// ---- setupLimiter Redis failure -> fallback to MemStore ----

// TestSetupLimiter_RedisFailureFallback verifies that when Redis is configured
// but unreachable, the limiter falls back to an in-memory store gracefully.
func TestSetupLimiter_RedisFailureFallback(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	// Use a Redis address that is guaranteed to be unreachable.
	content := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 19102
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
  requests_per_second: 100
  burst: 10
  shared_store:
    enabled: true
    backend: "redis"
    key: "global"
    redis:
      addr: "127.0.0.1:19999"
metrics:
  enabled: false
compression:
  enabled: false
`, be.URL)

	f, err := os.CreateTemp(t.TempDir(), "limiter-redis-fail-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(content)
	f.Close()

	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	// New() calls setupLimiter which will attempt Redis, fail, and fall back to MemStore.
	// Should not panic.
	s := New(cfg, f.Name())
	if s.limiter == nil {
		t.Error("limiter should still be non-nil after Redis fallback")
	}
	s.limiter.Stop()
}

// ---- setupProxy with sticky sessions ----

// TestSetupProxy_StickySession exercises the sticky-session code path in setupProxy.
func TestSetupProxy_StickySession(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host: "127.0.0.1",
			Port: 19103,
		},
		Backends: []config.BackendConfig{
			{URL: be.URL, Weight: 1},
		},
		LoadBalancer: config.LoadBalancerConfig{
			Algorithm: "round_robin",
			Sticky: config.StickyConfig{
				Enabled: true,
				Cookie:  "rplb_affinity",
			},
		},
		Logging: config.LoggingConfig{Level: "error"},
	}

	s := New(cfg, "")
	if s.proxy == nil {
		t.Error("proxy should be non-nil")
	}
}

// ---- setupHealthCheck with canary ----

// TestSetupHealthCheck_WithCanary verifies that when canary is configured and
// health checking is enabled, the canary group gets its own HealthChecker.
func TestSetupHealthCheck_WithCanary(t *testing.T) {
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
	cfg.LoadBalancer.HealthCheck = config.HealthCheckConfig{
		Enabled:  true,
		Path:     "/healthz",
		Interval: 60_000_000_000,
		Timeout:  5_000_000_000,
	}
	s := New(cfg, "")
	t.Cleanup(func() {
		for _, hc := range s.healthChks {
			hc.Stop()
		}
	})

	// Expect: default + canary = at least 2 health checkers.
	if len(s.healthChks) < 2 {
		t.Errorf("expected at least 2 health checkers (default+canary), got %d", len(s.healthChks))
	}
}

// ---- hasHealthyBackend with canary group ----

// TestHasHealthyBackend_WithCanary verifies that hasHealthyBackend returns true
// when only the canary backend is healthy.
func TestHasHealthyBackend_WithCanary(t *testing.T) {
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

	// Mark default group backends unhealthy.
	for _, b := range s.balancer.All() {
		b.SetHealthy(false)
	}

	// Canary backends are still healthy (default is true).
	if !s.hasHealthyBackend() {
		t.Error("hasHealthyBackend should return true when canary backend is healthy")
	}
}

// ---- backendsUseTiers ----

func TestBackendsUseTiers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		backends []config.BackendConfig
		want     bool
	}{
		{
			name:     "no tiers",
			backends: []config.BackendConfig{{URL: "http://a:1"}, {URL: "http://b:2"}},
			want:     false,
		},
		{
			name:     "some with tier",
			backends: []config.BackendConfig{{URL: "http://a:1", Tier: 0}, {URL: "http://b:2", Tier: 1}},
			want:     true,
		},
		{
			name:     "all tier 0",
			backends: []config.BackendConfig{{URL: "http://a:1", Tier: 0}},
			want:     false,
		},
		{
			name:     "empty",
			backends: []config.BackendConfig{},
			want:     false,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := backendsUseTiers(tc.backends); got != tc.want {
				t.Errorf("backendsUseTiers = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---- reloadConfig mirror / fault-injection warning ----

// TestReloadConfig_MirrorChangeWarning exercises the mirror-config-changed warning.
func TestReloadConfig_MirrorChangeWarning(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	content := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 19104
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

	// Add a mirror config directly to s.cfg so that the reload diff sees a change.
	s.cfg.Mirror = config.MirrorConfig{Enabled: true, URL: "http://mirror:9999/"}

	// Reload from the original file (no mirror) — should log warning about mirror change.
	s.reloadConfig()
}
