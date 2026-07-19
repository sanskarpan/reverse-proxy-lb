package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
)

func opsTestServer(t *testing.T, backendURL string) *Server {
	t.Helper()
	content := "" +
		"server:\n  host: \"127.0.0.1\"\n  port: 18080\n  shutdown_timeout: 3s\n" +
		"backends:\n  - url: \"" + backendURL + "\"\n" +
		"load_balancer:\n  algorithm: \"round_robin\"\n  health_check:\n    enabled: false\n" +
		"circuit_breaker:\n  enabled: false\nrate_limiter:\n  enabled: false\nmetrics:\n  enabled: false\ncompression:\n  enabled: false\n"
	f, _ := os.CreateTemp("", "ops-*.yaml")
	defer os.Remove(f.Name())
	f.WriteString(content)
	f.Close()
	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.ShutdownTimeout != 3*time.Second {
		t.Fatalf("shutdown_timeout not parsed: %v", cfg.Server.ShutdownTimeout)
	}
	return New(cfg, f.Name())
}

func TestHealthzAndReadyz(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()
	s := opsTestServer(t, be.URL)
	mux := s.metricsMux

	// /healthz always 200
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("healthz: want 200, got %d", rec.Code)
	}

	// /readyz 200 when a backend is healthy (default healthy=true)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("readyz (healthy): want 200, got %d", rec.Code)
	}

	// mark the only backend unhealthy -> 503
	for _, b := range s.balancer.All() {
		b.SetHealthy(false)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz (unhealthy): want 503, got %d", rec.Code)
	}
}
