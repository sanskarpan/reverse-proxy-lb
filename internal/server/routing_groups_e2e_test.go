package server

import (
	"net/http"
	"net/http/httptest"
	"reverse-proxy-lb/internal/config"
	"testing"
)

// This file drives §L7 request routing end-to-end through the real server stack
// (Server.Handler(), i.e. proxy + full middleware chain) with multiple httptest
// backend pools. Every backend reports which pool it belongs to via its response
// body, so assertions are made purely by backend identity: each scenario proves
// that a request lands in the correct group's backend and never in another
// group's backend. No assertions are weakened to force a pass.
//
// It complements routing_e2e_test.go by covering the task-specified shape:
//   - two distinct Host-routed pools plus a distinct default pool,
//   - path-prefix routing with default fallthrough,
//   - header-based (X-Canary) AND method-based canary routing,
//   - explicit default fallback, and
//   - in-group failover that provably stays within the matched pool.

// hdrReq issues a request with the given method/host/path and optional headers
// through handler and returns the backend id (response body) and status code.
func hdrReq(t *testing.T, handler http.Handler, method, host, path string, headers map[string]string) (string, int) {
	t.Helper()
	req := httptest.NewRequest(method, "http://"+host+path, nil)
	req.Host = host
	req.RemoteAddr = "127.0.0.1:12345"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	res := rec.Result()
	body := readBodyTrim(res)
	return body, res.StatusCode
}

// readBodyTrim reads and closes res.Body, returning the trimmed string body.
func readBodyTrim(res *http.Response) string {
	buf := make([]byte, 0, 64)
	tmp := make([]byte, 512)
	for {
		n, err := res.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	_ = res.Body.Close()
	// Trim surrounding whitespace/newlines.
	start, end := 0, len(buf)
	for start < end && (buf[start] == ' ' || buf[start] == '\n' || buf[start] == '\r' || buf[start] == '\t') {
		start++
	}
	for end > start && (buf[end-1] == ' ' || buf[end-1] == '\n' || buf[end-1] == '\r' || buf[end-1] == '\t') {
		end--
	}
	return string(buf[start:end])
}

// TestE2E_Routing_HostPools verifies Host-based routing to two distinct pools,
// both of which are distinct from the default pool. api.example.com -> pool A;
// web.example.com -> pool B; any other host -> default pool. Assertions are by
// backend identity, and each off-path pool must receive zero traffic.
func TestE2E_Routing_HostPools(t *testing.T) {
	poolA := newIDBackend("POOL_A", nil)
	poolB := newIDBackend("POOL_B", nil)
	def := newIDBackend("DEFAULT", nil)
	defer poolA.close()
	defer poolB.close()
	defer def.close()

	cfg := baseConfig("round_robin", backendCfgs(def))
	cfg.Routes = []config.RouteConfig{
		{
			Name:      "api-host",
			Host:      "api.example.com",
			Algorithm: "round_robin",
			Backends:  backendCfgs(poolA),
		},
		{
			Name:      "web-host",
			Host:      "web.example.com",
			Algorithm: "round_robin",
			Backends:  backendCfgs(poolB),
		},
	}

	h := New(cfg, "").Handler()

	if id, code := hdrReq(t, h, http.MethodGet, "api.example.com", "/anything", nil); code != http.StatusOK || id != "POOL_A" {
		t.Fatalf("api.example.com: got id=%q code=%d, want POOL_A/200", id, code)
	}
	if id, code := hdrReq(t, h, http.MethodGet, "web.example.com", "/anything", nil); code != http.StatusOK || id != "POOL_B" {
		t.Fatalf("web.example.com: got id=%q code=%d, want POOL_B/200", id, code)
	}
	if id, code := hdrReq(t, h, http.MethodGet, "other.example.com", "/anything", nil); code != http.StatusOK || id != "DEFAULT" {
		t.Fatalf("other.example.com: got id=%q code=%d, want DEFAULT/200", id, code)
	}

	// Cross-pool leakage check: A's host never reached B or default and vice versa.
	if poolA.hitCount() != 1 {
		t.Fatalf("pool A received %d hits, want exactly 1 (api.example.com only)", poolA.hitCount())
	}
	if poolB.hitCount() != 1 {
		t.Fatalf("pool B received %d hits, want exactly 1 (web.example.com only)", poolB.hitCount())
	}
	if def.hitCount() != 1 {
		t.Fatalf("default received %d hits, want exactly 1 (unmatched host only)", def.hitCount())
	}
}

// TestE2E_Routing_PathPrefixPool verifies path-prefix routing: /api/* goes to
// pool A, and everything else falls through to the default pool. Verified by
// backend identity and per-pool hit isolation.
func TestE2E_Routing_PathPrefixPool(t *testing.T) {
	poolA := newIDBackend("POOL_A", nil)
	def := newIDBackend("DEFAULT", nil)
	defer poolA.close()
	defer def.close()

	cfg := baseConfig("round_robin", backendCfgs(def))
	cfg.Routes = []config.RouteConfig{
		{
			Name:       "api-path",
			PathPrefix: "/api/",
			Algorithm:  "round_robin",
			Backends:   backendCfgs(poolA),
		},
	}

	h := New(cfg, "").Handler()

	for _, p := range []string{"/api/v1/users", "/api/health", "/api/"} {
		if id, code := hdrReq(t, h, http.MethodGet, "proxy.test", p, nil); code != http.StatusOK || id != "POOL_A" {
			t.Fatalf("path %q: got id=%q code=%d, want POOL_A/200", p, id, code)
		}
	}
	for _, p := range []string{"/web", "/", "/apiv2/x", "/other"} {
		if id, code := hdrReq(t, h, http.MethodGet, "proxy.test", p, nil); code != http.StatusOK || id != "DEFAULT" {
			t.Fatalf("path %q: got id=%q code=%d, want DEFAULT/200 (fallthrough)", p, id, code)
		}
	}

	if poolA.hitCount() != 3 {
		t.Fatalf("pool A received %d hits, want exactly 3 (/api/* only)", poolA.hitCount())
	}
	if def.hitCount() != 4 {
		t.Fatalf("default received %d hits, want exactly 4 (non-/api paths)", def.hitCount())
	}
}

// TestE2E_Routing_HeaderCanary verifies header-based canary routing: a request
// carrying X-Canary: yes routes to the canary pool; a request without it (or
// with a different value) falls through to the default pool.
func TestE2E_Routing_HeaderCanary(t *testing.T) {
	canary := newIDBackend("CANARY", nil)
	def := newIDBackend("DEFAULT", nil)
	defer canary.close()
	defer def.close()

	cfg := baseConfig("round_robin", backendCfgs(def))
	cfg.Routes = []config.RouteConfig{
		{
			Name:      "canary-header",
			Headers:   map[string]string{"X-Canary": "yes"},
			Algorithm: "round_robin",
			Backends:  backendCfgs(canary),
		},
	}

	h := New(cfg, "").Handler()

	if id, code := hdrReq(t, h, http.MethodGet, "proxy.test", "/x", map[string]string{"X-Canary": "yes"}); code != http.StatusOK || id != "CANARY" {
		t.Fatalf("X-Canary:yes: got id=%q code=%d, want CANARY/200", id, code)
	}
	// No header -> default.
	if id, code := hdrReq(t, h, http.MethodGet, "proxy.test", "/x", nil); code != http.StatusOK || id != "DEFAULT" {
		t.Fatalf("no header: got id=%q code=%d, want DEFAULT/200", id, code)
	}
	// Non-matching value -> default.
	if id, code := hdrReq(t, h, http.MethodGet, "proxy.test", "/x", map[string]string{"X-Canary": "no"}); code != http.StatusOK || id != "DEFAULT" {
		t.Fatalf("X-Canary:no: got id=%q code=%d, want DEFAULT/200", id, code)
	}

	if canary.hitCount() != 1 {
		t.Fatalf("canary received %d hits, want exactly 1 (X-Canary:yes only)", canary.hitCount())
	}
	if def.hitCount() != 2 {
		t.Fatalf("default received %d hits, want exactly 2 (non-matching header)", def.hitCount())
	}
}

// TestE2E_Routing_MethodCanary verifies method-based canary routing: POST goes
// to the canary pool; GET (and other non-matching methods) fall through to the
// default pool. This is the method-matching variant of the canary scenario.
func TestE2E_Routing_MethodCanary(t *testing.T) {
	canary := newIDBackend("CANARY", nil)
	def := newIDBackend("DEFAULT", nil)
	defer canary.close()
	defer def.close()

	cfg := baseConfig("round_robin", backendCfgs(def))
	cfg.Routes = []config.RouteConfig{
		{
			Name:      "canary-method",
			Methods:   []string{"POST"},
			Algorithm: "round_robin",
			Backends:  backendCfgs(canary),
		},
	}

	h := New(cfg, "").Handler()

	if id, code := hdrReq(t, h, http.MethodPost, "proxy.test", "/x", nil); code != http.StatusOK || id != "CANARY" {
		t.Fatalf("POST: got id=%q code=%d, want CANARY/200", id, code)
	}
	if id, code := hdrReq(t, h, http.MethodGet, "proxy.test", "/x", nil); code != http.StatusOK || id != "DEFAULT" {
		t.Fatalf("GET: got id=%q code=%d, want DEFAULT/200", id, code)
	}
	if id, code := hdrReq(t, h, http.MethodPut, "proxy.test", "/x", nil); code != http.StatusOK || id != "DEFAULT" {
		t.Fatalf("PUT: got id=%q code=%d, want DEFAULT/200 (non-matching method)", id, code)
	}

	if canary.hitCount() != 1 {
		t.Fatalf("canary received %d hits, want exactly 1 (POST only)", canary.hitCount())
	}
	if def.hitCount() != 2 {
		t.Fatalf("default received %d hits, want exactly 2 (GET+PUT)", def.hitCount())
	}
}

// TestE2E_Routing_DefaultFallback verifies that a request matching no configured
// route reaches the default backends, while every configured pool is left
// untouched. Routes span host, path, method, and header criteria so the request
// must dodge all of them.
func TestE2E_Routing_DefaultFallback(t *testing.T) {
	poolHost := newIDBackend("HOST_POOL", nil)
	poolPath := newIDBackend("PATH_POOL", nil)
	poolCanary := newIDBackend("CANARY_POOL", nil)
	def := newIDBackend("DEFAULT", nil)
	defer poolHost.close()
	defer poolPath.close()
	defer poolCanary.close()
	defer def.close()

	cfg := baseConfig("round_robin", backendCfgs(def))
	cfg.Routes = []config.RouteConfig{
		{Name: "h", Host: "api.example.com", Algorithm: "round_robin", Backends: backendCfgs(poolHost)},
		{Name: "p", PathPrefix: "/api/", Algorithm: "round_robin", Backends: backendCfgs(poolPath)},
		{Name: "c", Methods: []string{"POST"}, Headers: map[string]string{"X-Canary": "yes"}, Algorithm: "round_robin", Backends: backendCfgs(poolCanary)},
	}

	h := New(cfg, "").Handler()

	// GET to a non-api host on a non-/api path with no canary header: matches nothing.
	if id, code := hdrReq(t, h, http.MethodGet, "plain.example.com", "/home", nil); code != http.StatusOK || id != "DEFAULT" {
		t.Fatalf("unmatched request: got id=%q code=%d, want DEFAULT/200", id, code)
	}

	if poolHost.hitCount() != 0 || poolPath.hitCount() != 0 || poolCanary.hitCount() != 0 {
		t.Fatalf("configured pools received traffic on a default-fallback request: host=%d path=%d canary=%d",
			poolHost.hitCount(), poolPath.hitCount(), poolCanary.hitCount())
	}
	if def.hitCount() != 1 {
		t.Fatalf("default received %d hits, want exactly 1", def.hitCount())
	}
}

// TestE2E_Routing_FailoverStaysInGroup verifies that when the matched pool holds
// one dead and one live backend, the request fails over to the live backend IN
// THAT POOL and never to any other pool's backend. Two matched pools are set up
// (one keyed by host, one by path), each with a dead+live pair, plus a default
// pool. A dead backend in the matched pool must never divert traffic to the
// default pool or the sibling matched pool.
func TestE2E_Routing_FailoverStaysInGroup(t *testing.T) {
	// Default pool: must never serve a routed request.
	def := newIDBackend("DEFAULT_LIVE", nil)
	defer def.close()

	// Host pool: dead + live.
	hostDead := newIDBackend("HOST_DEAD", nil)
	hostDeadURL := hostDead.url
	hostDead.close() // refuses connections -> retryable connect error
	hostLive := newIDBackend("HOST_LIVE", nil)
	defer hostLive.close()

	// Path pool: dead + live (a DIFFERENT live backend, to prove group isolation).
	pathDead := newIDBackend("PATH_DEAD", nil)
	pathDeadURL := pathDead.url
	pathDead.close()
	pathLive := newIDBackend("PATH_LIVE", nil)
	defer pathLive.close()

	cfg := baseConfig("round_robin", backendCfgs(def))
	// One retry per attempt budget beyond the first pick so failover can reach the
	// live sibling in the same group.
	cfg.Retry = config.RetryConfig{MaxAttempts: 1}
	cfg.Routes = []config.RouteConfig{
		{
			Name:      "host-pool",
			Host:      "api.example.com",
			Algorithm: "round_robin",
			Backends: []config.BackendConfig{
				{URL: hostDeadURL, Weight: 1, MaxConns: 100},
				{URL: hostLive.url, Weight: 1, MaxConns: 100},
			},
		},
		{
			Name:       "path-pool",
			PathPrefix: "/svc/",
			Algorithm:  "round_robin",
			Backends: []config.BackendConfig{
				{URL: pathDeadURL, Weight: 1, MaxConns: 100},
				{URL: pathLive.url, Weight: 1, MaxConns: 100},
			},
		},
	}

	h := New(cfg, "").Handler()

	// Drive many requests so round_robin repeatedly picks the dead backend first;
	// every one must recover to the live backend IN THE MATCHED GROUP.
	const iters = 30
	for i := 0; i < iters; i++ {
		// Host-matched request: must end on HOST_LIVE only.
		if id, code := hdrReq(t, h, http.MethodGet, "api.example.com", "/whatever", nil); code != http.StatusOK || id != "HOST_LIVE" {
			t.Fatalf("host req %d: got id=%q code=%d, want HOST_LIVE/200 (in-group failover)", i, id, code)
		}
		// Path-matched request: must end on PATH_LIVE only.
		if id, code := hdrReq(t, h, http.MethodGet, "proxy.test", "/svc/op", nil); code != http.StatusOK || id != "PATH_LIVE" {
			t.Fatalf("path req %d: got id=%q code=%d, want PATH_LIVE/200 (in-group failover)", i, id, code)
		}
	}

	// Cross-group leakage assertions: the default pool and the sibling live pools
	// must never have served the other group's traffic.
	if def.hitCount() != 0 {
		t.Fatalf("default pool served %d routed requests; failover crossed into the default group", def.hitCount())
	}
	if hostLive.hitCount() != iters {
		t.Fatalf("HOST_LIVE served %d requests, want %d (host-pool failover must land here every time)", hostLive.hitCount(), iters)
	}
	if pathLive.hitCount() != iters {
		t.Fatalf("PATH_LIVE served %d requests, want %d (path-pool failover must land here every time)", pathLive.hitCount(), iters)
	}
}
