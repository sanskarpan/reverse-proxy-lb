package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/config"
	"reverse-proxy-lb/internal/discovery"
)

// This file drives three integrated subsystems end-to-end through the real server
// stack: the HTTP response CACHE (through Server.Handler(), i.e. proxy + the full
// middleware chain), DNS service DISCOVERY (a discovery.Discoverer syncing into
// the very balancer the running Server serves from, over an injected FAKE
// resolver), and the ADMIN API (through Server.MetricsMux(), the same admin
// ServeMux the server serves on its metrics listener). Requests are driven via
// httptest without binding a real listener. No assertions are weakened to force a
// pass.

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

// countingBackend is an httptest backend that counts the requests it receives and
// serves a caller-supplied handler, so cache tests can assert how many times the
// upstream was actually hit across multiple client requests.
type countingBackend struct {
	server *httptest.Server
	url    string
	hits   int64
}

func newCountingBackend(h http.HandlerFunc) *countingBackend {
	b := &countingBackend{}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&b.hits, 1)
		h(w, r)
	}))
	b.url = b.server.URL
	return b
}

func (b *countingBackend) hitCount() int64 { return atomic.LoadInt64(&b.hits) }
func (b *countingBackend) close()          { b.server.Close() }

// cacheGET issues a GET for path through handler with optional request headers,
// returning the status, response headers, and body. It uses a loopback peer so the
// full proxy path (trusted-proxy handling) behaves as in production.
func cacheGET(t *testing.T, handler http.Handler, path string, reqHeaders map[string]string) (int, http.Header, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://proxy.test"+path, nil)
	req.RemoteAddr = "127.0.0.1:12345"
	for k, v := range reqHeaders {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	return res.StatusCode, res.Header, string(body)
}

// cacheConfig returns a config wired to backend url with the response cache
// enabled at the given TTL, and everything else (health/metrics/rate-limit/
// compression) disabled so only the cache behavior is exercised. Compression is
// explicitly disabled so the cached body is compared byte-for-byte without gzip.
func cacheConfig(url string, ttl time.Duration) *config.Config {
	cfg := baseConfig("round_robin", []config.BackendConfig{{URL: url, Weight: 1, MaxConns: 100}})
	cfg.Cache = config.CacheConfig{
		Enabled:      true,
		DefaultTTL:   ttl,
		MaxEntries:   1000,
		MaxBodyBytes: 1 << 20,
		Methods:      []string{http.MethodGet, http.MethodHead},
	}
	return cfg
}

// adminReq issues an admin request through mux with an optional bearer token and
// returns status, response headers, and body.
func adminReq(t *testing.T, mux http.Handler, method, target, token string) (int, http.Header, string) {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	return res.StatusCode, res.Header, string(body)
}

// adminBackendURLs unmarshals GET /admin/backends and returns the sorted URL set.
func adminBackendURLs(t *testing.T, body string) []string {
	t.Helper()
	var out []adminBackend
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode /admin/backends: %v (body=%q)", err, body)
	}
	urls := make([]string, 0, len(out))
	for _, b := range out {
		urls = append(urls, b.URL)
	}
	sort.Strings(urls)
	return urls
}

// adminBackendByURL finds a single admin backend entry by URL, or fails.
func adminBackendEntry(t *testing.T, body, url string) adminBackend {
	t.Helper()
	var out []adminBackend
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode /admin/backends: %v", err)
	}
	for _, b := range out {
		if b.URL == url {
			return b
		}
	}
	t.Fatalf("backend %q not found in /admin/backends: %v", url, out)
	return adminBackend{}
}

// serverTestResolver is a programmable discovery.Resolver for server-level
// discovery tests. Its returned host set can be swapped concurrently while a
// Discoverer runs against the live server balancer.
type serverTestResolver struct {
	mu    sync.Mutex
	hosts map[string][]string
}

func newServerTestResolver() *serverTestResolver {
	return &serverTestResolver{hosts: make(map[string][]string)}
}

func (r *serverTestResolver) set(name string, hosts []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hosts[name] = hosts
}

func (r *serverTestResolver) LookupHost(name string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hosts[name]...), nil
}

func (r *serverTestResolver) LookupSRV(string) ([]discovery.Addr, error) { return nil, nil }

// balancerURLs returns the sorted URL set currently in the balancer group.
func balancerURLs(b balancer.Balancer) []string {
	all := b.All()
	urls := make([]string, 0, len(all))
	for _, be := range all {
		urls = append(urls, be.URL)
	}
	sort.Strings(urls)
	return urls
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------------------
// CACHE (through Server.Handler())
// -----------------------------------------------------------------------------

// TestE2E_Cache_CacheableGET_HitsBackendOnce verifies a cacheable GET is fetched
// from the backend exactly once across two client requests, and the second is
// served from cache (X-Cache: HIT) with an identical body.
func TestE2E_Cache_CacheableGET_HitsBackendOnce(t *testing.T) {
	be := newCountingBackend(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=300")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "payload-v1")
	})
	defer be.close()

	srv := New(cacheConfig(be.url, time.Minute), "")
	h := srv.Handler()

	code1, hdr1, body1 := cacheGET(t, h, "/thing", nil)
	if code1 != http.StatusOK {
		t.Fatalf("first request: status %d", code1)
	}
	if got := hdr1.Get("X-Cache"); got == "HIT" {
		t.Fatalf("first request should be a MISS, got X-Cache=%q", got)
	}
	if body1 != "payload-v1" {
		t.Fatalf("first body = %q, want payload-v1", body1)
	}

	code2, hdr2, body2 := cacheGET(t, h, "/thing", nil)
	if code2 != http.StatusOK {
		t.Fatalf("second request: status %d", code2)
	}
	if got := hdr2.Get("X-Cache"); got != "HIT" {
		t.Fatalf("second request X-Cache = %q, want HIT", got)
	}
	if body2 != body1 {
		t.Fatalf("cached body = %q, want %q (same as first)", body2, body1)
	}
	if n := be.hitCount(); n != 1 {
		t.Fatalf("backend hit %d times across 2 requests, want exactly 1", n)
	}
}

// TestE2E_Cache_NoStore_FetchedEveryTime verifies a response marked no-store is
// never cached: the backend is hit on every request and no HIT is ever served.
func TestE2E_Cache_NoStore_FetchedEveryTime(t *testing.T) {
	be := newCountingBackend(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "dynamic")
	})
	defer be.close()

	srv := New(cacheConfig(be.url, time.Minute), "")
	h := srv.Handler()

	for i := 0; i < 3; i++ {
		code, hdr, body := cacheGET(t, h, "/live", nil)
		if code != http.StatusOK {
			t.Fatalf("request %d: status %d", i, code)
		}
		if got := hdr.Get("X-Cache"); got == "HIT" {
			t.Fatalf("request %d unexpectedly served from cache (X-Cache=HIT)", i)
		}
		if body != "dynamic" {
			t.Fatalf("request %d body = %q", i, body)
		}
	}
	if n := be.hitCount(); n != 3 {
		t.Fatalf("no-store backend hit %d times, want 3 (fetched every time)", n)
	}
}

// TestE2E_Cache_SetCookie_FetchedEveryTime verifies a response carrying a
// Set-Cookie header is never cached (per-user response), so the backend is hit on
// every request.
func TestE2E_Cache_SetCookie_FetchedEveryTime(t *testing.T) {
	be := newCountingBackend(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=300")
		w.Header().Set("Set-Cookie", "sid=abc; Path=/")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "with-cookie")
	})
	defer be.close()

	srv := New(cacheConfig(be.url, time.Minute), "")
	h := srv.Handler()

	for i := 0; i < 3; i++ {
		code, hdr, _ := cacheGET(t, h, "/acct", nil)
		if code != http.StatusOK {
			t.Fatalf("request %d: status %d", i, code)
		}
		if got := hdr.Get("X-Cache"); got == "HIT" {
			t.Fatalf("request %d unexpectedly cached a Set-Cookie response", i)
		}
	}
	if n := be.hitCount(); n != 3 {
		t.Fatalf("Set-Cookie backend hit %d times, want 3", n)
	}
}

// TestE2E_Cache_TTLExpiry_Refetches verifies that once the TTL lapses the cache
// re-fetches from the backend rather than serving a stale HIT. A short TTL keeps
// the test fast and deterministic.
func TestE2E_Cache_TTLExpiry_Refetches(t *testing.T) {
	var version int64 = 1
	be := newCountingBackend(func(w http.ResponseWriter, r *http.Request) {
		v := atomic.LoadInt64(&version)
		// No explicit max-age: the cache uses the config DefaultTTL, which we set
		// short so expiry is observable.
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "v"+itoa(v))
	})
	defer be.close()

	srv := New(cacheConfig(be.url, 40*time.Millisecond), "")
	h := srv.Handler()

	// First: MISS, backend hit once.
	code, hdr, body := cacheGET(t, h, "/ttl", nil)
	if code != http.StatusOK || hdr.Get("X-Cache") == "HIT" || body != "v1" {
		t.Fatalf("first: code=%d xcache=%q body=%q", code, hdr.Get("X-Cache"), body)
	}
	// Second within TTL: HIT, backend NOT hit again.
	code, hdr, body = cacheGET(t, h, "/ttl", nil)
	if code != http.StatusOK || hdr.Get("X-Cache") != "HIT" || body != "v1" {
		t.Fatalf("second (fresh): code=%d xcache=%q body=%q", code, hdr.Get("X-Cache"), body)
	}
	if n := be.hitCount(); n != 1 {
		t.Fatalf("within TTL backend hit %d times, want 1", n)
	}

	// Bump the backend's version and let the entry expire.
	atomic.StoreInt64(&version, 2)
	waitUntil(t, 2*time.Second, func() bool {
		_, h2, b2 := cacheGET(t, h, "/ttl", nil)
		// Once expired, the next request is a re-fetch (not HIT) and returns v2.
		return h2.Get("X-Cache") != "HIT" && b2 == "v2"
	}, "TTL never expired: cache kept serving stale v1")

	if n := be.hitCount(); n < 2 {
		t.Fatalf("after TTL expiry backend hit %d times, want >=2 (re-fetched)", n)
	}
}

// TestE2E_Cache_ETag_Returns304 verifies conditional revalidation against a cached
// entry: a first request populates the cache with an ETag, and a follow-up
// carrying If-None-Match matching that ETag gets a 304 Not Modified from the cache
// without re-hitting the backend.
func TestE2E_Cache_ETag_Returns304(t *testing.T) {
	const etag = `"abc123"`
	be := newCountingBackend(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=300")
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "etag-body")
	})
	defer be.close()

	srv := New(cacheConfig(be.url, time.Minute), "")
	h := srv.Handler()

	// Prime the cache.
	code, hdr, body := cacheGET(t, h, "/doc", nil)
	if code != http.StatusOK || body != "etag-body" {
		t.Fatalf("prime: code=%d body=%q", code, body)
	}
	if hdr.Get("ETag") != etag {
		t.Fatalf("prime ETag = %q, want %q", hdr.Get("ETag"), etag)
	}

	// Conditional GET with matching validator -> 304 from cache, no backend hit.
	code, hdr, body = cacheGET(t, h, "/doc", map[string]string{"If-None-Match": etag})
	if code != http.StatusNotModified {
		t.Fatalf("conditional GET status = %d, want 304", code)
	}
	if body != "" {
		t.Fatalf("304 response should have empty body, got %q", body)
	}
	if hdr.Get("ETag") != etag {
		t.Fatalf("304 ETag = %q, want %q", hdr.Get("ETag"), etag)
	}
	if got := hdr.Get("X-Cache"); got != "HIT" {
		t.Fatalf("304 X-Cache = %q, want HIT (served from cache)", got)
	}
	if n := be.hitCount(); n != 1 {
		t.Fatalf("backend hit %d times, want exactly 1 (304 served from cache)", n)
	}
}

// -----------------------------------------------------------------------------
// DISCOVERY (Discoverer over a FAKE resolver, syncing the live server balancer)
// -----------------------------------------------------------------------------

// TestE2E_Discovery_SyncsIntoServerBalancer builds a real Server with one static
// backend, then attaches a Discoverer (over an injected fake resolver) to that
// server's live default balancer. It asserts the resolved backends appear in the
// balancer the Server actually serves from (and are visible via the admin API),
// that changing the fake's result syncs add/remove, and that the statically
// configured backend is never touched.
func TestE2E_Discovery_SyncsIntoServerBalancer(t *testing.T) {
	staticBE := newIDBackend("static", nil)
	defer staticBE.close()

	// Real server with health checks disabled so discovered backends are eligible
	// immediately and the static backend stays healthy.
	cfg := baseConfig("round_robin", backendCfgs(staticBE))
	srv := New(cfg, "")

	// Attach discovery to the SAME balancer the server serves from. This is the
	// production wiring path (discoverer syncs into the default group); we inject a
	// fake resolver rather than the stdlib one.
	fr := newServerTestResolver()
	fr.set("web.svc", []string{"10.0.0.1", "10.0.0.2"})
	// A short interval so the periodic re-resolve picks up the resolver swap below
	// quickly. Start() performs an immediate resolve, then ticks.
	target := config.DNSTarget{
		Name: "web.svc", Type: "a", Scheme: "http", Port: 8080,
		Interval: 5 * time.Millisecond, Weight: 1, MaxConns: 100,
	}
	d := discovery.NewDiscoverer(srv.GetBalancer(), []config.DNSTarget{target}, fr)
	d.Start()
	defer d.Stop()

	// Initial resolve: the two discovered backends plus the static one.
	wantInitial := []string{
		"http://10.0.0.1:8080", "http://10.0.0.2:8080", staticBE.url,
	}
	sort.Strings(wantInitial)
	waitUntil(t, time.Second, func() bool {
		return equalStrs(balancerURLs(srv.GetBalancer()), wantInitial)
	}, "initial discovery never synced the resolved backends into the server balancer")

	// The admin API (served off the same balancer) sees the discovered backends.
	mux := srv.MetricsMux()
	code, _, body := adminReq(t, mux, http.MethodGet, "/admin/backends", "")
	if code != http.StatusOK {
		t.Fatalf("/admin/backends status = %d", code)
	}
	if got := adminBackendURLs(t, body); !equalStrs(got, wantInitial) {
		t.Fatalf("/admin/backends = %v, want %v", got, wantInitial)
	}

	// Change the resolved set: drop .1, add .3; keep .2. The static backend must
	// remain untouched. The periodic resolve applies the change.
	fr.set("web.svc", []string{"10.0.0.2", "10.0.0.3"})

	wantAfter := []string{
		"http://10.0.0.2:8080", "http://10.0.0.3:8080", staticBE.url,
	}
	sort.Strings(wantAfter)
	waitUntil(t, time.Second, func() bool {
		return equalStrs(balancerURLs(srv.GetBalancer()), wantAfter)
	}, "discovery never synced the changed resolver result (add .3 / remove .1)")

	// The static backend is still present (never removed by discovery).
	if findBackend(srv, staticBE.url) == nil {
		t.Fatalf("static backend %q was removed by discovery", staticBE.url)
	}
}

// TestE2E_Discovery_StartStop_SyncsAndTearsDown exercises the goroutine lifecycle
// against the live server balancer: Start() resolves immediately (so discovered
// backends appear without a manual sync), and Stop() is clean and idempotent.
func TestE2E_Discovery_StartStop_SyncsAndTearsDown(t *testing.T) {
	cfg := baseConfig("round_robin", nil)
	srv := New(cfg, "")

	fr := newServerTestResolver()
	fr.set("api.svc", []string{"10.1.0.1"})
	target := config.DNSTarget{
		Name: "api.svc", Type: "a", Scheme: "http", Port: 9000,
		Interval: 5 * time.Millisecond, Weight: 1, MaxConns: 100,
	}
	d := discovery.NewDiscoverer(srv.GetBalancer(), []config.DNSTarget{target}, fr)
	d.Start()
	defer d.Stop()

	waitUntil(t, time.Second, func() bool {
		return equalStrs(balancerURLs(srv.GetBalancer()), []string{"http://10.1.0.1:9000"})
	}, "Start() did not sync the discovered backend into the server balancer")

	d.Stop()
	d.Stop() // idempotent
}

// -----------------------------------------------------------------------------
// ADMIN API (through Server.MetricsMux())
// -----------------------------------------------------------------------------

// TestE2E_Admin_ListBackends verifies GET /admin/backends returns every backend in
// the server's balancer group with its live health/weight.
func TestE2E_Admin_ListBackends(t *testing.T) {
	b1 := newIDBackend("A", nil)
	b2 := newIDBackend("B", nil)
	defer b1.close()
	defer b2.close()

	cfg := baseConfig("round_robin", backendCfgs(b1, b2))
	srv := New(cfg, "")
	mux := srv.MetricsMux()

	code, hdr, body := adminReq(t, mux, http.MethodGet, "/admin/backends", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	want := []string{b1.url, b2.url}
	sort.Strings(want)
	if got := adminBackendURLs(t, body); !equalStrs(got, want) {
		t.Fatalf("/admin/backends = %v, want %v", got, want)
	}
	// Both backends start healthy.
	for _, u := range want {
		if e := adminBackendEntry(t, body, u); !e.Healthy {
			t.Fatalf("backend %q reported unhealthy at start", u)
		}
	}
}

// TestE2E_Admin_Drain_StopsServing verifies POST /admin/drain?url=X marks X
// unhealthy so it is no longer served: after draining one of two backends, all
// traffic goes to the other, and /admin/backends reports the drained one unhealthy.
func TestE2E_Admin_Drain_StopsServing(t *testing.T) {
	b1 := newIDBackend("A", nil)
	b2 := newIDBackend("B", nil)
	defer b1.close()
	defer b2.close()

	cfg := baseConfig("round_robin", backendCfgs(b1, b2))
	srv := New(cfg, "")
	mux := srv.MetricsMux()
	h := srv.Handler()

	// Before draining, both serve.
	seen := map[string]int{}
	for i := 0; i < 20; i++ {
		id, code, _ := doReq(t, h, "")
		if code != http.StatusOK {
			t.Fatalf("pre-drain request %d: status %d", i, code)
		}
		seen[id]++
	}
	if seen["A"] == 0 || seen["B"] == 0 {
		t.Fatalf("pre-drain both backends should serve, got %v", seen)
	}

	// Drain A.
	code, _, _ := adminReq(t, mux, http.MethodPost, "/admin/drain?url="+b1.url, "")
	if code != http.StatusOK {
		t.Fatalf("drain status = %d, want 200", code)
	}

	// A is now unhealthy per the admin listing.
	_, _, body := adminReq(t, mux, http.MethodGet, "/admin/backends", "")
	if e := adminBackendEntry(t, body, b1.url); e.Healthy {
		t.Fatalf("drained backend %q still reported healthy", b1.url)
	}

	// All subsequent traffic goes to B only.
	after := map[string]int{}
	for i := 0; i < 30; i++ {
		id, code, _ := doReq(t, h, "")
		if code != http.StatusOK {
			t.Fatalf("post-drain request %d: status %d", i, code)
		}
		after[id]++
	}
	if after["A"] != 0 {
		t.Fatalf("drained backend A still served %d requests", after["A"])
	}
	if after["B"] != 30 {
		t.Fatalf("backend B served %d/30 requests after drain, want all 30", after["B"])
	}

	// Undrain restores A to rotation.
	code, _, _ = adminReq(t, mux, http.MethodPost, "/admin/undrain?url="+b1.url, "")
	if code != http.StatusOK {
		t.Fatalf("undrain status = %d, want 200", code)
	}
	_, _, body = adminReq(t, mux, http.MethodGet, "/admin/backends", "")
	if e := adminBackendEntry(t, body, b1.url); !e.Healthy {
		t.Fatalf("undrained backend %q still unhealthy", b1.url)
	}
}

// TestE2E_Admin_Weight_Changes verifies POST /admin/weight?url=X&weight=N updates
// the backend's weight, reflected in /admin/backends.
func TestE2E_Admin_Weight_Changes(t *testing.T) {
	b1 := newIDBackend("A", nil)
	defer b1.close()

	cfg := baseConfig("round_robin", backendCfgs(b1))
	srv := New(cfg, "")
	mux := srv.MetricsMux()

	_, _, body := adminReq(t, mux, http.MethodGet, "/admin/backends", "")
	if e := adminBackendEntry(t, body, b1.url); e.Weight != 1 {
		t.Fatalf("initial weight = %d, want 1", e.Weight)
	}

	code, _, _ := adminReq(t, mux, http.MethodPost, "/admin/weight?url="+b1.url+"&weight=7", "")
	if code != http.StatusOK {
		t.Fatalf("weight update status = %d, want 200", code)
	}

	_, _, body = adminReq(t, mux, http.MethodGet, "/admin/backends", "")
	if e := adminBackendEntry(t, body, b1.url); e.Weight != 7 {
		t.Fatalf("weight after update = %d, want 7", e.Weight)
	}

	// A bad weight is rejected without changing state.
	code, _, _ = adminReq(t, mux, http.MethodPost, "/admin/weight?url="+b1.url+"&weight=abc", "")
	if code != http.StatusBadRequest {
		t.Fatalf("invalid weight status = %d, want 400", code)
	}
	_, _, body = adminReq(t, mux, http.MethodGet, "/admin/backends", "")
	if e := adminBackendEntry(t, body, b1.url); e.Weight != 7 {
		t.Fatalf("weight after rejected update = %d, want still 7", e.Weight)
	}
}

// TestE2E_Admin_CircuitReset verifies POST /admin/circuit/reset?url=X resets a
// tripped circuit breaker back to closed, observable via /admin/backends'
// circuit_state field.
func TestE2E_Admin_CircuitReset(t *testing.T) {
	b1 := newIDBackend("A", nil)
	defer b1.close()

	cfg := baseConfig("round_robin", backendCfgs(b1))
	// Enable the circuit breaker so the server retains one and the admin reset acts
	// on real state. A low threshold makes tripping deterministic.
	cfg.CircuitBreaker = config.CircuitBreakerConfig{
		Enabled:          true,
		FailureThreshold: 2,
		SuccessThreshold: 1,
		Timeout:          time.Hour, // stay Open (do not auto half-open) during the test
	}
	srv := New(cfg, "")
	mux := srv.MetricsMux()

	be := findBackend(srv, b1.url)
	if be == nil {
		t.Fatalf("backend %q not found", b1.url)
	}
	if srv.circuitBreaker == nil {
		t.Fatalf("expected a circuit breaker to be configured")
	}

	// Trip the breaker: FailureThreshold consecutive failures -> Open.
	srv.circuitBreaker.RecordFailure(be)
	srv.circuitBreaker.RecordFailure(be)

	_, _, body := adminReq(t, mux, http.MethodGet, "/admin/backends", "")
	if e := adminBackendEntry(t, body, b1.url); e.CircuitState != "open" {
		t.Fatalf("circuit_state before reset = %q, want open", e.CircuitState)
	}

	// Reset via admin API.
	code, _, _ := adminReq(t, mux, http.MethodPost, "/admin/circuit/reset?url="+b1.url, "")
	if code != http.StatusOK {
		t.Fatalf("circuit reset status = %d, want 200", code)
	}

	_, _, body = adminReq(t, mux, http.MethodGet, "/admin/backends", "")
	if e := adminBackendEntry(t, body, b1.url); e.CircuitState != "closed" {
		t.Fatalf("circuit_state after reset = %q, want closed", e.CircuitState)
	}
}

// TestE2E_Admin_RequiresToken verifies that when an admin auth token is configured,
// every admin endpoint returns 401 without the bearer token and succeeds with it.
func TestE2E_Admin_RequiresToken(t *testing.T) {
	b1 := newIDBackend("A", nil)
	defer b1.close()

	const token = "s3cr3t-token"
	cfg := baseConfig("round_robin", backendCfgs(b1))
	cfg.Metrics.AuthToken = token
	// A circuit breaker so /admin/circuit/reset has real state to act on.
	cfg.CircuitBreaker = config.CircuitBreakerConfig{
		Enabled: true, FailureThreshold: 2, SuccessThreshold: 1, Timeout: time.Hour,
	}
	srv := New(cfg, "")
	mux := srv.MetricsMux()

	type call struct {
		method, target string
	}
	calls := []call{
		{http.MethodGet, "/admin/backends"},
		{http.MethodPost, "/admin/drain?url=" + b1.url},
		{http.MethodPost, "/admin/undrain?url=" + b1.url},
		{http.MethodPost, "/admin/weight?url=" + b1.url + "&weight=3"},
		{http.MethodPost, "/admin/circuit/reset?url=" + b1.url},
	}

	// Without the token: every endpoint is 401 and Set/Not-authorized.
	for _, c := range calls {
		code, hdr, _ := adminReq(t, mux, c.method, c.target, "")
		if code != http.StatusUnauthorized {
			t.Fatalf("%s %s without token: status %d, want 401", c.method, c.target, code)
		}
		if wa := hdr.Get("WWW-Authenticate"); wa == "" {
			t.Fatalf("%s %s: missing WWW-Authenticate challenge on 401", c.method, c.target)
		}
	}

	// Wrong token is also rejected.
	code, _, _ := adminReq(t, mux, http.MethodGet, "/admin/backends", "wrong-token")
	if code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status %d, want 401", code)
	}

	// With the correct token: every endpoint is authorized (not 401).
	for _, c := range calls {
		code, _, _ := adminReq(t, mux, c.method, c.target, token)
		if code == http.StatusUnauthorized {
			t.Fatalf("%s %s with valid token: got 401, want authorized", c.method, c.target)
		}
	}
}

// itoa is a tiny int64->string helper used by the TTL test's backend body so it
// avoids importing strconv purely for one call site.
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// waitUntil polls cond until it returns true or the timeout elapses, failing with
// msg on timeout. Used for time-dependent behavior (TTL expiry, async discovery).
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}
