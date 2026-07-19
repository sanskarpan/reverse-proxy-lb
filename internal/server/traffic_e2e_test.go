package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"reverse-proxy-lb/internal/config"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file drives §9 traffic management end-to-end through the real server
// stack (Server.Handler(), i.e. proxy + full middleware chain) via httptest.
// It covers canary splitting, request mirroring (shadow traffic), request/
// response rewriting with HTTPS redirect, fault injection (abort + delay), and
// the compression content-type allowlist. Backends identify themselves in the
// response body so assertions can tell which pool/backend served each request.
// No assertions are weakened to force a pass.

// -----------------------------------------------------------------------------
// §9.1 canary traffic splitting
// -----------------------------------------------------------------------------

// canaryConfig builds a config whose default pool is defBackends and whose
// canary pool is canaryBackends at the given weight percent.
func canaryConfig(weightPercent int, def, canary []config.BackendConfig) *config.Config {
	cfg := baseConfig("round_robin", def)
	cfg.Canary = config.CanaryConfig{
		Enabled:       true,
		WeightPercent: weightPercent,
		Algorithm:     "round_robin",
		Backends:      canary,
	}
	return cfg
}

// countCanary fires n requests through h and returns how many landed on a
// canary backend (id prefixed "CANARY") vs a default backend (id prefixed
// "DEFAULT"). Any other/failed outcome fails the test.
func countCanary(t *testing.T, h http.Handler, n int) (canaryHits, defaultHits int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id, code, _ := doReq(t, h, "")
		if code != http.StatusOK {
			t.Fatalf("canary request %d got status %d", i, code)
		}
		switch {
		case strings.HasPrefix(id, "CANARY"):
			canaryHits++
		case strings.HasPrefix(id, "DEFAULT"):
			defaultHits++
		default:
			t.Fatalf("canary request %d served by unexpected backend %q", i, id)
		}
	}
	return canaryHits, defaultHits
}

// TestE2E_Canary_SplitApproxHalf verifies a ~50% canary weight sends roughly
// half of a large request stream to the distinct canary pool and half to the
// default pool.
func TestE2E_Canary_SplitApproxHalf(t *testing.T) {
	def1 := newIDBackend("DEFAULT-1", nil)
	def2 := newIDBackend("DEFAULT-2", nil)
	can1 := newIDBackend("CANARY-1", nil)
	can2 := newIDBackend("CANARY-2", nil)
	defer def1.close()
	defer def2.close()
	defer can1.close()
	defer can2.close()

	cfg := canaryConfig(50, backendCfgs(def1, def2), backendCfgs(can1, can2))
	srv := New(cfg, "")
	h := srv.Handler()

	const n = 2000
	canaryHits, defaultHits := countCanary(t, h, n)

	if canaryHits+defaultHits != n {
		t.Fatalf("canary split: accounted %d hits, want %d", canaryHits+defaultHits, n)
	}
	want := n / 2
	tol := n / 10 // 10% of total
	if abs(canaryHits-want) > tol {
		t.Errorf("canary split: canary got %d/%d hits, want ~%d (+-%d)", canaryHits, n, want, tol)
	}
	if abs(defaultHits-want) > tol {
		t.Errorf("canary split: default got %d/%d hits, want ~%d (+-%d)", defaultHits, n, want, tol)
	}
}

// TestE2E_Canary_WeightZeroNone verifies weight 0 routes NO traffic to the
// canary pool (all requests stay on the default pool). This is deterministic.
func TestE2E_Canary_WeightZeroNone(t *testing.T) {
	def := newIDBackend("DEFAULT-1", nil)
	can := newIDBackend("CANARY-1", nil)
	defer def.close()
	defer can.close()

	cfg := canaryConfig(0, backendCfgs(def), backendCfgs(can))
	srv := New(cfg, "")
	h := srv.Handler()

	const n = 500
	canaryHits, defaultHits := countCanary(t, h, n)
	if canaryHits != 0 {
		t.Errorf("canary weight 0: canary received %d hits, want 0", canaryHits)
	}
	if defaultHits != n {
		t.Errorf("canary weight 0: default received %d hits, want %d", defaultHits, n)
	}
}

// TestE2E_Canary_WeightHundredAll verifies weight 100 routes ALL traffic to the
// canary pool. This is deterministic.
func TestE2E_Canary_WeightHundredAll(t *testing.T) {
	def := newIDBackend("DEFAULT-1", nil)
	can := newIDBackend("CANARY-1", nil)
	defer def.close()
	defer can.close()

	cfg := canaryConfig(100, backendCfgs(def), backendCfgs(can))
	srv := New(cfg, "")
	h := srv.Handler()

	const n = 500
	canaryHits, defaultHits := countCanary(t, h, n)
	if defaultHits != 0 {
		t.Errorf("canary weight 100: default received %d hits, want 0", defaultHits)
	}
	if canaryHits != n {
		t.Errorf("canary weight 100: canary received %d hits, want %d", canaryHits, n)
	}
}

// -----------------------------------------------------------------------------
// §9 request mirroring (shadow traffic)
// -----------------------------------------------------------------------------

// TestE2E_Mirror_ShadowsToTargetWithoutAffectingClient verifies that with
// sample_percent=100 the mirror target receives a shadow copy of every request
// while the client still gets the PRIMARY backend's response.
func TestE2E_Mirror_ShadowsToTargetWithoutAffectingClient(t *testing.T) {
	primary := newIDBackend("PRIMARY", nil)
	defer primary.close()

	var mirrorHits int64
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&mirrorHits, 1)
		// Drain the body so the shadow request is faithfully consumed.
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer mirror.Close()

	cfg := baseConfig("round_robin", backendCfgs(primary))
	cfg.Mirror = config.MirrorConfig{
		Enabled:       true,
		URL:           mirror.URL,
		SamplePercent: 100,
		Timeout:       2 * time.Second,
	}
	srv := New(cfg, "")
	h := srv.Handler()

	const n = 50
	for i := 0; i < n; i++ {
		id, code, _ := doReq(t, h, "")
		if code != http.StatusOK {
			t.Fatalf("mirror request %d got status %d", i, code)
		}
		if id != "PRIMARY" {
			t.Fatalf("mirror request %d served by %q, want PRIMARY (client must get primary response)", i, id)
		}
	}

	// Mirroring is fire-and-forget on background goroutines; wait for the shadow
	// copies to arrive at the mirror target.
	if !waitFor(2*time.Second, func() bool { return atomic.LoadInt64(&mirrorHits) == n }) {
		t.Fatalf("mirror target saw %d shadow requests, want %d", atomic.LoadInt64(&mirrorHits), n)
	}
}

// TestE2E_Mirror_FailingMirrorDoesNotBreakClient verifies that a mirror target
// which is down (connection refused) never breaks or fails the primary request:
// the client still receives the primary backend's 200 response.
func TestE2E_Mirror_FailingMirrorDoesNotBreakClient(t *testing.T) {
	primary := newIDBackend("PRIMARY", nil)
	defer primary.close()

	// Stand up a mirror target then immediately close it so its URL refuses
	// connections: every shadow copy will error out on the background goroutine.
	deadMirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := deadMirror.URL
	deadMirror.Close()

	cfg := baseConfig("round_robin", backendCfgs(primary))
	cfg.Mirror = config.MirrorConfig{
		Enabled:       true,
		URL:           deadURL,
		SamplePercent: 100,
		Timeout:       500 * time.Millisecond,
	}
	srv := New(cfg, "")
	h := srv.Handler()

	for i := 0; i < 30; i++ {
		id, code, _ := doReq(t, h, "")
		if code != http.StatusOK {
			t.Fatalf("failing-mirror request %d got status %d; a broken mirror must not affect the client", i, code)
		}
		if id != "PRIMARY" {
			t.Fatalf("failing-mirror request %d served by %q, want PRIMARY", i, id)
		}
	}
}

// -----------------------------------------------------------------------------
// §9 rewrite + HTTPS redirect
// -----------------------------------------------------------------------------

// echoBackend captures what the upstream actually received (path + a chosen
// request header) and lets the test set a response header the client should see.
type echoBackend struct {
	server        *httptest.Server
	url           string
	mu            sync.Mutex
	lastPath      string
	lastReqHeader http.Header
}

func newEchoBackend(respHeaders map[string]string) *echoBackend {
	b := &echoBackend{}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.lastPath = r.URL.Path
		b.lastReqHeader = r.Header.Clone()
		b.mu.Unlock()
		for k, v := range respHeaders {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ECHO")
	}))
	b.url = b.server.URL
	return b
}

func (b *echoBackend) close() { b.server.Close() }

func (b *echoBackend) snapshot() (string, http.Header) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastPath, b.lastReqHeader.Clone()
}

// TestE2E_Rewrite_HeadersAndPath verifies request-header set/remove reaches the
// backend, response-header set/remove reaches the client, and strip_path_prefix
// changes the upstream path.
func TestE2E_Rewrite_HeadersAndPath(t *testing.T) {
	// Backend emits X-Backend-Secret (to be removed) and returns 200. The proxy
	// rewrite should remove that response header and add X-Proxy-Added.
	be := newEchoBackend(map[string]string{"X-Backend-Secret": "leak"})
	defer be.close()

	cfg := baseConfig("round_robin", backendCfgs2(be.url))
	cfg.Rewrite = config.RewriteConfig{
		RequestHeadersSet:     map[string]string{"X-Injected": "yes"},
		RequestHeadersRemove:  []string{"X-Client-Drop"},
		ResponseHeadersSet:    map[string]string{"X-Proxy-Added": "added"},
		ResponseHeadersRemove: []string{"X-Backend-Secret"},
		StripPathPrefix:       "/strip",
	}
	srv := New(cfg, "")
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "http://proxy.test/strip/real/path", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Client-Drop", "should-be-removed")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	_, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("rewrite request got status %d, want 200", res.StatusCode)
	}

	// Request side: header set reached backend, removed header did not, path stripped.
	gotPath, gotReqHeader := be.snapshot()
	if gotPath != "/real/path" {
		t.Errorf("strip_path_prefix: backend saw path %q, want %q", gotPath, "/real/path")
	}
	if v := gotReqHeader.Get("X-Injected"); v != "yes" {
		t.Errorf("request_headers_set: backend saw X-Injected=%q, want %q", v, "yes")
	}
	if v := gotReqHeader.Get("X-Client-Drop"); v != "" {
		t.Errorf("request_headers_remove: backend still saw X-Client-Drop=%q, want removed", v)
	}

	// Response side: header set reached client, removed header did not.
	if v := res.Header.Get("X-Proxy-Added"); v != "added" {
		t.Errorf("response_headers_set: client saw X-Proxy-Added=%q, want %q", v, "added")
	}
	if v := res.Header.Get("X-Backend-Secret"); v != "" {
		t.Errorf("response_headers_remove: client still saw X-Backend-Secret=%q, want removed", v)
	}
}

// TestE2E_Rewrite_HTTPSRedirect verifies that a plain HTTP request with
// https_redirect enabled gets a 308 redirect to the https equivalent and never
// reaches the backend.
func TestE2E_Rewrite_HTTPSRedirect(t *testing.T) {
	be := newEchoBackend(nil)
	defer be.close()

	cfg := baseConfig("round_robin", backendCfgs2(be.url))
	cfg.Rewrite = config.RewriteConfig{HTTPSRedirect: true}
	srv := New(cfg, "")
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "http://proxy.test/secure/thing?q=1", nil)
	req.Host = "proxy.test"
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	_, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()

	if res.StatusCode != http.StatusPermanentRedirect {
		t.Fatalf("https_redirect: got status %d, want 308", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); loc != "https://proxy.test/secure/thing?q=1" {
		t.Errorf("https_redirect: Location=%q, want %q", loc, "https://proxy.test/secure/thing?q=1")
	}
	// The backend must NOT have been reached: the redirect short-circuits.
	if path, _ := be.snapshot(); path != "" {
		t.Errorf("https_redirect: backend was reached (path %q); redirect should short-circuit", path)
	}
}

// -----------------------------------------------------------------------------
// §9 fault injection
// -----------------------------------------------------------------------------

// TestE2E_FaultInjection_AbortAll verifies abort_percent=100 makes every request
// return the configured abort status and never reach the backend.
func TestE2E_FaultInjection_AbortAll(t *testing.T) {
	be := newIDBackend("BACKEND", nil)
	defer be.close()

	cfg := baseConfig("round_robin", backendCfgs(be))
	cfg.FaultInjection = config.FaultConfig{
		Enabled:      true,
		AbortPercent: 100,
		AbortStatus:  http.StatusTeapot, // 418, a distinctive configured status
	}
	srv := New(cfg, "")
	h := srv.Handler()

	const n = 50
	for i := 0; i < n; i++ {
		_, code, _ := doReq(t, h, "")
		if code != http.StatusTeapot {
			t.Fatalf("fault abort request %d got status %d, want 418", i, code)
		}
	}
	if be.hitCount() != 0 {
		t.Errorf("fault abort: backend received %d requests, want 0 (all aborted)", be.hitCount())
	}
}

// TestE2E_FaultInjection_DelayAddsLatency verifies delay_percent=100 with a
// configured delay adds at least that latency to every request while still
// serving the backend response.
func TestE2E_FaultInjection_DelayAddsLatency(t *testing.T) {
	be := newIDBackend("BACKEND", nil)
	defer be.close()

	const delay = 120 * time.Millisecond
	cfg := baseConfig("round_robin", backendCfgs(be))
	cfg.FaultInjection = config.FaultConfig{
		Enabled:      true,
		DelayPercent: 100,
		Delay:        delay,
	}
	srv := New(cfg, "")
	h := srv.Handler()

	const n = 10
	for i := 0; i < n; i++ {
		start := time.Now()
		id, code, _ := doReq(t, h, "")
		elapsed := time.Since(start)
		if code != http.StatusOK {
			t.Fatalf("fault delay request %d got status %d, want 200", i, code)
		}
		if id != "BACKEND" {
			t.Fatalf("fault delay request %d served by %q, want BACKEND", i, id)
		}
		if elapsed < delay {
			t.Errorf("fault delay request %d elapsed %v, want >= %v", i, elapsed, delay)
		}
	}
}

// -----------------------------------------------------------------------------
// §9 compression polish: content-type allowlist
// -----------------------------------------------------------------------------

// ctBackend returns a body of the given content-type. The body is large enough
// that gzip is clearly worthwhile.
func ctBackend(contentType string) *httptest.Server {
	body := strings.Repeat("compress-me-please ", 200)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
}

// gzipReq issues a request with Accept-Encoding: gzip through h and reports
// whether the response was gzip-encoded (and validates decodability when it is).
func gzipReq(t *testing.T, h http.Handler) (encoded bool, status int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	raw, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()

	if res.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("response claimed gzip but body is not decodable: %v", err)
		}
		if _, err := io.ReadAll(gz); err != nil {
			t.Fatalf("gzip body failed to decompress: %v", err)
		}
		return true, res.StatusCode
	}
	return false, res.StatusCode
}

// TestE2E_Compression_ContentTypeAllowlist verifies a content-type NOT on the
// allowlist is not gzipped even with Accept-Encoding: gzip, while an allowlisted
// one is.
func TestE2E_Compression_ContentTypeAllowlist(t *testing.T) {
	// Non-allowlisted content type: image/png should not be gzipped.
	notAllowed := ctBackend("image/png")
	defer notAllowed.Close()

	cfgNo := baseConfig("round_robin", backendCfgs2(notAllowed.URL))
	cfgNo.Compression = config.CompressionConfig{
		Enabled:      true,
		ContentTypes: []string{"application/json", "text/plain"},
	}
	srvNo := New(cfgNo, "")
	encoded, code := gzipReq(t, srvNo.Handler())
	if code != http.StatusOK {
		t.Fatalf("non-allowlisted compression request got status %d", code)
	}
	if encoded {
		t.Errorf("compression: image/png was gzipped, but it is not on the allowlist")
	}

	// Allowlisted content type: text/plain should be gzipped.
	allowed := ctBackend("text/plain; charset=utf-8")
	defer allowed.Close()

	cfgYes := baseConfig("round_robin", backendCfgs2(allowed.URL))
	cfgYes.Compression = config.CompressionConfig{
		Enabled:      true,
		ContentTypes: []string{"application/json", "text/plain"},
	}
	srvYes := New(cfgYes, "")
	encodedYes, codeYes := gzipReq(t, srvYes.Handler())
	if codeYes != http.StatusOK {
		t.Fatalf("allowlisted compression request got status %d", codeYes)
	}
	if !encodedYes {
		t.Errorf("compression: text/plain was NOT gzipped, but it is on the allowlist")
	}
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

// backendCfgs2 builds a single-backend config slice from a raw URL (for backends
// that are not *idBackend).
func backendCfgs2(url string) []config.BackendConfig {
	return []config.BackendConfig{{URL: url, Weight: 1, MaxConns: 100}}
}

// waitFor polls cond until it is true or the timeout elapses.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
