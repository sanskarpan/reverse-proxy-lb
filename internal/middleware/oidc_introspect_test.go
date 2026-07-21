package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
)

// oidcOKHandler is the downstream handler used in OIDC introspection tests.
var oidcOKHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
})

// newIntrospectionServer starts an httptest server that serves a single RFC 7662
// response body. calls is atomically incremented each time the endpoint is hit.
func newIntrospectionServer(t *testing.T, resp map[string]interface{}, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("introspection server: encode: %v", err)
		}
	}))
}

// serveOIDC builds a middleware from cfg and serves r, returning the recorder.
func serveOIDC(cfg config.OIDCIntrospectionConfig, r *http.Request) *httptest.ResponseRecorder {
	mw := OIDCIntrospect(cfg)
	rec := httptest.NewRecorder()
	mw(oidcOKHandler).ServeHTTP(rec, r)
	return rec
}

// TestOIDCIntrospectValidToken: stub server returns active=true, expects 200.
func TestOIDCIntrospectValidToken(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	srv := newIntrospectionServer(t, map[string]interface{}{
		"active": true,
		"scope":  "read write",
		"exp":    future,
		"sub":    "user1",
	}, nil)
	defer srv.Close()

	cfg := config.OIDCIntrospectionConfig{
		Enabled:          true,
		IntrospectionURL: srv.URL,
		CacheTTL:         30 * time.Second,
		CacheSize:        100,
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer valid-token")
	rec := serveOIDC(cfg, r)
	if rec.Code != http.StatusOK {
		t.Errorf("valid token: status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("valid token: body = %q, want ok", rec.Body.String())
	}
}

// TestOIDCIntrospectInactiveToken: server returns active=false, expects 401.
func TestOIDCIntrospectInactiveToken(t *testing.T) {
	srv := newIntrospectionServer(t, map[string]interface{}{
		"active": false,
	}, nil)
	defer srv.Close()

	cfg := config.OIDCIntrospectionConfig{
		Enabled:          true,
		IntrospectionURL: srv.URL,
		CacheTTL:         30 * time.Second,
		CacheSize:        100,
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer inactive-token")
	rec := serveOIDC(cfg, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("inactive token: status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("inactive token: missing WWW-Authenticate header")
	}
}

// TestOIDCIntrospectExpiredToken: server returns active=true but exp in past, expects 401.
func TestOIDCIntrospectExpiredToken(t *testing.T) {
	past := time.Now().Add(-time.Hour).Unix()
	srv := newIntrospectionServer(t, map[string]interface{}{
		"active": true,
		"exp":    past,
		"sub":    "user1",
	}, nil)
	defer srv.Close()

	cfg := config.OIDCIntrospectionConfig{
		Enabled:          true,
		IntrospectionURL: srv.URL,
		CacheTTL:         30 * time.Second,
		CacheSize:        100,
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer expired-token")
	rec := serveOIDC(cfg, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expired token: status = %d, want 401", rec.Code)
	}
}

// TestOIDCIntrospectCacheHit: first request hits server, second uses cache.
// Asserts the server is called exactly once.
func TestOIDCIntrospectCacheHit(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	var calls atomic.Int64
	srv := newIntrospectionServer(t, map[string]interface{}{
		"active": true,
		"exp":    future,
		"sub":    "user1",
	}, &calls)
	defer srv.Close()

	cfg := config.OIDCIntrospectionConfig{
		Enabled:          true,
		IntrospectionURL: srv.URL,
		CacheTTL:         30 * time.Second,
		CacheSize:        100,
	}

	// Build the middleware once so both requests share the same cache.
	mw := OIDCIntrospect(cfg)

	makeReq := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer cache-test-token")
		rec := httptest.NewRecorder()
		mw(oidcOKHandler).ServeHTTP(rec, r)
		return rec
	}

	rec1 := makeReq()
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", rec1.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("after first request: server calls = %d, want 1", calls.Load())
	}

	rec2 := makeReq()
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request (cache hit): status = %d, want 200", rec2.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("after second request: server calls = %d, want 1 (cache hit)", calls.Load())
	}
}

// TestOIDCIntrospectScopeMismatch: token missing a required scope, expects 401.
func TestOIDCIntrospectScopeMismatch(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	srv := newIntrospectionServer(t, map[string]interface{}{
		"active": true,
		"exp":    future,
		"scope":  "read",
		"sub":    "user1",
	}, nil)
	defer srv.Close()

	cfg := config.OIDCIntrospectionConfig{
		Enabled:          true,
		IntrospectionURL: srv.URL,
		CacheTTL:         30 * time.Second,
		CacheSize:        100,
		ScopesRequired:   []string{"read", "write"},
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer scope-test-token")
	rec := serveOIDC(cfg, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("scope mismatch: status = %d, want 401", rec.Code)
	}
}

// TestOIDCIntrospectNetworkError: introspection server is down, expects 503.
func TestOIDCIntrospectNetworkError(t *testing.T) {
	// Start and immediately stop a server to get a URL that refuses connections.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	cfg := config.OIDCIntrospectionConfig{
		Enabled:          true,
		IntrospectionURL: srv.URL,
		CacheTTL:         30 * time.Second,
		CacheSize:        100,
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer any-token")
	rec := serveOIDC(cfg, r)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("network error: status = %d, want 503", rec.Code)
	}
}

// TestOIDCIntrospectMissingBearer: no Authorization header, expects 401.
func TestOIDCIntrospectMissingBearer(t *testing.T) {
	cfg := config.OIDCIntrospectionConfig{
		Enabled:          true,
		IntrospectionURL: "http://does-not-matter",
		CacheTTL:         30 * time.Second,
		CacheSize:        100,
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	// No Authorization header.
	rec := serveOIDC(cfg, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing bearer: status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("missing bearer: missing WWW-Authenticate header")
	}
}

// TestOIDCIntrospectDisabledIsNoop: when Enabled=false the middleware is a no-op.
func TestOIDCIntrospectDisabledIsNoop(t *testing.T) {
	cfg := config.OIDCIntrospectionConfig{
		Enabled: false,
	}
	// No Authorization header — should still pass through.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := serveOIDC(cfg, r)
	if rec.Code != http.StatusOK {
		t.Errorf("disabled: status = %d, want 200", rec.Code)
	}
}
