package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	content := `
server:
  host: "0.0.0.0"
  port: 8080
  read_timeout: 30s
  write_timeout: 30s
  idle_timeout: 120s

backends:
  - url: "http://localhost:8001"
    weight: 1
    max_conns: 100
  - url: "http://localhost:8002"
    weight: 2
    max_conns: 50

load_balancer:
  algorithm: "round_robin"
  health_check:
    enabled: true
    interval: 10s
    timeout: 5s
    path: "/health"

circuit_breaker:
  enabled: true
  failure_threshold: 5
  success_threshold: 2
  timeout: 30s

rate_limiter:
  enabled: true
  requests_per_second: 100
  burst: 200

retry:
  max_attempts: 3
  backoff: "exponential"
  max_backoff: 10s

logging:
  level: "info"
  format: "json"

metrics:
  enabled: true
  port: 9090
`
	tmpFile, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(content); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	cfg, err := Load(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if cfg.Server.Port != 8080 {
		t.Errorf("Expected port 8080, got %d", cfg.Server.Port)
	}

	if len(cfg.Backends) != 2 {
		t.Errorf("Expected 2 backends, got %d", len(cfg.Backends))
	}

	if cfg.Backends[0].Weight != 1 {
		t.Errorf("Expected weight 1, got %d", cfg.Backends[0].Weight)
	}

	if cfg.LoadBalancer.Algorithm != "round_robin" {
		t.Errorf("Expected round_robin algorithm, got %s", cfg.LoadBalancer.Algorithm)
	}

	if !cfg.CircuitBreaker.Enabled {
		t.Error("Expected circuit breaker to be enabled")
	}

	if !cfg.RateLimiter.Enabled {
		t.Error("Expected rate limiter to be enabled")
	}

	if cfg.RateLimiter.RequestsPerSecond != 100 {
		t.Errorf("Expected 100 RPS, got %d", cfg.RateLimiter.RequestsPerSecond)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	content := `
server:
  host: "0.0.0.0"
backends:
  - url: "http://localhost:8001"
`
	tmpFile, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(content); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	cfg, err := Load(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if cfg.Server.Port != 8080 {
		t.Errorf("Expected default port 8080, got %d", cfg.Server.Port)
	}

	if cfg.Backends[0].Weight != 1 {
		t.Errorf("Expected default weight 1, got %d", cfg.Backends[0].Weight)
	}

	if cfg.Backends[0].MaxConns != 100 {
		t.Errorf("Expected default max_conns 100, got %d", cfg.Backends[0].MaxConns)
	}

	if cfg.Metrics.Host != "127.0.0.1" {
		t.Errorf("Expected default metrics host 127.0.0.1, got %q", cfg.Metrics.Host)
	}
}

func TestGetAddr(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Host: "0.0.0.0",
			Port: 8080,
		},
	}

	addr := cfg.GetAddr()
	expected := "0.0.0.0:8080"
	if addr != expected {
		t.Errorf("Expected addr %s, got %s", expected, addr)
	}
}

func TestHealthCheckConfig(t *testing.T) {
	cfg := &Config{
		LoadBalancer: LoadBalancerConfig{
			HealthCheck: HealthCheckConfig{
				Enabled:  true,
				Interval: 10 * time.Second,
				Timeout:  5 * time.Second,
				Path:     "/health",
			},
		},
	}

	if !cfg.LoadBalancer.HealthCheck.Enabled {
		t.Error("Expected health check to be enabled")
	}

	if cfg.LoadBalancer.HealthCheck.Interval != 10*time.Second {
		t.Errorf("Expected interval 10s, got %v", cfg.LoadBalancer.HealthCheck.Interval)
	}

	if cfg.LoadBalancer.HealthCheck.Path != "/health" {
		t.Errorf("Expected path /health, got %s", cfg.LoadBalancer.HealthCheck.Path)
	}
}

// ID 14: invalid configurations must be rejected instead of silently misbehaving.
func TestValidateRejectsBadConfigs(t *testing.T) {
	write := func(content string) (string, func()) {
		f, err := os.CreateTemp("", "cfg-*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(content)
		f.Close()
		return f.Name(), func() { os.Remove(f.Name()) }
	}

	cases := map[string]string{
		"no backends": `
server:
  host: "0.0.0.0"
`,
		"unknown algorithm": `
backends:
  - url: "http://localhost:8001"
load_balancer:
  algorithm: "magic"
`,
		"bad backend url": `
backends:
  - url: "not-a-url"
`,
		"rate limiter zero rps": `
backends:
  - url: "http://localhost:8001"
rate_limiter:
  enabled: true
  requests_per_second: 0
`,
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path, cleanup := write(content)
			defer cleanup()
			if _, err := Load(path); err == nil {
				t.Errorf("expected validation error for %q, got nil", name)
			}
		})
	}
}

// A minimal valid config with an empty algorithm must default to round_robin and load.
func TestValidateDefaultsAlgorithm(t *testing.T) {
	f, _ := os.CreateTemp("", "cfg-*.yaml")
	f.WriteString("backends:\n  - url: \"http://localhost:8001\"\n")
	f.Close()
	defer os.Remove(f.Name())

	cfg, err := Load(f.Name())
	if err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}
	if cfg.LoadBalancer.Algorithm != "round_robin" {
		t.Errorf("expected default algorithm round_robin, got %q", cfg.LoadBalancer.Algorithm)
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", "cfg-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

// Env override: RPLB_SERVER_PORT must replace the port loaded from the file.
func TestEnvOverrideServerPort(t *testing.T) {
	path := writeConfig(t, `
server:
  host: "0.0.0.0"
  port: 8080
backends:
  - url: "http://localhost:8001"
`)

	t.Setenv("RPLB_SERVER_PORT", "9999")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 9999 {
		t.Errorf("expected port 9999 from env override, got %d", cfg.Server.Port)
	}
}

// Env override: RPLB_BACKENDS must replace the backends list.
func TestEnvOverrideBackends(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
`)

	t.Setenv("RPLB_BACKENDS", "http://a:1000, http://b:2000")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Backends) != 2 {
		t.Fatalf("expected 2 backends from env override, got %d", len(cfg.Backends))
	}
	if cfg.Backends[0].URL != "http://a:1000" || cfg.Backends[1].URL != "http://b:2000" {
		t.Errorf("unexpected backend urls: %+v", cfg.Backends)
	}
	if cfg.Backends[0].Weight != 1 || cfg.Backends[0].MaxConns != 100 {
		t.Errorf("expected defaulted weight/max_conns, got weight=%d max_conns=%d",
			cfg.Backends[0].Weight, cfg.Backends[0].MaxConns)
	}
}

// New load-balancing algorithm names must validate successfully.
func TestValidateNewAlgorithms(t *testing.T) {
	algos := []string{"swrr", "p2c", "weighted_least_conn", "weighted_random", "consistent_hash", "ewma"}
	for _, algo := range algos {
		t.Run(algo, func(t *testing.T) {
			path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
load_balancer:
  algorithm: "`+algo+`"
`)
			if _, err := Load(path); err != nil {
				t.Errorf("expected algorithm %q to validate, got %v", algo, err)
			}
		})
	}
}

// Consistent-hash, sticky, and slow-start defaults must be applied by Load().
func TestConsistentHashAndStickyDefaults(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
load_balancer:
  algorithm: "consistent_hash"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LoadBalancer.ConsistentHash.Replicas != 100 {
		t.Errorf("expected default replicas 100, got %d", cfg.LoadBalancer.ConsistentHash.Replicas)
	}
	if cfg.LoadBalancer.ConsistentHash.LoadFactor != 1.25 {
		t.Errorf("expected default load_factor 1.25, got %v", cfg.LoadBalancer.ConsistentHash.LoadFactor)
	}
	if cfg.LoadBalancer.Sticky.Cookie != "rplb_affinity" {
		t.Errorf("expected default sticky cookie rplb_affinity, got %q", cfg.LoadBalancer.Sticky.Cookie)
	}
}

// Explicit consistent-hash / sticky values must survive defaulting.
func TestConsistentHashExplicitValues(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
load_balancer:
  algorithm: "consistent_hash"
  consistent_hash:
    replicas: 256
    load_factor: 2.0
  sticky:
    enabled: true
    cookie: "myaff"
    ttl: 1h
  slow_start: 30s
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LoadBalancer.ConsistentHash.Replicas != 256 {
		t.Errorf("expected replicas 256, got %d", cfg.LoadBalancer.ConsistentHash.Replicas)
	}
	if cfg.LoadBalancer.ConsistentHash.LoadFactor != 2.0 {
		t.Errorf("expected load_factor 2.0, got %v", cfg.LoadBalancer.ConsistentHash.LoadFactor)
	}
	if !cfg.LoadBalancer.Sticky.Enabled || cfg.LoadBalancer.Sticky.Cookie != "myaff" {
		t.Errorf("unexpected sticky config: %+v", cfg.LoadBalancer.Sticky)
	}
	if cfg.LoadBalancer.Sticky.TTL != time.Hour {
		t.Errorf("expected sticky ttl 1h, got %v", cfg.LoadBalancer.Sticky.TTL)
	}
	if cfg.LoadBalancer.SlowStart != 30*time.Second {
		t.Errorf("expected slow_start 30s, got %v", cfg.LoadBalancer.SlowStart)
	}
}

// Backend/server tier and zone fields must parse and prefer_same_zone must load.
func TestTierAndZoneParse(t *testing.T) {
	path := writeConfig(t, `
server:
  zone: "us-east-1a"
backends:
  - url: "http://localhost:8001"
    zone: "us-east-1a"
    tier: 0
  - url: "http://localhost:8002"
    zone: "us-east-1b"
    tier: 1
load_balancer:
  prefer_same_zone: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Zone != "us-east-1a" {
		t.Errorf("expected server zone us-east-1a, got %q", cfg.Server.Zone)
	}
	if !cfg.LoadBalancer.PreferSameZone {
		t.Error("expected prefer_same_zone true")
	}
	if cfg.Backends[0].Zone != "us-east-1a" || cfg.Backends[0].Tier != 0 {
		t.Errorf("unexpected backend[0]: zone=%q tier=%d", cfg.Backends[0].Zone, cfg.Backends[0].Tier)
	}
	if cfg.Backends[1].Zone != "us-east-1b" || cfg.Backends[1].Tier != 1 {
		t.Errorf("unexpected backend[1]: zone=%q tier=%d", cfg.Backends[1].Zone, cfg.Backends[1].Tier)
	}
}

// Outlier detection with a valid config must load and parse fields.
func TestOutlierDetectionParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
load_balancer:
  outlier_detection:
    enabled: true
    error_rate_threshold: 0.5
    min_requests: 20
    base_ejection: 30s
    max_ejection_percent: 50
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	od := cfg.LoadBalancer.OutlierDetection
	if !od.Enabled || od.ErrorRateThreshold != 0.5 || od.MinRequests != 20 ||
		od.BaseEjection != 30*time.Second || od.MaxEjectionPercent != 50 {
		t.Errorf("unexpected outlier_detection config: %+v", od)
	}
}

// Invalid MaxEjectionPercent (and other outlier/consistent-hash bounds) must be rejected.
func TestValidateRejectsBadLBConfigs(t *testing.T) {
	cases := map[string]string{
		"max_ejection_percent too high": `
backends:
  - url: "http://localhost:8001"
load_balancer:
  outlier_detection:
    enabled: true
    max_ejection_percent: 150
`,
		"error_rate_threshold out of range": `
backends:
  - url: "http://localhost:8001"
load_balancer:
  outlier_detection:
    enabled: true
    error_rate_threshold: 2.0
    max_ejection_percent: 50
`,
		"load_factor below one": `
backends:
  - url: "http://localhost:8001"
load_balancer:
  consistent_hash:
    load_factor: 0.5
`,
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, content)
			if _, err := Load(path); err == nil {
				t.Errorf("expected validation error for %q, got nil", name)
			}
		})
	}
}

// New health-check fields must parse from YAML.
func TestHealthCheckNewFieldsParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
load_balancer:
  health_check:
    enabled: true
    interval: 10s
    timeout: 5s
    path: "/healthz"
    type: "http"
    method: "HEAD"
    expected_statuses: [200, 204]
    expected_body: "ok"
    host: "svc.internal"
    headers:
      X-Probe: "yes"
    healthy_threshold: 5
    unhealthy_threshold: 4
    jitter: 0.25
    startup_grace_period: 15s
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	hc := cfg.LoadBalancer.HealthCheck
	if hc.Type != "http" || hc.Method != "HEAD" {
		t.Errorf("unexpected type/method: %q/%q", hc.Type, hc.Method)
	}
	if len(hc.ExpectedStatuses) != 2 || hc.ExpectedStatuses[0] != 200 || hc.ExpectedStatuses[1] != 204 {
		t.Errorf("unexpected expected_statuses: %v", hc.ExpectedStatuses)
	}
	if hc.ExpectedBody != "ok" {
		t.Errorf("unexpected expected_body: %q", hc.ExpectedBody)
	}
	if hc.Host != "svc.internal" {
		t.Errorf("unexpected host: %q", hc.Host)
	}
	if hc.Headers["X-Probe"] != "yes" {
		t.Errorf("unexpected headers: %v", hc.Headers)
	}
	if hc.HealthyThreshold != 5 || hc.UnhealthyThreshold != 4 {
		t.Errorf("unexpected thresholds: %d/%d", hc.HealthyThreshold, hc.UnhealthyThreshold)
	}
	if hc.Jitter != 0.25 {
		t.Errorf("unexpected jitter: %v", hc.Jitter)
	}
	if hc.StartupGracePeriod != 15*time.Second {
		t.Errorf("unexpected startup_grace_period: %v", hc.StartupGracePeriod)
	}
}

// Health-check per-field defaults must be applied by Load().
func TestHealthCheckDefaults(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
load_balancer:
  health_check:
    enabled: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	hc := cfg.LoadBalancer.HealthCheck
	if hc.Type != "http" {
		t.Errorf("expected default type http, got %q", hc.Type)
	}
	if hc.Method != "GET" {
		t.Errorf("expected default method GET, got %q", hc.Method)
	}
	if hc.HealthyThreshold != 2 {
		t.Errorf("expected default healthy_threshold 2, got %d", hc.HealthyThreshold)
	}
	if hc.UnhealthyThreshold != 3 {
		t.Errorf("expected default unhealthy_threshold 3, got %d", hc.UnhealthyThreshold)
	}
	if hc.Jitter != 0.1 {
		t.Errorf("expected default jitter 0.1, got %v", hc.Jitter)
	}
}

// A per-backend health_check override must parse and be defaulted.
func TestPerBackendHealthCheckOverrideDefaults(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
    health_check:
      enabled: true
      path: "/ping"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	hc := cfg.Backends[0].HealthCheck
	if hc == nil {
		t.Fatal("expected non-nil per-backend health_check")
	}
	if hc.Path != "/ping" {
		t.Errorf("expected path /ping, got %q", hc.Path)
	}
	if hc.Type != "http" || hc.Method != "GET" {
		t.Errorf("expected defaulted type/method http/GET, got %q/%q", hc.Type, hc.Method)
	}
	if hc.HealthyThreshold != 2 || hc.UnhealthyThreshold != 3 {
		t.Errorf("expected defaulted thresholds 2/3, got %d/%d", hc.HealthyThreshold, hc.UnhealthyThreshold)
	}
	if hc.Jitter != 0.1 {
		t.Errorf("expected defaulted jitter 0.1, got %v", hc.Jitter)
	}
}

// Backends without an override must leave HealthCheck nil.
func TestPerBackendHealthCheckNilWhenAbsent(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Backends[0].HealthCheck != nil {
		t.Errorf("expected nil per-backend health_check, got %+v", cfg.Backends[0].HealthCheck)
	}
}

// Invalid health-check configs must be rejected.
func TestValidateRejectsBadHealthCheck(t *testing.T) {
	cases := map[string]string{
		"jitter above one": `
backends:
  - url: "http://localhost:8001"
load_balancer:
  health_check:
    jitter: 1.5
`,
		"invalid type": `
backends:
  - url: "http://localhost:8001"
load_balancer:
  health_check:
    type: "udp"
`,
		"per-backend invalid type": `
backends:
  - url: "http://localhost:8001"
    health_check:
      type: "grpc"
`,
		"expected status out of range": `
backends:
  - url: "http://localhost:8001"
load_balancer:
  health_check:
    expected_statuses: [999]
`,
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, content)
			if _, err := Load(path); err == nil {
				t.Errorf("expected validation error for %q, got nil", name)
			}
		})
	}
}

// New circuit-breaker and retry fields must parse from YAML.
func TestCircuitBreakerAndRetryNewFieldsParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
circuit_breaker:
  enabled: true
  mode: "rolling"
  rolling_window: 5s
  error_rate_threshold: 0.75
  min_requests: 50
  trip_on: ["connect", "5xx"]
retry:
  max_attempts: 3
  budget: 0.2
  per_try_timeout: 2s
  honor_retry_after: false
  retry_on: ["connect"]
  hedge:
    enabled: true
    delay: 100ms
    max_extra: 3
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cb := cfg.CircuitBreaker
	if cb.Mode != "rolling" {
		t.Errorf("expected mode rolling, got %q", cb.Mode)
	}
	if cb.RollingWindow != 5*time.Second {
		t.Errorf("expected rolling_window 5s, got %v", cb.RollingWindow)
	}
	if cb.ErrorRateThreshold != 0.75 {
		t.Errorf("expected error_rate_threshold 0.75, got %v", cb.ErrorRateThreshold)
	}
	if cb.MinRequests != 50 {
		t.Errorf("expected min_requests 50, got %d", cb.MinRequests)
	}
	if len(cb.TripOn) != 2 || cb.TripOn[0] != "connect" || cb.TripOn[1] != "5xx" {
		t.Errorf("unexpected trip_on: %v", cb.TripOn)
	}
	r := cfg.Retry
	if r.Budget != 0.2 {
		t.Errorf("expected budget 0.2, got %v", r.Budget)
	}
	if r.PerTryTimeout != 2*time.Second {
		t.Errorf("expected per_try_timeout 2s, got %v", r.PerTryTimeout)
	}
	// honor_retry_after: false alongside other retry fields must be respected.
	if r.HonorRetryAfter {
		t.Error("expected honor_retry_after false to be respected")
	}
	if len(r.RetryOn) != 1 || r.RetryOn[0] != "connect" {
		t.Errorf("unexpected retry_on: %v", r.RetryOn)
	}
	if !r.Hedge.Enabled || r.Hedge.Delay != 100*time.Millisecond || r.Hedge.MaxExtra != 3 {
		t.Errorf("unexpected hedge: %+v", r.Hedge)
	}
}

// Circuit-breaker and retry defaults must be applied by Load().
func TestCircuitBreakerAndRetryDefaults(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cb := cfg.CircuitBreaker
	if cb.Mode != "consecutive" {
		t.Errorf("expected default mode consecutive, got %q", cb.Mode)
	}
	if cb.RollingWindow != 10*time.Second {
		t.Errorf("expected default rolling_window 10s, got %v", cb.RollingWindow)
	}
	if cb.ErrorRateThreshold != 0.5 {
		t.Errorf("expected default error_rate_threshold 0.5, got %v", cb.ErrorRateThreshold)
	}
	if cb.MinRequests != 20 {
		t.Errorf("expected default min_requests 20, got %d", cb.MinRequests)
	}
	if len(cb.TripOn) != 2 || cb.TripOn[0] != "connect" || cb.TripOn[1] != "timeout" {
		t.Errorf("expected default trip_on [connect timeout], got %v", cb.TripOn)
	}
	r := cfg.Retry
	// A fully-omitted retry block must default HonorRetryAfter to true.
	if !r.HonorRetryAfter {
		t.Error("expected default honor_retry_after true for omitted retry block")
	}
	if len(r.RetryOn) != 2 || r.RetryOn[0] != "connect" || r.RetryOn[1] != "timeout" {
		t.Errorf("expected default retry_on [connect timeout], got %v", r.RetryOn)
	}
}

// Hedge.MaxExtra must default to 1 when hedging is enabled and left unset.
func TestHedgeMaxExtraDefault(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
retry:
  hedge:
    enabled: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Retry.Hedge.Enabled {
		t.Fatal("expected hedge enabled")
	}
	if cfg.Retry.Hedge.MaxExtra != 1 {
		t.Errorf("expected default hedge max_extra 1, got %d", cfg.Retry.Hedge.MaxExtra)
	}
}

// Invalid circuit-breaker / retry configs must be rejected.
func TestValidateRejectsBadCircuitAndRetryConfigs(t *testing.T) {
	cases := map[string]string{
		"invalid circuit mode": `
backends:
  - url: "http://localhost:8001"
circuit_breaker:
  mode: "magic"
`,
		"error_rate_threshold above one": `
backends:
  - url: "http://localhost:8001"
circuit_breaker:
  error_rate_threshold: 1.5
`,
		"unknown trip_on class": `
backends:
  - url: "http://localhost:8001"
circuit_breaker:
  trip_on: ["connect", "bogus"]
`,
		"unknown retry_on class": `
backends:
  - url: "http://localhost:8001"
retry:
  retry_on: ["5xx"]
`,
		"negative retry budget": `
backends:
  - url: "http://localhost:8001"
retry:
  budget: -0.1
`,
		"negative hedge delay": `
backends:
  - url: "http://localhost:8001"
retry:
  hedge:
    enabled: true
    delay: -1s
`,
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, content)
			if _, err := Load(path); err == nil {
				t.Errorf("expected validation error for %q, got nil", name)
			}
		})
	}
}

// New rate-limiter fields (algorithm, global rps/burst, key, retry-after,
// message, allowlist, rules) must parse from YAML.
func TestRateLimiterNewFieldsParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
rate_limiter:
  enabled: true
  requests_per_second: 100
  burst: 200
  algorithm: "gcra"
  global_rps: 1000
  global_burst: 2000
  key: "header:X-Api-Key"
  retry_after_seconds: 5
  message: "slow down"
  allowlist:
    - "10.0.0.0/8"
    - "192.168.1.1"
  rules:
    - path_prefix: "/api"
      method: "POST"
      rps: 10
      burst: 20
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rl := cfg.RateLimiter
	if rl.Algorithm != "gcra" {
		t.Errorf("expected algorithm gcra, got %q", rl.Algorithm)
	}
	if rl.GlobalRPS != 1000 || rl.GlobalBurst != 2000 {
		t.Errorf("unexpected global rps/burst: %d/%d", rl.GlobalRPS, rl.GlobalBurst)
	}
	if rl.Key != "header:X-Api-Key" {
		t.Errorf("unexpected key: %q", rl.Key)
	}
	if rl.RetryAfterSeconds != 5 {
		t.Errorf("expected retry_after_seconds 5, got %d", rl.RetryAfterSeconds)
	}
	if rl.Message != "slow down" {
		t.Errorf("unexpected message: %q", rl.Message)
	}
	if len(rl.Allowlist) != 2 || rl.Allowlist[0] != "10.0.0.0/8" || rl.Allowlist[1] != "192.168.1.1" {
		t.Errorf("unexpected allowlist: %v", rl.Allowlist)
	}
	if len(rl.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rl.Rules))
	}
	rule := rl.Rules[0]
	if rule.PathPrefix != "/api" || rule.Method != "POST" || rule.RPS != 10 || rule.Burst != 20 {
		t.Errorf("unexpected rule: %+v", rule)
	}
}

// Rate-limiter defaults must be applied by Load(): algorithm token_bucket, key
// ip, retry_after 1, default message, and global rps/burst falling back to the
// per-key requests_per_second/burst.
func TestRateLimiterDefaults(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
rate_limiter:
  enabled: true
  requests_per_second: 100
  burst: 200
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rl := cfg.RateLimiter
	if rl.Algorithm != "token_bucket" {
		t.Errorf("expected default algorithm token_bucket, got %q", rl.Algorithm)
	}
	if rl.Key != "ip" {
		t.Errorf("expected default key ip, got %q", rl.Key)
	}
	if rl.RetryAfterSeconds != 1 {
		t.Errorf("expected default retry_after_seconds 1, got %d", rl.RetryAfterSeconds)
	}
	if rl.Message != "Rate limit exceeded" {
		t.Errorf("expected default message, got %q", rl.Message)
	}
	if rl.GlobalRPS != 100 {
		t.Errorf("expected global_rps to fall back to 100, got %d", rl.GlobalRPS)
	}
	if rl.GlobalBurst != 200 {
		t.Errorf("expected global_burst to fall back to 200, got %d", rl.GlobalBurst)
	}
}

// Invalid rate-limiter configs must be rejected.
func TestValidateRejectsBadRateLimiterConfigs(t *testing.T) {
	cases := map[string]string{
		"invalid algorithm": `
backends:
  - url: "http://localhost:8001"
rate_limiter:
  enabled: true
  requests_per_second: 10
  algorithm: "leaky"
`,
		"bad allowlist cidr": `
backends:
  - url: "http://localhost:8001"
rate_limiter:
  allowlist:
    - "not-an-ip"
`,
		"bad key": `
backends:
  - url: "http://localhost:8001"
rate_limiter:
  key: "cookie:session"
`,
		"empty header key": `
backends:
  - url: "http://localhost:8001"
rate_limiter:
  key: "header:"
`,
		"rule missing rps": `
backends:
  - url: "http://localhost:8001"
rate_limiter:
  enabled: true
  requests_per_second: 10
  rules:
    - path_prefix: "/api"
      burst: 5
`,
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, content)
			if _, err := Load(path); err == nil {
				t.Errorf("expected validation error for %q, got nil", name)
			}
		})
	}
}

// Upstream, L4, and WebSocket fields must parse from YAML.
func TestServerUpstreamL4WebSocketParse(t *testing.T) {
	path := writeConfig(t, `
server:
  upstream:
    dial_timeout: 2s
    tls_handshake_timeout: 3s
    response_header_timeout: 15s
    expect_continue_timeout: 500ms
    idle_conn_timeout: 45s
    max_idle_conns: 200
    max_idle_conns_per_host: 50
    max_conns_per_host: 25
    http2: true
  l4:
    enabled: true
    port: 9000
    dial_timeout: 4s
  websocket:
    idle_timeout: 60s
    max_message_bytes: 1048576
backends:
  - url: "http://localhost:8001"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	u := cfg.Server.Upstream
	if u.DialTimeout != 2*time.Second {
		t.Errorf("expected dial_timeout 2s, got %v", u.DialTimeout)
	}
	if u.TLSHandshakeTimeout != 3*time.Second {
		t.Errorf("expected tls_handshake_timeout 3s, got %v", u.TLSHandshakeTimeout)
	}
	if u.ResponseHeaderTimeout != 15*time.Second {
		t.Errorf("expected response_header_timeout 15s, got %v", u.ResponseHeaderTimeout)
	}
	if u.ExpectContinueTimeout != 500*time.Millisecond {
		t.Errorf("expected expect_continue_timeout 500ms, got %v", u.ExpectContinueTimeout)
	}
	if u.IdleConnTimeout != 45*time.Second {
		t.Errorf("expected idle_conn_timeout 45s, got %v", u.IdleConnTimeout)
	}
	if u.MaxIdleConns != 200 || u.MaxIdleConnsPerHost != 50 || u.MaxConnsPerHost != 25 {
		t.Errorf("unexpected conn caps: %d/%d/%d", u.MaxIdleConns, u.MaxIdleConnsPerHost, u.MaxConnsPerHost)
	}
	if !u.HTTP2 {
		t.Error("expected http2 true")
	}
	l := cfg.Server.L4
	if !l.Enabled || l.Port != 9000 || l.DialTimeout != 4*time.Second {
		t.Errorf("unexpected l4 config: %+v", l)
	}
	w := cfg.Server.WebSocket
	if w.IdleTimeout != 60*time.Second || w.MaxMessageBytes != 1048576 {
		t.Errorf("unexpected websocket config: %+v", w)
	}
}

// Upstream defaults must be applied by Load() when the block is omitted, and an
// explicit smaller value must be respected. HTTP2 defaults to false and
// MaxConnsPerHost to 0 (unlimited).
func TestServerUpstreamDefaults(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	u := cfg.Server.Upstream
	if u.DialTimeout != 5*time.Second {
		t.Errorf("expected default dial_timeout 5s, got %v", u.DialTimeout)
	}
	if u.TLSHandshakeTimeout != 5*time.Second {
		t.Errorf("expected default tls_handshake_timeout 5s, got %v", u.TLSHandshakeTimeout)
	}
	if u.ResponseHeaderTimeout != 30*time.Second {
		t.Errorf("expected default response_header_timeout 30s, got %v", u.ResponseHeaderTimeout)
	}
	if u.ExpectContinueTimeout != 1*time.Second {
		t.Errorf("expected default expect_continue_timeout 1s, got %v", u.ExpectContinueTimeout)
	}
	if u.IdleConnTimeout != 90*time.Second {
		t.Errorf("expected default idle_conn_timeout 90s, got %v", u.IdleConnTimeout)
	}
	if u.MaxIdleConns != 100 {
		t.Errorf("expected default max_idle_conns 100, got %d", u.MaxIdleConns)
	}
	if u.MaxIdleConnsPerHost != 100 {
		t.Errorf("expected default max_idle_conns_per_host 100, got %d", u.MaxIdleConnsPerHost)
	}
	if u.MaxConnsPerHost != 0 {
		t.Errorf("expected default max_conns_per_host 0, got %d", u.MaxConnsPerHost)
	}
	if u.HTTP2 {
		t.Error("expected default http2 false")
	}
}

// An explicit small upstream timeout must be respected (only zero-values default).
func TestServerUpstreamExplicitSmallValueRespected(t *testing.T) {
	path := writeConfig(t, `
server:
  upstream:
    dial_timeout: 1s
backends:
  - url: "http://localhost:8001"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Upstream.DialTimeout != 1*time.Second {
		t.Errorf("expected explicit dial_timeout 1s to be respected, got %v", cfg.Server.Upstream.DialTimeout)
	}
	// Other fields still default.
	if cfg.Server.Upstream.IdleConnTimeout != 90*time.Second {
		t.Errorf("expected default idle_conn_timeout 90s, got %v", cfg.Server.Upstream.IdleConnTimeout)
	}
}

// L4 must be disabled by default with a defaulted dial timeout.
func TestL4DisabledByDefault(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.L4.Enabled {
		t.Error("expected l4 disabled by default")
	}
	if cfg.Server.L4.DialTimeout != 5*time.Second {
		t.Errorf("expected default l4 dial_timeout 5s, got %v", cfg.Server.L4.DialTimeout)
	}
}

// An enabled L4 proxy with a bad port must be rejected; a disabled L4 with an
// invalid/zero port must load fine.
func TestL4PortValidation(t *testing.T) {
	// Enabled with out-of-range port is rejected.
	bad := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
server:
  l4:
    enabled: true
    port: 0
`)
	if _, err := Load(bad); err == nil {
		t.Error("expected validation error for enabled l4 with port 0, got nil")
	}

	bad2 := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
server:
  l4:
    enabled: true
    port: 70000
`)
	if _, err := Load(bad2); err == nil {
		t.Error("expected validation error for enabled l4 with port 70000, got nil")
	}

	// Disabled with a zero port loads fine.
	ok := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
server:
  l4:
    enabled: false
`)
	if _, err := Load(ok); err != nil {
		t.Errorf("expected disabled l4 to load, got %v", err)
	}
}

// WebSocket zero values mean unlimited (the default) and must load.
func TestWebSocketZeroUnlimited(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.WebSocket.IdleTimeout != 0 {
		t.Errorf("expected default websocket idle_timeout 0 (unlimited), got %v", cfg.Server.WebSocket.IdleTimeout)
	}
	if cfg.Server.WebSocket.MaxMessageBytes != 0 {
		t.Errorf("expected default websocket max_message_bytes 0 (unlimited), got %d", cfg.Server.WebSocket.MaxMessageBytes)
	}
}

// Negative upstream/websocket values must be rejected.
func TestValidateRejectsBadServerSubConfigs(t *testing.T) {
	cases := map[string]string{
		"negative upstream dial_timeout": `
backends:
  - url: "http://localhost:8001"
server:
  upstream:
    dial_timeout: -1s
`,
		"negative upstream max_idle_conns": `
backends:
  - url: "http://localhost:8001"
server:
  upstream:
    max_idle_conns: -1
`,
		"negative l4 dial_timeout": `
backends:
  - url: "http://localhost:8001"
server:
  l4:
    dial_timeout: -1s
`,
		"negative websocket idle_timeout": `
backends:
  - url: "http://localhost:8001"
server:
  websocket:
    idle_timeout: -1s
`,
		"negative websocket max_message_bytes": `
backends:
  - url: "http://localhost:8001"
server:
  websocket:
    max_message_bytes: -1
`,
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, content)
			if _, err := Load(path); err == nil {
				t.Errorf("expected validation error for %q, got nil", name)
			}
		})
	}
}

// Metrics.Host defaults to loopback when not set in the file.
func TestMetricsHostDefault(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Metrics.Host != "127.0.0.1" {
		t.Errorf("expected default metrics host 127.0.0.1, got %q", cfg.Metrics.Host)
	}
}

// With no routes configured the proxy keeps its single default balancer: the
// Routes slice stays nil so callers behave exactly as before.
func TestRoutesAbsentByDefault(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Routes != nil {
		t.Errorf("expected nil Routes with none configured, got %+v", cfg.Routes)
	}
}

// Routes must parse: match criteria, algorithm, backends, and consistent-hash.
func TestRoutesParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
routes:
  - name: "api"
    host: "api.example.com"
    path_prefix: "/v1"
    methods: ["GET", "POST"]
    headers:
      X-Tenant: "acme"
    algorithm: "least_conn"
    backends:
      - url: "http://localhost:9001"
        weight: 3
        max_conns: 40
      - url: "http://localhost:9002"
  - name: "hashed"
    path_prefix: "/cache"
    algorithm: "consistent_hash"
    consistent_hash:
      replicas: 256
      load_factor: 2.0
    backends:
      - url: "http://localhost:9003"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(cfg.Routes))
	}
	r0 := cfg.Routes[0]
	if r0.Name != "api" || r0.Host != "api.example.com" || r0.PathPrefix != "/v1" {
		t.Errorf("unexpected route[0] match: %+v", r0)
	}
	if len(r0.Methods) != 2 || r0.Methods[0] != "GET" || r0.Methods[1] != "POST" {
		t.Errorf("unexpected route[0] methods: %v", r0.Methods)
	}
	if r0.Headers["X-Tenant"] != "acme" {
		t.Errorf("unexpected route[0] headers: %v", r0.Headers)
	}
	if r0.Algorithm != "least_conn" {
		t.Errorf("expected route[0] algorithm least_conn, got %q", r0.Algorithm)
	}
	if len(r0.Backends) != 2 || r0.Backends[0].URL != "http://localhost:9001" {
		t.Errorf("unexpected route[0] backends: %+v", r0.Backends)
	}
	if r0.Backends[0].Weight != 3 || r0.Backends[0].MaxConns != 40 {
		t.Errorf("expected explicit weight/max_conns preserved, got %+v", r0.Backends[0])
	}
	r1 := cfg.Routes[1]
	if r1.Algorithm != "consistent_hash" {
		t.Errorf("expected route[1] algorithm consistent_hash, got %q", r1.Algorithm)
	}
	if r1.ConsistentHash.Replicas != 256 || r1.ConsistentHash.LoadFactor != 2.0 {
		t.Errorf("unexpected route[1] consistent_hash: %+v", r1.ConsistentHash)
	}
}

// Per-route defaults: algorithm round_robin, backend weight/max_conns, and
// consistent-hash replicas/load_factor must be applied by Load().
func TestRouteDefaults(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
routes:
  - path_prefix: "/app"
    backends:
      - url: "http://localhost:9001"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := cfg.Routes[0]
	if r.Algorithm != "round_robin" {
		t.Errorf("expected default route algorithm round_robin, got %q", r.Algorithm)
	}
	if r.Backends[0].Weight != 1 {
		t.Errorf("expected default route backend weight 1, got %d", r.Backends[0].Weight)
	}
	if r.Backends[0].MaxConns != 100 {
		t.Errorf("expected default route backend max_conns 100, got %d", r.Backends[0].MaxConns)
	}
	if r.ConsistentHash.Replicas != 100 {
		t.Errorf("expected default route replicas 100, got %d", r.ConsistentHash.Replicas)
	}
	if r.ConsistentHash.LoadFactor != 1.25 {
		t.Errorf("expected default route load_factor 1.25, got %v", r.ConsistentHash.LoadFactor)
	}
}

// A per-route backend health_check override must parse and be defaulted.
func TestRouteBackendHealthCheckDefaults(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
routes:
  - path_prefix: "/app"
    backends:
      - url: "http://localhost:9001"
        health_check:
          enabled: true
          path: "/ping"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	hc := cfg.Routes[0].Backends[0].HealthCheck
	if hc == nil {
		t.Fatal("expected non-nil per-route-backend health_check")
	}
	if hc.Path != "/ping" || hc.Type != "http" || hc.Method != "GET" {
		t.Errorf("unexpected defaulted health_check: %+v", hc)
	}
	if hc.HealthyThreshold != 2 || hc.UnhealthyThreshold != 3 {
		t.Errorf("expected defaulted thresholds 2/3, got %d/%d", hc.HealthyThreshold, hc.UnhealthyThreshold)
	}
}

// A catch-all route (no match criteria) is permitted as a documented per-route default.
func TestRouteCatchAllAllowed(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
routes:
  - name: "catch-all"
    backends:
      - url: "http://localhost:9001"
`)

	if _, err := Load(path); err != nil {
		t.Errorf("expected catch-all route to load, got %v", err)
	}
}

// Invalid route configs must be rejected: no backends, bad URL, unknown
// algorithm, negative weight, bad consistent-hash bounds, and duplicate names.
func TestValidateRejectsBadRoutes(t *testing.T) {
	cases := map[string]string{
		"route no backends": `
backends:
  - url: "http://localhost:8001"
routes:
  - name: "empty"
    path_prefix: "/x"
`,
		"route bad url": `
backends:
  - url: "http://localhost:8001"
routes:
  - path_prefix: "/x"
    backends:
      - url: "not-a-url"
`,
		"route non-http scheme": `
backends:
  - url: "http://localhost:8001"
routes:
  - path_prefix: "/x"
    backends:
      - url: "ftp://localhost:9001"
`,
		"route unknown algorithm": `
backends:
  - url: "http://localhost:8001"
routes:
  - path_prefix: "/x"
    algorithm: "magic"
    backends:
      - url: "http://localhost:9001"
`,
		"route negative weight": `
backends:
  - url: "http://localhost:8001"
routes:
  - path_prefix: "/x"
    backends:
      - url: "http://localhost:9001"
        weight: -1
`,
		"route load_factor below one": `
backends:
  - url: "http://localhost:8001"
routes:
  - path_prefix: "/x"
    consistent_hash:
      load_factor: 0.5
    backends:
      - url: "http://localhost:9001"
`,
		"duplicate route names": `
backends:
  - url: "http://localhost:8001"
routes:
  - name: "dup"
    path_prefix: "/a"
    backends:
      - url: "http://localhost:9001"
  - name: "dup"
    path_prefix: "/b"
    backends:
      - url: "http://localhost:9002"
`,
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, content)
			if _, err := Load(path); err == nil {
				t.Errorf("expected validation error for %q, got nil", name)
			}
		})
	}
}

// TLS security fields (min_version, cipher_suites, additional SNI certs,
// client_auth, client_ca_file, reload_on_change) must parse from YAML.
func TestTLSSecurityFieldsParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
tls:
  enabled: true
  cert_file: "/etc/certs/server.crt"
  key_file: "/etc/certs/server.key"
  min_version: "1.3"
  cipher_suites:
    - "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"
  certificates:
    - cert_file: "/etc/certs/alt.crt"
      key_file: "/etc/certs/alt.key"
  client_auth: "require_and_verify"
  client_ca_file: "/etc/certs/ca.crt"
  reload_on_change: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tlsc := cfg.TLS
	if tlsc.MinVersion != "1.3" {
		t.Errorf("expected min_version 1.3, got %q", tlsc.MinVersion)
	}
	if len(tlsc.CipherSuites) != 1 || tlsc.CipherSuites[0] != "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256" {
		t.Errorf("unexpected cipher_suites: %v", tlsc.CipherSuites)
	}
	if len(tlsc.Certificates) != 1 || tlsc.Certificates[0].CertFile != "/etc/certs/alt.crt" ||
		tlsc.Certificates[0].KeyFile != "/etc/certs/alt.key" {
		t.Errorf("unexpected certificates: %+v", tlsc.Certificates)
	}
	if tlsc.ClientAuth != "require_and_verify" {
		t.Errorf("unexpected client_auth: %q", tlsc.ClientAuth)
	}
	if tlsc.ClientCAFile != "/etc/certs/ca.crt" {
		t.Errorf("unexpected client_ca_file: %q", tlsc.ClientCAFile)
	}
	if !tlsc.ReloadOnChange {
		t.Error("expected reload_on_change true")
	}
}

// TLS defaults: min_version 1.2, client_auth none must be applied by Load().
func TestTLSDefaults(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TLS.MinVersion != "1.2" {
		t.Errorf("expected default min_version 1.2, got %q", cfg.TLS.MinVersion)
	}
	if cfg.TLS.ClientAuth != "none" {
		t.Errorf("expected default client_auth none, got %q", cfg.TLS.ClientAuth)
	}
}

// Security config (headers, cors, acl, auth) must parse from YAML.
func TestSecurityConfigParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
security:
  headers:
    enabled: true
    hsts: "max-age=31536000"
    frame_options: "DENY"
    content_type_options: true
    csp: "default-src 'self'"
    referrer_policy: "no-referrer"
  cors:
    enabled: true
    allow_origins: ["https://a.example.com"]
    allow_methods: ["GET", "POST"]
    allow_headers: ["Authorization"]
    allow_credentials: true
    max_age: 600
  acl:
    allow: ["10.0.0.0/8"]
    deny: ["192.168.1.1"]
    methods: ["GET", "POST"]
    blocked_paths: ["/admin"]
  auth:
    type: "apikey"
    api_keys: ["k1", "k2"]
    header: "X-Custom-Key"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	h := cfg.Security.Headers
	if !h.Enabled || h.HSTS != "max-age=31536000" || h.FrameOptions != "DENY" ||
		!h.ContentTypeOptions || h.CSP != "default-src 'self'" || h.ReferrerPolicy != "no-referrer" {
		t.Errorf("unexpected headers config: %+v", h)
	}
	co := cfg.Security.CORS
	if !co.Enabled || len(co.AllowOrigins) != 1 || co.AllowOrigins[0] != "https://a.example.com" ||
		len(co.AllowMethods) != 2 || len(co.AllowHeaders) != 1 || !co.AllowCredentials || co.MaxAge != 600 {
		t.Errorf("unexpected cors config: %+v", co)
	}
	acl := cfg.Security.ACL
	if len(acl.Allow) != 1 || acl.Allow[0] != "10.0.0.0/8" || len(acl.Deny) != 1 ||
		len(acl.Methods) != 2 || len(acl.BlockedPaths) != 1 || acl.BlockedPaths[0] != "/admin" {
		t.Errorf("unexpected acl config: %+v", acl)
	}
	auth := cfg.Security.Auth
	if auth.Type != "apikey" || len(auth.APIKeys) != 2 || auth.Header != "X-Custom-Key" {
		t.Errorf("unexpected auth config: %+v", auth)
	}
}

// Auth defaults: Header "X-API-Key" and JWTAlg "HS256" must be applied by Load().
func TestAuthDefaults(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Security.Auth.Header != "X-API-Key" {
		t.Errorf("expected default auth header X-API-Key, got %q", cfg.Security.Auth.Header)
	}
	if cfg.Security.Auth.JWTAlg != "HS256" {
		t.Errorf("expected default jwt_alg HS256, got %q", cfg.Security.Auth.JWTAlg)
	}
}

// ACME fields must parse from YAML and the HTTP-01 challenge port must be
// preserved when set explicitly.
func TestACMEConfigParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
tls:
  enabled: true
  acme:
    enabled: true
    domains: ["a.example.com", "b.example.com"]
    email: "ops@example.com"
    cache_dir: "/var/lib/rplb/acme"
    directory_url: "https://acme-staging.example.com/directory"
    http_challenge_port: 8080
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.TLS.ACME
	if !a.Enabled {
		t.Error("expected acme enabled")
	}
	if len(a.Domains) != 2 || a.Domains[0] != "a.example.com" || a.Domains[1] != "b.example.com" {
		t.Errorf("unexpected domains: %v", a.Domains)
	}
	if a.Email != "ops@example.com" {
		t.Errorf("unexpected email: %q", a.Email)
	}
	if a.CacheDir != "/var/lib/rplb/acme" {
		t.Errorf("unexpected cache_dir: %q", a.CacheDir)
	}
	if a.DirectoryURL != "https://acme-staging.example.com/directory" {
		t.Errorf("unexpected directory_url: %q", a.DirectoryURL)
	}
	if a.HTTPChallengePort != 8080 {
		t.Errorf("expected http_challenge_port 8080, got %d", a.HTTPChallengePort)
	}
}

// ACME HTTPChallengePort must default to 80 when omitted, and ACME stays disabled
// by default. OCSPStapling must default to false.
func TestACMEDefaults(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TLS.ACME.Enabled {
		t.Error("expected acme disabled by default")
	}
	if cfg.TLS.ACME.HTTPChallengePort != 80 {
		t.Errorf("expected default http_challenge_port 80, got %d", cfg.TLS.ACME.HTTPChallengePort)
	}
	if cfg.TLS.OCSPStapling {
		t.Error("expected ocsp_stapling disabled by default")
	}
}

// An enabled ACME config with domains omitted must be rejected.
func TestValidateRejectsACMEWithoutDomains(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
tls:
  enabled: true
  acme:
    enabled: true
`)

	if _, err := Load(path); err == nil {
		t.Error("expected validation error for acme enabled without domains, got nil")
	}
}

// Invalid ACME configs must be rejected: empty domain entry, bad challenge port,
// and a bad directory_url.
func TestValidateRejectsBadACMEConfigs(t *testing.T) {
	cases := map[string]string{
		"empty domain entry": `
backends:
  - url: "http://localhost:8001"
tls:
  acme:
    enabled: true
    domains: ["  "]
`,
		"challenge port out of range": `
backends:
  - url: "http://localhost:8001"
tls:
  acme:
    enabled: true
    domains: ["a.example.com"]
    http_challenge_port: 70000
`,
		"bad directory_url": `
backends:
  - url: "http://localhost:8001"
tls:
  acme:
    enabled: true
    domains: ["a.example.com"]
    directory_url: "not-a-url"
`,
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, content)
			if _, err := Load(path); err == nil {
				t.Errorf("expected validation error for %q, got nil", name)
			}
		})
	}
}

// ACME_CACHE_DIR env var must override the cache_dir field loaded from the
// config file, allowing container deployments to inject the cache path at
// runtime without modifying the config file.
func TestACMECacheDirEnvOverride(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
tls:
  enabled: true
  acme:
    enabled: true
    domains: ["a.example.com"]
    cache_dir: "/original/cache"
    http_challenge_port: 80
`)
	dir := t.TempDir()
	t.Setenv("ACME_CACHE_DIR", dir)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TLS.ACME.CacheDir != dir {
		t.Errorf("ACME_CACHE_DIR override: got %q, want %q", cfg.TLS.ACME.CacheDir, dir)
	}
}

// OCSPStapling must parse from YAML when set true.
func TestOCSPStaplingParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
tls:
  enabled: true
  ocsp_stapling: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.TLS.OCSPStapling {
		t.Error("expected ocsp_stapling true")
	}
}

// An RS256 JWT auth config with a PEM public key must load and preserve it.
func TestJWTAuthRS256PublicKeyParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
security:
  auth:
    type: "jwt"
    jwt_alg: "RS256"
    jwt_public_key: |
      -----BEGIN PUBLIC KEY-----
      MIIBIjANBgkqh
      -----END PUBLIC KEY-----
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Security.Auth.JWTAlg != "RS256" {
		t.Errorf("expected jwt_alg RS256, got %q", cfg.Security.Auth.JWTAlg)
	}
	if cfg.Security.Auth.JWTPublicKey == "" {
		t.Error("expected jwt_public_key to be preserved")
	}
}

// An RS256 JWT auth config with a JWKS URL must load and preserve it.
func TestJWTAuthRS256JWKSURLParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
security:
  auth:
    type: "jwt"
    jwt_alg: "RS256"
    jwks_url: "https://issuer.example.com/.well-known/jwks.json"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Security.Auth.JWKSURL != "https://issuer.example.com/.well-known/jwks.json" {
		t.Errorf("unexpected jwks_url: %q", cfg.Security.Auth.JWKSURL)
	}
}

// RS256 JWT auth key-source rules must be enforced: exactly one of jwt_public_key
// or jwks_url is required, and a bad jwks_url is rejected.
func TestValidateRejectsBadRS256JWTConfigs(t *testing.T) {
	cases := map[string]string{
		"rs256 with neither key source": `
backends:
  - url: "http://localhost:8001"
security:
  auth:
    type: "jwt"
    jwt_alg: "RS256"
`,
		"rs256 with both key sources": `
backends:
  - url: "http://localhost:8001"
security:
  auth:
    type: "jwt"
    jwt_alg: "RS256"
    jwt_public_key: "-----BEGIN PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----"
    jwks_url: "https://issuer.example.com/jwks.json"
`,
		"rs256 with bad jwks_url": `
backends:
  - url: "http://localhost:8001"
security:
  auth:
    type: "jwt"
    jwt_alg: "RS256"
    jwks_url: "not-a-url"
`,
		"jwt with unknown alg": `
backends:
  - url: "http://localhost:8001"
security:
  auth:
    type: "jwt"
    jwt_alg: "ES256"
    jwt_secret: "x"
`,
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, content)
			if _, err := Load(path); err == nil {
				t.Errorf("expected validation error for %q, got nil", name)
			}
		})
	}
}

// A jwt auth config with a secret must load and preserve the secret.
func TestJWTAuthParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
security:
  auth:
    type: "jwt"
    jwt_secret: "s3cr3t"
    jwt_alg: "HS256"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Security.Auth.Type != "jwt" || cfg.Security.Auth.JWTSecret != "s3cr3t" {
		t.Errorf("unexpected jwt auth: %+v", cfg.Security.Auth)
	}
}

// Invalid TLS/security configs must be rejected.
func TestValidateRejectsBadTLSAndSecurityConfigs(t *testing.T) {
	cases := map[string]string{
		"bad min_version": `
backends:
  - url: "http://localhost:8001"
tls:
  min_version: "1.1"
`,
		"bad client_auth": `
backends:
  - url: "http://localhost:8001"
tls:
  client_auth: "maybe"
`,
		"unknown cipher suite": `
backends:
  - url: "http://localhost:8001"
tls:
  cipher_suites: ["TLS_NOT_A_REAL_SUITE"]
`,
		"require_and_verify without client_ca_file": `
backends:
  - url: "http://localhost:8001"
tls:
  client_auth: "require_and_verify"
`,
		"bad auth type": `
backends:
  - url: "http://localhost:8001"
security:
  auth:
    type: "oauth"
`,
		"jwt without secret": `
backends:
  - url: "http://localhost:8001"
security:
  auth:
    type: "jwt"
`,
		"bad acl allow cidr": `
backends:
  - url: "http://localhost:8001"
security:
  acl:
    allow: ["not-a-cidr"]
`,
		"bad acl deny cidr": `
backends:
  - url: "http://localhost:8001"
security:
  acl:
    deny: ["999.999.0.0/8"]
`,
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, content)
			if _, err := Load(path); err == nil {
				t.Errorf("expected validation error for %q, got nil", name)
			}
		})
	}
}

// writeTestKeyPair generates a self-signed ECDSA cert/key PEM pair in dir and
// returns their file paths.
func writeTestKeyPair(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "client.crt")
	keyPath = filepath.Join(dir, "client.key")

	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	certOut.Close()

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatal(err)
	}
	keyOut.Close()
	return certPath, keyPath
}

// ClientTLSConfig must load a client certificate into Certificates when
// ClientCertFile/ClientKeyFile are configured (mTLS to backends).
func TestBackendClientTLSConfigLoadsClientCert(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeTestKeyPair(t, dir)

	b := BackendTLSConfig{
		ClientCertFile: certPath,
		ClientKeyFile:  keyPath,
	}
	cfg, err := b.ClientTLSConfig()
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil tls.Config when client cert configured")
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("expected 1 loaded client certificate, got %d", len(cfg.Certificates))
	}
}

// ClientTLSConfig must stay nil when no customization (including no client cert)
// is configured, preserving current behavior.
func TestBackendClientTLSConfigNilWhenUnset(t *testing.T) {
	b := BackendTLSConfig{}
	cfg, err := b.ClientTLSConfig()
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	if cfg != nil {
		t.Errorf("expected nil tls.Config when unconfigured, got %+v", cfg)
	}
}

// Canary config must parse: enabled, weight_percent, algorithm, consistent_hash,
// and backends (with explicit weight/max_conns preserved).
func TestCanaryConfigParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
canary:
  enabled: true
  weight_percent: 20
  algorithm: "consistent_hash"
  consistent_hash:
    replicas: 256
    load_factor: 2.0
  backends:
    - url: "http://localhost:9001"
      weight: 3
      max_conns: 40
    - url: "http://localhost:9002"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cn := cfg.Canary
	if !cn.Enabled || cn.WeightPercent != 20 || cn.Algorithm != "consistent_hash" {
		t.Errorf("unexpected canary config: %+v", cn)
	}
	if cn.ConsistentHash.Replicas != 256 || cn.ConsistentHash.LoadFactor != 2.0 {
		t.Errorf("unexpected canary consistent_hash: %+v", cn.ConsistentHash)
	}
	if len(cn.Backends) != 2 || cn.Backends[0].URL != "http://localhost:9001" {
		t.Fatalf("unexpected canary backends: %+v", cn.Backends)
	}
	if cn.Backends[0].Weight != 3 || cn.Backends[0].MaxConns != 40 {
		t.Errorf("expected explicit weight/max_conns preserved, got %+v", cn.Backends[0])
	}
}

// Canary defaults must be applied by Load(): algorithm round_robin, consistent-
// hash replicas/load_factor, and per-backend weight/max_conns.
func TestCanaryDefaults(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
canary:
  enabled: true
  weight_percent: 10
  backends:
    - url: "http://localhost:9001"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cn := cfg.Canary
	if cn.Algorithm != "round_robin" {
		t.Errorf("expected default canary algorithm round_robin, got %q", cn.Algorithm)
	}
	if cn.ConsistentHash.Replicas != 100 {
		t.Errorf("expected default canary replicas 100, got %d", cn.ConsistentHash.Replicas)
	}
	if cn.ConsistentHash.LoadFactor != 1.25 {
		t.Errorf("expected default canary load_factor 1.25, got %v", cn.ConsistentHash.LoadFactor)
	}
	if cn.Backends[0].Weight != 1 || cn.Backends[0].MaxConns != 100 {
		t.Errorf("expected defaulted canary backend weight/max_conns, got %+v", cn.Backends[0])
	}
}

// Mirror config must parse: enabled, url, sample_percent, and timeout.
func TestMirrorConfigParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
mirror:
  enabled: true
  url: "http://shadow:9000"
  sample_percent: 25
  timeout: 2s
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m := cfg.Mirror
	if !m.Enabled || m.URL != "http://shadow:9000" || m.SamplePercent != 25 || m.Timeout != 2*time.Second {
		t.Errorf("unexpected mirror config: %+v", m)
	}
}

// Rewrite config must parse: request/response header set/remove, strip prefix,
// and https_redirect.
func TestRewriteConfigParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
rewrite:
  request_headers_set:
    X-Env: "prod"
  request_headers_remove: ["X-Debug"]
  response_headers_set:
    X-Served-By: "rplb"
  response_headers_remove: ["Server"]
  strip_path_prefix: "/api"
  https_redirect: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rw := cfg.Rewrite
	if rw.RequestHeadersSet["X-Env"] != "prod" {
		t.Errorf("unexpected request_headers_set: %v", rw.RequestHeadersSet)
	}
	if len(rw.RequestHeadersRemove) != 1 || rw.RequestHeadersRemove[0] != "X-Debug" {
		t.Errorf("unexpected request_headers_remove: %v", rw.RequestHeadersRemove)
	}
	if rw.ResponseHeadersSet["X-Served-By"] != "rplb" {
		t.Errorf("unexpected response_headers_set: %v", rw.ResponseHeadersSet)
	}
	if len(rw.ResponseHeadersRemove) != 1 || rw.ResponseHeadersRemove[0] != "Server" {
		t.Errorf("unexpected response_headers_remove: %v", rw.ResponseHeadersRemove)
	}
	if rw.StripPathPrefix != "/api" || !rw.HTTPSRedirect {
		t.Errorf("unexpected rewrite path/redirect: %+v", rw)
	}
}

// Fault-injection config must parse: enabled, delay/abort percents, delay, and
// abort status.
func TestFaultInjectionConfigParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
fault_injection:
  enabled: true
  delay_percent: 10
  delay: 100ms
  abort_percent: 5
  abort_status: 500
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	f := cfg.FaultInjection
	if !f.Enabled || f.DelayPercent != 10 || f.Delay != 100*time.Millisecond ||
		f.AbortPercent != 5 || f.AbortStatus != 500 {
		t.Errorf("unexpected fault_injection config: %+v", f)
	}
}

// Fault-injection AbortStatus must default to 503 when enabled and left unset.
func TestFaultInjectionAbortStatusDefault(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
fault_injection:
  enabled: true
  abort_percent: 5
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.FaultInjection.AbortStatus != 503 {
		t.Errorf("expected default abort_status 503, got %d", cfg.FaultInjection.AbortStatus)
	}
}

// Compression min_size and content_types must parse; defaults preserve current
// behavior (min_size 0, empty content_types).
func TestCompressionNewFieldsParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
compression:
  enabled: true
  min_size: 1024
  content_types:
    - "text/"
    - "application/json"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	comp := cfg.Compression
	if !comp.Enabled || comp.MinSize != 1024 {
		t.Errorf("unexpected compression config: %+v", comp)
	}
	if len(comp.ContentTypes) != 2 || comp.ContentTypes[0] != "text/" || comp.ContentTypes[1] != "application/json" {
		t.Errorf("unexpected content_types: %v", comp.ContentTypes)
	}
}

// Compression defaults preserve current behavior: min_size 0, nil content_types.
func TestCompressionDefaults(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
compression:
  enabled: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Compression.MinSize != 0 {
		t.Errorf("expected default min_size 0, got %d", cfg.Compression.MinSize)
	}
	if cfg.Compression.ContentTypes != nil {
		t.Errorf("expected nil content_types, got %v", cfg.Compression.ContentTypes)
	}
}

// The new opt-in blocks must be absent/disabled by default, preserving behavior.
func TestNewBlocksDisabledByDefault(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Canary.Enabled || cfg.Mirror.Enabled || cfg.FaultInjection.Enabled {
		t.Errorf("expected canary/mirror/fault_injection disabled by default")
	}
	// Canary algorithm is still defaulted for predictability even when disabled.
	if cfg.Canary.Algorithm != "round_robin" {
		t.Errorf("expected canary algorithm defaulted to round_robin, got %q", cfg.Canary.Algorithm)
	}
	if cfg.Rewrite.RequestHeadersSet != nil || cfg.Rewrite.StripPathPrefix != "" || cfg.Rewrite.HTTPSRedirect {
		t.Errorf("expected empty rewrite config, got %+v", cfg.Rewrite)
	}
}

// Invalid canary/mirror/fault-injection configs must be rejected: bad
// percentages, bad abort status, and bad canary/mirror URLs.
func TestValidateRejectsBadCanaryMirrorFaultConfigs(t *testing.T) {
	cases := map[string]string{
		"canary weight over 100": `
backends:
  - url: "http://localhost:8001"
canary:
  enabled: true
  weight_percent: 150
  backends:
    - url: "http://localhost:9001"
`,
		"canary weight negative": `
backends:
  - url: "http://localhost:8001"
canary:
  weight_percent: -1
`,
		"canary enabled without backends": `
backends:
  - url: "http://localhost:8001"
canary:
  enabled: true
  weight_percent: 10
`,
		"canary bad backend url": `
backends:
  - url: "http://localhost:8001"
canary:
  enabled: true
  weight_percent: 10
  backends:
    - url: "not-a-url"
`,
		"canary non-http backend scheme": `
backends:
  - url: "http://localhost:8001"
canary:
  enabled: true
  weight_percent: 10
  backends:
    - url: "ftp://localhost:9001"
`,
		"canary unknown algorithm": `
backends:
  - url: "http://localhost:8001"
canary:
  enabled: true
  weight_percent: 10
  algorithm: "magic"
  backends:
    - url: "http://localhost:9001"
`,
		"mirror sample over 100": `
backends:
  - url: "http://localhost:8001"
mirror:
  enabled: true
  url: "http://shadow:9000"
  sample_percent: 101
`,
		"mirror enabled with bad url": `
backends:
  - url: "http://localhost:8001"
mirror:
  enabled: true
  url: "not-a-url"
  sample_percent: 10
`,
		"mirror enabled with non-http url": `
backends:
  - url: "http://localhost:8001"
mirror:
  enabled: true
  url: "ftp://shadow:9000"
  sample_percent: 10
`,
		"mirror negative timeout": `
backends:
  - url: "http://localhost:8001"
mirror:
  enabled: true
  url: "http://shadow:9000"
  timeout: -1s
`,
		"fault delay percent out of range": `
backends:
  - url: "http://localhost:8001"
fault_injection:
  enabled: true
  delay_percent: 200
`,
		"fault abort percent negative": `
backends:
  - url: "http://localhost:8001"
fault_injection:
  abort_percent: -5
`,
		"fault bad abort status": `
backends:
  - url: "http://localhost:8001"
fault_injection:
  enabled: true
  abort_percent: 5
  abort_status: 99
`,
		"fault negative delay": `
backends:
  - url: "http://localhost:8001"
fault_injection:
  enabled: true
  delay: -1s
`,
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, content)
			if _, err := Load(path); err == nil {
				t.Errorf("expected validation error for %q, got nil", name)
			}
		})
	}
}

// A require_and_verify client_auth with a client_ca_file must load.
func TestTLSRequireAndVerifyWithCA(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
tls:
  client_auth: "require_and_verify"
  client_ca_file: "/etc/certs/ca.crt"
`)

	if _, err := Load(path); err != nil {
		t.Errorf("expected require_and_verify with client_ca_file to load, got %v", err)
	}
}

// WatchConfig defaults to disabled and WatchInterval stays 0 when the config
// file omits the watch fields.
func TestWatchConfigDefaultDisabled(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.WatchConfig {
		t.Error("expected watch_config disabled by default")
	}
	if cfg.Server.WatchInterval != 0 {
		t.Errorf("expected default watch_interval 0 when disabled, got %v", cfg.Server.WatchInterval)
	}
}

// WatchConfig and WatchInterval must parse from YAML, and an explicit interval
// must be preserved.
func TestWatchConfigExplicitInterval(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
server:
  watch_config: true
  watch_interval: 2s
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Server.WatchConfig {
		t.Error("expected watch_config true")
	}
	if cfg.Server.WatchInterval != 2*time.Second {
		t.Errorf("expected watch_interval 2s, got %v", cfg.Server.WatchInterval)
	}
}

// When WatchConfig is enabled without an explicit interval, Load() must default
// WatchInterval to 5s.
func TestWatchConfigDefaultsIntervalWhenEnabled(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
server:
  watch_config: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.WatchInterval != 5*time.Second {
		t.Errorf("expected default watch_interval 5s when enabled, got %v", cfg.Server.WatchInterval)
	}
}

// A negative WatchInterval must be rejected by validation.
func TestValidateRejectsNegativeWatchInterval(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
server:
  watch_interval: -1s
`)

	if _, err := Load(path); err == nil {
		t.Error("expected validation error for negative watch_interval, got nil")
	}
}

// Cache fields must parse from YAML.
func TestCacheConfigParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
cache:
  enabled: true
  default_ttl: 120s
  max_entries: 500
  max_body_bytes: 2097152
  methods: ["GET", "POST"]
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cfg.Cache
	if !c.Enabled {
		t.Error("expected cache enabled")
	}
	if c.DefaultTTL != 120*time.Second {
		t.Errorf("expected default_ttl 120s, got %v", c.DefaultTTL)
	}
	if c.MaxEntries != 500 {
		t.Errorf("expected max_entries 500, got %d", c.MaxEntries)
	}
	if c.MaxBodyBytes != 2097152 {
		t.Errorf("expected max_body_bytes 2097152, got %d", c.MaxBodyBytes)
	}
	if len(c.Methods) != 2 || c.Methods[0] != "GET" || c.Methods[1] != "POST" {
		t.Errorf("unexpected methods: %v", c.Methods)
	}
}

// Cache defaults (default_ttl 60s, max_entries 1000, max_body_bytes 1MiB,
// methods [GET,HEAD]) must be applied by Load() even when the block is omitted,
// and the cache must stay disabled by default.
func TestCacheDefaults(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cfg.Cache
	if c.Enabled {
		t.Error("expected cache disabled by default")
	}
	if c.DefaultTTL != 60*time.Second {
		t.Errorf("expected default default_ttl 60s, got %v", c.DefaultTTL)
	}
	if c.MaxEntries != 1000 {
		t.Errorf("expected default max_entries 1000, got %d", c.MaxEntries)
	}
	if c.MaxBodyBytes != 1<<20 {
		t.Errorf("expected default max_body_bytes 1MiB, got %d", c.MaxBodyBytes)
	}
	if len(c.Methods) != 2 || c.Methods[0] != "GET" || c.Methods[1] != "HEAD" {
		t.Errorf("expected default methods [GET HEAD], got %v", c.Methods)
	}
}

// validateCache guards an enabled cache against non-positive TTL/entries/body and
// empty methods. Because Load() defaults these before validation, YAML cannot
// produce such a config; the validator is exercised directly here to prove the
// bounds fire (e.g. if a future caller constructs a Config in code).
func TestValidateRejectsBadCacheConfigs(t *testing.T) {
	base := func() *Config {
		return &Config{Cache: CacheConfig{
			Enabled:      true,
			DefaultTTL:   60 * time.Second,
			MaxEntries:   1000,
			MaxBodyBytes: 1 << 20,
			Methods:      []string{"GET"},
		}}
	}

	cases := map[string]func(*Config){
		"zero default_ttl":    func(c *Config) { c.Cache.DefaultTTL = 0 },
		"zero max_entries":    func(c *Config) { c.Cache.MaxEntries = 0 },
		"zero max_body_bytes": func(c *Config) { c.Cache.MaxBodyBytes = -1 },
		"empty methods":       func(c *Config) { c.Cache.Methods = nil },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := base()
			mutate(c)
			if err := c.validateCache(); err == nil {
				t.Errorf("expected validateCache error for %q, got nil", name)
			}
		})
	}

	// A well-formed enabled cache must pass.
	if err := base().validateCache(); err != nil {
		t.Errorf("expected valid cache to pass, got %v", err)
	}
}

// A disabled cache with non-positive values must still load (defaults apply, no
// validation).
func TestCacheDisabledSkipsValidation(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
cache:
  enabled: false
`)

	if _, err := Load(path); err != nil {
		t.Errorf("expected disabled cache to load, got %v", err)
	}
}

// Discovery DNS targets must parse from YAML.
func TestDiscoveryConfigParse(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
discovery:
  dns:
    - name: "svc.internal"
      type: "a"
      scheme: "https"
      port: 8443
      interval: 10s
      weight: 5
      max_conns: 25
    - name: "srv.internal"
      type: "srv"
      interval: 15s
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Discovery.DNS) != 2 {
		t.Fatalf("expected 2 dns targets, got %d", len(cfg.Discovery.DNS))
	}
	t0 := cfg.Discovery.DNS[0]
	if t0.Name != "svc.internal" || t0.Type != "a" || t0.Scheme != "https" ||
		t0.Port != 8443 || t0.Interval != 10*time.Second || t0.Weight != 5 || t0.MaxConns != 25 {
		t.Errorf("unexpected dns target[0]: %+v", t0)
	}
	t1 := cfg.Discovery.DNS[1]
	if t1.Name != "srv.internal" || t1.Type != "srv" || t1.Interval != 15*time.Second {
		t.Errorf("unexpected dns target[1]: %+v", t1)
	}
}

// Discovery defaults (Type "a", Scheme "http", Interval 30s, Weight 1, MaxConns
// 100) must be applied by Load().
func TestDiscoveryDefaults(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
discovery:
  dns:
    - name: "svc.internal"
      port: 8080
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tgt := cfg.Discovery.DNS[0]
	if tgt.Type != "a" {
		t.Errorf("expected default type a, got %q", tgt.Type)
	}
	if tgt.Scheme != "http" {
		t.Errorf("expected default scheme http, got %q", tgt.Scheme)
	}
	if tgt.Interval != 30*time.Second {
		t.Errorf("expected default interval 30s, got %v", tgt.Interval)
	}
	if tgt.Weight != 1 {
		t.Errorf("expected default weight 1, got %d", tgt.Weight)
	}
	if tgt.MaxConns != 100 {
		t.Errorf("expected default max_conns 100, got %d", tgt.MaxConns)
	}
}

// With no discovery configured the DNS slice stays nil.
func TestDiscoveryAbsentByDefault(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Discovery.DNS != nil {
		t.Errorf("expected nil discovery.dns, got %+v", cfg.Discovery.DNS)
	}
}

// Invalid discovery configs must be rejected.
func TestValidateRejectsBadDiscovery(t *testing.T) {
	cases := map[string]string{
		"missing name": `
backends:
  - url: "http://localhost:8001"
discovery:
  dns:
    - type: "a"
      port: 80
`,
		"bad type": `
backends:
  - url: "http://localhost:8001"
discovery:
  dns:
    - name: "svc"
      type: "cname"
      port: 80
`,
		"bad scheme": `
backends:
  - url: "http://localhost:8001"
discovery:
  dns:
    - name: "svc"
      scheme: "grpc"
      port: 80
`,
		"a-type bad port": `
backends:
  - url: "http://localhost:8001"
discovery:
  dns:
    - name: "svc"
      type: "a"
      port: 0
`,
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, content)
			if _, err := Load(path); err == nil {
				t.Errorf("expected validation error for %q, got nil", name)
			}
		})
	}
}

// An srv-type target does not require a port and must load.
func TestDiscoverySRVNoPortRequired(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://localhost:8001"
discovery:
  dns:
    - name: "_http._tcp.svc.internal"
      type: "srv"
`)

	if _, err := Load(path); err != nil {
		t.Errorf("expected srv target without port to load, got %v", err)
	}
}
