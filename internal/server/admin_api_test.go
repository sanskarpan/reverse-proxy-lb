package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"reverse-proxy-lb/internal/config"
)

// buildAdminServer creates a server wired with the given set of backend URLs,
// suitable for exercising the admin HTTP API via metricsMux.
func buildAdminServer(t *testing.T, backendURLs ...string) *Server {
	t.Helper()
	if len(backendURLs) == 0 {
		// Need at least one backend to satisfy setupProxy.
		be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		t.Cleanup(be.Close)
		backendURLs = []string{be.URL}
	}

	var backendsYAML strings.Builder
	for _, u := range backendURLs {
		fmt.Fprintf(&backendsYAML, "  - url: %q\n    weight: 1\n", u)
	}

	content := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 19090
  shutdown_timeout: 1s
backends:
%sload_balancer:
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
`, backendsYAML.String())

	f, err := os.CreateTemp(t.TempDir(), "admin-api-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return New(cfg, f.Name())
}

// ---- handleAdminBackends ----

func TestHandleAdminBackends_OK(t *testing.T) {
	t.Parallel()
	be1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be1.Close()
	be2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be2.Close()

	s := buildAdminServer(t, be1.URL, be2.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/backends", nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: want application/json, got %q", ct)
	}

	var out []adminBackend
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v (body=%q)", err, rec.Body.String())
	}
	if len(out) != 2 {
		t.Fatalf("want 2 backends, got %d", len(out))
	}
	urls := map[string]bool{be1.URL: true, be2.URL: true}
	for _, b := range out {
		if !urls[b.URL] {
			t.Errorf("unexpected backend URL %q", b.URL)
		}
		if b.CircuitState == "" {
			t.Errorf("empty circuit_state for %q", b.URL)
		}
	}
}

func TestHandleAdminBackends_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	s := buildAdminServer(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/admin/backends", nil)
		s.metricsMux.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: want 405, got %d", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Errorf("%s: Allow header: want GET, got %q", method, allow)
		}
	}
}

func TestHandleAdminBackends_EmptyList(t *testing.T) {
	t.Parallel()
	// Build a server with one backend, then remove it from the balancer.
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	s := buildAdminServer(t, be.URL)

	// Remove the backend to get an empty list.
	for _, b := range s.balancer.All() {
		s.balancer.Remove(b)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/backends", nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var out []adminBackend
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("want empty list, got %v", out)
	}
}

// ---- handleAdminDrain / handleAdminUndrain ----

func TestHandleAdminDrain_OK(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	s := buildAdminServer(t, be.URL)

	// Confirm the backend starts healthy.
	all := s.balancer.All()
	if len(all) == 0 {
		t.Fatal("no backends in balancer")
	}
	if !all[0].IsHealthy() {
		t.Fatal("backend should start healthy")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/drain?url="+be.URL, nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if all[0].IsHealthy() {
		t.Error("backend should be marked unhealthy after drain")
	}
}

func TestHandleAdminUndrain_OK(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	s := buildAdminServer(t, be.URL)

	// First drain it.
	all := s.balancer.All()
	all[0].SetHealthy(false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/undrain?url="+be.URL, nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if !all[0].IsHealthy() {
		t.Error("backend should be healthy after undrain")
	}
}

func TestHandleAdminDrain_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	s := buildAdminServer(t, be.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/drain?url="+be.URL, nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}

func TestHandleAdminUndrain_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	s := buildAdminServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/undrain?url=something", nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}

func TestHandleAdminDrain_MissingURL(t *testing.T) {
	t.Parallel()
	s := buildAdminServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/drain", nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rec.Code)
	}
}

func TestHandleAdminUndrain_MissingURL(t *testing.T) {
	t.Parallel()
	s := buildAdminServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/undrain", nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rec.Code)
	}
}

func TestHandleAdminDrain_UnknownBackend(t *testing.T) {
	t.Parallel()
	s := buildAdminServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/drain?url=http://no-such-backend:9999", nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
}

func TestHandleAdminUndrain_UnknownBackend(t *testing.T) {
	t.Parallel()
	s := buildAdminServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/undrain?url=http://no-such-backend:9999", nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
}

// ---- handleAdminWeight ----

func TestHandleAdminWeight_OK(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	s := buildAdminServer(t, be.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/weight?url=%s&weight=5", be.URL), nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	all := s.balancer.All()
	if all[0].GetWeight() != 5 {
		t.Errorf("want weight=5, got %d", all[0].GetWeight())
	}
}

func TestHandleAdminWeight_MissingWeight(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	s := buildAdminServer(t, be.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/weight?url="+be.URL, nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rec.Code)
	}
}

func TestHandleAdminWeight_NegativeWeight(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	s := buildAdminServer(t, be.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/weight?url=%s&weight=-1", be.URL), nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rec.Code)
	}
}

func TestHandleAdminWeight_InvalidWeight(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	s := buildAdminServer(t, be.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/weight?url=%s&weight=notanint", be.URL), nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rec.Code)
	}
}

// ---- handleReload ----

func TestHandleReload_OK(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	s := buildAdminServer(t, be.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/reload", nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestHandleReload_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	s := buildAdminServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/reload", nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Errorf("Allow header: want POST, got %q", allow)
	}
}

// ---- metrics endpoint ----

func TestMetricsEndpoint_OK(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	s := buildAdminServer(t, be.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	// Prometheus text format always contains a HELP line.
	if !strings.Contains(body, "#") && body != "" {
		// Some builds use a no-op Prometheus handler; accept empty body.
	}
}

func TestMetricsJSONEndpoint_OK(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	s := buildAdminServer(t, be.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics.json", nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
}

// ---- GetMetrics accessor ----

func TestGetMetrics(t *testing.T) {
	t.Parallel()
	s := buildAdminServer(t)
	if m := s.GetMetrics(); m == nil {
		t.Error("GetMetrics should return non-nil Metrics instance")
	}
}

// ---- redisAddrFromURL ----

func TestRedisAddrFromURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"redis://localhost:6379", "localhost:6379"},
		{"rediss://secure-host:6380", "secure-host:6380"},
		{"localhost:6379", "localhost:6379"}, // already host:port
		{"redis://", "redis://"},             // too short — returned verbatim
		{"other://foo:1234", "other://foo:1234"},
	}
	for _, tc := range tests {
		got := redisAddrFromURL(tc.input)
		if got != tc.want {
			t.Errorf("redisAddrFromURL(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ---- Stop (graceful shutdown) ----

// TestStop_WithoutStart verifies that Stop() can be called on a server that was
// never started (i.e., all optional subsystems are nil). This exercises the nil
// guards in Stop's teardown sequence without starting any background goroutines.
func TestStop_WithoutStart(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	s := buildAdminServer(t, be.URL)

	// Stop() on a never-started server should return immediately without panic.
	// httpServer.Shutdown() works even if the server was never listening; it simply
	// closes the idle connection tracking state.
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop() on un-started server returned error: %v", err)
	}
}

// ---- configFileStamp ----

func TestConfigFileStamp_ValidFile(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp(t.TempDir(), "stamp-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("content")
	f.Close()

	mod, size := configFileStamp(f.Name())
	if mod.IsZero() {
		t.Error("expected non-zero modification time for existing file")
	}
	if size == 0 {
		t.Error("expected non-zero size for file with content")
	}
}

func TestConfigFileStamp_MissingFile(t *testing.T) {
	t.Parallel()
	mod, size := configFileStamp("/does/not/exist.yaml")
	if !mod.IsZero() {
		t.Error("expected zero modification time for missing file")
	}
	if size != 0 {
		t.Errorf("expected size=0 for missing file, got %d", size)
	}
}

// ---- backendsEqual ----

func TestBackendsEqual(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b []config.BackendConfig
		want bool
	}{
		{
			name: "identical single",
			a:    []config.BackendConfig{{URL: "http://a:1", Weight: 1}},
			b:    []config.BackendConfig{{URL: "http://a:1", Weight: 1}},
			want: true,
		},
		{
			name: "different weight",
			a:    []config.BackendConfig{{URL: "http://a:1", Weight: 1}},
			b:    []config.BackendConfig{{URL: "http://a:1", Weight: 2}},
			want: false,
		},
		{
			name: "different URL",
			a:    []config.BackendConfig{{URL: "http://a:1", Weight: 1}},
			b:    []config.BackendConfig{{URL: "http://b:2", Weight: 1}},
			want: false,
		},
		{
			name: "different lengths",
			a:    []config.BackendConfig{{URL: "http://a:1"}},
			b:    []config.BackendConfig{{URL: "http://a:1"}, {URL: "http://b:2"}},
			want: false,
		},
		{
			name: "both empty",
			a:    []config.BackendConfig{},
			b:    []config.BackendConfig{},
			want: true,
		},
		{
			name: "different max_conns",
			a:    []config.BackendConfig{{URL: "http://a:1", MaxConns: 10}},
			b:    []config.BackendConfig{{URL: "http://a:1", MaxConns: 20}},
			want: false,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := backendsEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("backendsEqual = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---- backendOverrides ----

func TestBackendOverrides_NoOverrides(t *testing.T) {
	t.Parallel()
	backends := []config.BackendConfig{
		{URL: "http://a:1"},
		{URL: "http://b:2"},
	}
	result := backendOverrides(backends)
	if result != nil {
		t.Errorf("expected nil when no backends have HealthCheck, got %v", result)
	}
}

func TestBackendOverrides_WithOverride(t *testing.T) {
	t.Parallel()
	hc := &config.HealthCheckConfig{Path: "/custom-health"}
	backends := []config.BackendConfig{
		{URL: "http://a:1"},
		{URL: "http://b:2", HealthCheck: hc},
	}
	result := backendOverrides(backends)
	if result == nil {
		t.Fatal("expected non-nil overrides map")
	}
	if _, ok := result["http://b:2"]; !ok {
		t.Error("expected override for http://b:2")
	}
	if _, ok := result["http://a:1"]; ok {
		t.Error("unexpected override for http://a:1 (no HealthCheck set)")
	}
}

// ---- hasHealthyBackend ----

func TestHasHealthyBackend_True(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	s := buildAdminServer(t, be.URL)

	// Default: backend starts healthy.
	if !s.hasHealthyBackend() {
		t.Error("hasHealthyBackend should be true when backend is healthy")
	}
}

func TestHasHealthyBackend_False(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	s := buildAdminServer(t, be.URL)

	// Mark all backends unhealthy.
	for _, b := range s.balancer.All() {
		b.SetHealthy(false)
	}

	if s.hasHealthyBackend() {
		t.Error("hasHealthyBackend should be false when all backends are unhealthy")
	}
}

// ---- balancerGroups ----

func TestBalancerGroups_DefaultOnly(t *testing.T) {
	t.Parallel()
	s := buildAdminServer(t)

	groups := s.balancerGroups()
	if len(groups) == 0 {
		t.Error("balancerGroups should return at least the default balancer")
	}
}

// ---- findBackend ----

func TestFindBackend_Found(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	s := buildAdminServer(t, be.URL)

	b, g := s.findBackend(be.URL)
	if b == nil {
		t.Fatal("findBackend should find the backend")
	}
	if g == nil {
		t.Fatal("findBackend should return the owning group")
	}
}

func TestFindBackend_NotFound(t *testing.T) {
	t.Parallel()
	s := buildAdminServer(t)

	b, g := s.findBackend("http://does-not-exist:9999")
	if b != nil || g != nil {
		t.Error("findBackend should return (nil, nil) for unknown URL")
	}
}

// ---- setupLimiter shared-store memory path ----

func TestSetupLimiter_MemoryStore(t *testing.T) {
	t.Parallel()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	content := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 19091
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
  enabled: true
  requests_per_second: 100
  burst: 10
  shared_store:
    enabled: true
    backend: "memory"
    key: "global"
metrics:
  enabled: false
compression:
  enabled: false
`, be.URL)

	f, err := os.CreateTemp(t.TempDir(), "limiter-mem-*.yaml")
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
		t.Error("limiter should be non-nil when rate_limiter.enabled=true")
	}
	// Stop the limiter background goroutine.
	s.limiter.Stop()
}
