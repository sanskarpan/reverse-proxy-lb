package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reverse-proxy-lb/internal/config"
	"strings"
	"testing"
)

// This file drives §L7 request routing end-to-end through the real server stack.
// It stands up per-route backends, configures cfg.Routes so the server installs a
// routing.Router on the proxy (via SetRouter), and verifies that requests are sent
// to the correct route group, that unmatched requests fall through to the default
// balancer, and that in-group failover stays within the routed group (a routed
// request never bleeds into another route's backends).

// routedReq issues a request with the given host/path/method through handler and
// returns the backend id, status code, and response.
func routedReq(t *testing.T, handler http.Handler, method, host, path string) (string, int) {
	t.Helper()
	target := "http://" + host + path
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	return strings.TrimSpace(string(body)), res.StatusCode
}

// TestE2E_Routing_HostAndPath verifies first-match-wins routing by host and by
// path prefix, plus catch-all fallthrough to the default balancer.
func TestE2E_Routing_HostAndPath(t *testing.T) {
	def := newIDBackend("DEFAULT", nil)
	apiB := newIDBackend("API", nil)
	adminB := newIDBackend("ADMIN", nil)
	defer def.close()
	defer apiB.close()
	defer adminB.close()

	cfg := baseConfig("round_robin", backendCfgs(def))
	cfg.Routes = []config.RouteConfig{
		{
			Name:       "api-by-path",
			PathPrefix: "/api",
			Algorithm:  "round_robin",
			Backends:   backendCfgs(apiB),
		},
		{
			Name:      "admin-by-host",
			Host:      "admin.test",
			Algorithm: "round_robin",
			Backends:  backendCfgs(adminB),
		},
	}

	srv := New(cfg, "")
	h := srv.Handler()

	// Path-prefix route.
	if id, code := routedReq(t, h, http.MethodGet, "proxy.test", "/api/v1/users"); code != http.StatusOK || id != "API" {
		t.Fatalf("/api route: got id=%q code=%d, want API/200", id, code)
	}
	// Host route.
	if id, code := routedReq(t, h, http.MethodGet, "admin.test", "/dashboard"); code != http.StatusOK || id != "ADMIN" {
		t.Fatalf("admin.test route: got id=%q code=%d, want ADMIN/200", id, code)
	}
	// Unmatched -> default balancer.
	if id, code := routedReq(t, h, http.MethodGet, "proxy.test", "/other"); code != http.StatusOK || id != "DEFAULT" {
		t.Fatalf("catch-all: got id=%q code=%d, want DEFAULT/200", id, code)
	}
}

// TestE2E_Routing_MethodMatch verifies method-based route matching: a POST route
// only captures POSTs, while GET falls through to the default group.
func TestE2E_Routing_MethodMatch(t *testing.T) {
	def := newIDBackend("DEFAULT", nil)
	writeB := newIDBackend("WRITE", nil)
	defer def.close()
	defer writeB.close()

	cfg := baseConfig("round_robin", backendCfgs(def))
	cfg.Routes = []config.RouteConfig{
		{
			Name:      "writes",
			Methods:   []string{"POST", "PUT"},
			Algorithm: "round_robin",
			Backends:  backendCfgs(writeB),
		},
	}

	srv := New(cfg, "")
	h := srv.Handler()

	if id, code := routedReq(t, h, http.MethodPost, "proxy.test", "/x"); code != http.StatusOK || id != "WRITE" {
		t.Fatalf("POST route: got id=%q code=%d, want WRITE/200", id, code)
	}
	if id, code := routedReq(t, h, http.MethodGet, "proxy.test", "/x"); code != http.StatusOK || id != "DEFAULT" {
		t.Fatalf("GET fallthrough: got id=%q code=%d, want DEFAULT/200", id, code)
	}
}

// TestE2E_Routing_InGroupFailover verifies that when a matched route's primary
// backend fails with a retryable connect error, failover selects ANOTHER backend
// in the SAME route group, and never crosses into the default group's backend.
func TestE2E_Routing_InGroupFailover(t *testing.T) {
	// Default group's backend: must never be hit for a routed request.
	def := newIDBackend("DEFAULT", nil)
	defer def.close()

	// Route group backends: one is dead (closed so it refuses connections),
	// forcing in-group failover to the live one.
	dead := newIDBackend("DEAD", nil)
	deadURL := dead.url
	dead.close() // now refuses connections -> connect error, retryable
	live := newIDBackend("LIVE", nil)
	defer live.close()

	cfg := baseConfig("round_robin", backendCfgs(def))
	cfg.Retry = config.RetryConfig{MaxAttempts: 1}
	cfg.Routes = []config.RouteConfig{
		{
			Name:       "svc",
			PathPrefix: "/svc",
			Algorithm:  "round_robin",
			Backends: []config.BackendConfig{
				{URL: deadURL, Weight: 1, MaxConns: 100},
				{URL: live.url, Weight: 1, MaxConns: 100},
			},
		},
	}

	srv := New(cfg, "")
	h := srv.Handler()

	// Issue several routed requests. round_robin will sometimes pick the dead
	// backend first; failover must recover to LIVE within the same group. The
	// default group's DEFAULT backend must never serve any of these.
	for i := 0; i < 20; i++ {
		id, code := routedReq(t, h, http.MethodGet, "proxy.test", "/svc/op")
		if code != http.StatusOK {
			t.Fatalf("routed request %d: status %d (want 200 via in-group failover)", i, code)
		}
		if id != "LIVE" {
			t.Fatalf("routed request %d served by %q, want LIVE (in-group failover only)", i, id)
		}
	}

	if def.hitCount() != 0 {
		t.Fatalf("default backend received %d routed requests; failover crossed groups", def.hitCount())
	}
}
