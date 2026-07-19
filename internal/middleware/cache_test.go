package middleware

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
)

// countingHandler returns a handler that increments hits on each call and writes
// the response produced by write.
func countingHandler(hits *int32, write func(w http.ResponseWriter, r *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		write(w, r)
	})
}

func doGET(t *testing.T, h http.Handler, url string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func enabledCfg() config.CacheConfig {
	return config.CacheConfig{
		Enabled:      true,
		DefaultTTL:   60 * time.Second,
		MaxEntries:   100,
		MaxBodyBytes: 1 << 20,
		Methods:      []string{http.MethodGet, http.MethodHead},
	}
}

func TestCacheDisabledIsPassthrough(t *testing.T) {
	var hits int32
	h := Cache(config.CacheConfig{Enabled: false})(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello")
	}))
	for i := 0; i < 3; i++ {
		doGET(t, h, "http://example.com/x", nil)
	}
	if hits != 3 {
		t.Fatalf("disabled cache should pass through every request; got %d backend hits, want 3", hits)
	}
}

func TestCacheServesSecondFromCache(t *testing.T) {
	var hits int32
	h := Cache(enabledCfg())(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "cached-body")
	}))

	r1 := doGET(t, h, "http://example.com/a", nil)
	if r1.Header().Get("X-Cache") != "" {
		t.Fatalf("first request should be a MISS (no X-Cache), got %q", r1.Header().Get("X-Cache"))
	}
	r2 := doGET(t, h, "http://example.com/a", nil)
	if got := r2.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("second request X-Cache = %q, want HIT", got)
	}
	if hits != 1 {
		t.Fatalf("backend hit %d times, want 1 (second served from cache)", hits)
	}
	if r2.Body.String() != "cached-body" {
		t.Fatalf("cached body = %q, want cached-body", r2.Body.String())
	}
	if r2.Header().Get("Age") == "" {
		t.Fatalf("cached response should carry an Age header")
	}
}

func TestCacheNoStoreNotCached(t *testing.T) {
	var hits int32
	h := Cache(enabledCfg())(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, "x")
	}))
	doGET(t, h, "http://example.com/ns", nil)
	doGET(t, h, "http://example.com/ns", nil)
	if hits != 2 {
		t.Fatalf("no-store response must not be cached; backend hits %d, want 2", hits)
	}
}

func TestCacheSetCookieNotCached(t *testing.T) {
	var hits int32
	h := Cache(enabledCfg())(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "sid=abc")
		io.WriteString(w, "x")
	}))
	doGET(t, h, "http://example.com/sc", nil)
	doGET(t, h, "http://example.com/sc", nil)
	if hits != 2 {
		t.Fatalf("Set-Cookie response must not be cached; backend hits %d, want 2", hits)
	}
}

func TestCachePrivateNotCached(t *testing.T) {
	var hits int32
	h := Cache(enabledCfg())(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private")
		io.WriteString(w, "x")
	}))
	doGET(t, h, "http://example.com/pv", nil)
	doGET(t, h, "http://example.com/pv", nil)
	if hits != 2 {
		t.Fatalf("private response must not be cached; backend hits %d, want 2", hits)
	}
}

func TestCacheNon200NotCached(t *testing.T) {
	var hits int32
	h := Cache(enabledCfg())(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "err")
	}))
	doGET(t, h, "http://example.com/e", nil)
	doGET(t, h, "http://example.com/e", nil)
	if hits != 2 {
		t.Fatalf("non-200 response must not be cached; backend hits %d, want 2", hits)
	}
}

func TestCacheRequestNoCacheBypasses(t *testing.T) {
	var hits int32
	h := Cache(enabledCfg())(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "x")
	}))
	// Prime the cache.
	doGET(t, h, "http://example.com/rc", nil)
	// A request with Cache-Control: no-cache must bypass the cache and re-fetch.
	rec := doGET(t, h, "http://example.com/rc", map[string]string{"Cache-Control": "no-cache"})
	if rec.Header().Get("X-Cache") == "HIT" {
		t.Fatalf("no-cache request must not be served from cache")
	}
	if hits != 2 {
		t.Fatalf("no-cache request should re-fetch; backend hits %d, want 2", hits)
	}
}

func TestCacheTTLExpiryRefetches(t *testing.T) {
	var hits int32
	cfg := enabledCfg()
	cfg.DefaultTTL = 30 * time.Millisecond
	h := Cache(cfg)(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "x")
	}))
	doGET(t, h, "http://example.com/ttl", nil) // miss, store
	r2 := doGET(t, h, "http://example.com/ttl", nil)
	if r2.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("second request within TTL should HIT")
	}
	time.Sleep(50 * time.Millisecond) // exceed TTL
	r3 := doGET(t, h, "http://example.com/ttl", nil)
	if r3.Header().Get("X-Cache") == "HIT" {
		t.Fatalf("request after TTL expiry must not be a HIT")
	}
	if hits != 2 {
		t.Fatalf("expired entry should re-fetch; backend hits %d, want 2", hits)
	}
}

func TestCacheMaxAgeSetsTTL(t *testing.T) {
	var hits int32
	h := Cache(enabledCfg())(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=1000")
		io.WriteString(w, "x")
	}))
	doGET(t, h, "http://example.com/ma", nil)
	r2 := doGET(t, h, "http://example.com/ma", nil)
	if r2.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("response with max-age should be cached and HIT on 2nd request")
	}
	if hits != 1 {
		t.Fatalf("backend hits %d, want 1", hits)
	}
}

func TestCacheMaxAgeZeroNotCached(t *testing.T) {
	var hits int32
	h := Cache(enabledCfg())(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=0")
		io.WriteString(w, "x")
	}))
	doGET(t, h, "http://example.com/m0", nil)
	doGET(t, h, "http://example.com/m0", nil)
	if hits != 2 {
		t.Fatalf("max-age=0 must not be cached; backend hits %d, want 2", hits)
	}
}

func TestCacheETagRevalidation304(t *testing.T) {
	var hits int32
	h := Cache(enabledCfg())(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		io.WriteString(w, "body")
	}))
	// Prime the cache.
	doGET(t, h, "http://example.com/et", nil)
	// Conditional request with the matching ETag must yield 304 from cache.
	rec := doGET(t, h, "http://example.com/et", map[string]string{"If-None-Match": `"v1"`})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match on cached ETag: status %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("304 must have empty body, got %q", rec.Body.String())
	}
	if hits != 1 {
		t.Fatalf("304 must be served from cache without a backend hit; hits %d, want 1", hits)
	}
	if rec.Header().Get("ETag") != `"v1"` {
		t.Fatalf("304 should echo the ETag")
	}
}

func TestCacheLRUEviction(t *testing.T) {
	var hits int32
	cfg := enabledCfg()
	cfg.MaxEntries = 2
	h := Cache(cfg)(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.URL.Path)
	}))

	// Fill entries for /1 and /2.
	doGET(t, h, "http://example.com/1", nil)
	doGET(t, h, "http://example.com/2", nil)
	// Touch /1 so it becomes most-recently-used.
	if r := doGET(t, h, "http://example.com/1", nil); r.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("/1 should still be cached before eviction")
	}
	// Insert /3 -> should evict the LRU entry (/2). Cache now holds {/3, /1}.
	doGET(t, h, "http://example.com/3", nil)

	// /1 (recently used) must still be a HIT after the eviction of /2.
	if r1 := doGET(t, h, "http://example.com/1", nil); r1.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("/1 (recently used) should survive eviction")
	}
	// /2 must have been evicted, so it re-fetches from the backend.
	before := atomic.LoadInt32(&hits)
	r2 := doGET(t, h, "http://example.com/2", nil)
	if r2.Header().Get("X-Cache") == "HIT" {
		t.Fatalf("/2 should have been evicted by LRU")
	}
	if atomic.LoadInt32(&hits) != before+1 {
		t.Fatalf("evicted /2 should trigger a backend re-fetch")
	}
}

func TestCacheVaryKeying(t *testing.T) {
	var hits int32
	h := Cache(enabledCfg())(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "Accept-Encoding")
		io.WriteString(w, r.Header.Get("Accept-Encoding"))
	}))

	// Two requests with different Accept-Encoding must be cached separately.
	doGET(t, h, "http://example.com/v", map[string]string{"Accept-Encoding": "gzip"})
	doGET(t, h, "http://example.com/v", map[string]string{"Accept-Encoding": "br"})
	if hits != 2 {
		t.Fatalf("distinct Vary values must key separately; backend hits %d, want 2", hits)
	}
	// Repeating the gzip variant must HIT.
	r := doGET(t, h, "http://example.com/v", map[string]string{"Accept-Encoding": "gzip"})
	if r.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("repeat of a stored Vary variant should HIT")
	}
	if r.Body.String() != "gzip" {
		t.Fatalf("Vary variant served wrong body: %q", r.Body.String())
	}
}

func TestCacheBodyTooLargeNotCached(t *testing.T) {
	var hits int32
	cfg := enabledCfg()
	cfg.MaxBodyBytes = 8
	h := Cache(cfg)(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "this body is definitely larger than eight bytes")
	}))
	r1 := doGET(t, h, "http://example.com/big", nil)
	if r1.Body.String() == "" {
		t.Fatalf("oversized response should still be forwarded to the client intact")
	}
	doGET(t, h, "http://example.com/big", nil)
	if hits != 2 {
		t.Fatalf("body over MaxBodyBytes must not be cached; backend hits %d, want 2", hits)
	}
}

func TestCacheStaleWhileRevalidate(t *testing.T) {
	var hits int32
	cfg := enabledCfg()
	cfg.DefaultTTL = 20 * time.Millisecond
	h := Cache(cfg)(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		// No max-age => DefaultTTL (20ms) applies; a stale window keeps the entry
		// serveable-stale after it expires.
		w.Header().Set("Cache-Control", "stale-while-revalidate=100")
		io.WriteString(w, "fresh")
	}))
	doGET(t, h, "http://example.com/swr", nil) // miss, store; TTL=20ms, SWR=100s
	// Wait past TTL but within SWR.
	time.Sleep(40 * time.Millisecond)
	r := doGET(t, h, "http://example.com/swr", nil)
	if got := r.Header().Get("X-Cache"); got != "STALE" {
		t.Fatalf("expired-but-within-SWR entry X-Cache = %q, want STALE", got)
	}
	// A background refresh should eventually re-hit the backend.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&hits) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&hits) < 2 {
		t.Fatalf("stale serve should trigger a background refresh (backend re-hit); hits %d", hits)
	}
}

// This test reuses flushRecorder (declared in observability_test.go), which
// tracks Flush delegation on an httptest.ResponseRecorder.
func TestCacheStreamingPassthrough(t *testing.T) {
	var hits int32
	h := Cache(enabledCfg())(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("cache capture writer must expose http.Flusher")
			return
		}
		for i := 0; i < 3; i++ {
			io.WriteString(w, "chunk")
			fl.Flush()
		}
	}))

	fr := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/stream", nil)
	h.ServeHTTP(fr, req)

	if !fr.flushed {
		t.Fatalf("Flush was not forwarded to the underlying writer")
	}

	// A flushed (streaming) response must NOT be cached: a second request re-hits
	// the backend.
	req2 := httptest.NewRequest(http.MethodGet, "http://example.com/stream", nil)
	fr2 := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(fr2, req2)
	if hits != 2 {
		t.Fatalf("streamed (flushed) response must not be cached; backend hits %d, want 2", hits)
	}
}

// This test reuses hijackableRecorder (declared in observability_test.go), which
// exposes Hijack and records that it was called.
func TestCacheHijackPassthrough(t *testing.T) {
	var hits int32
	h := Cache(enabledCfg())(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("cache capture writer must expose http.Hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack failed: %v", err)
			return
		}
		conn.Close()
	}))

	hr := &hijackableRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/ws", nil)
	h.ServeHTTP(hr, req)
	if !hr.hijacked {
		t.Fatalf("Hijack was not forwarded to the underlying writer")
	}
}

func TestCacheHeadNoBody(t *testing.T) {
	var hits int32
	h := Cache(enabledCfg())(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "getbody")
	}))
	// Prime with a GET.
	doGET(t, h, "http://example.com/hd", nil)
	// A HEAD to the same URL keys differently (method is part of the key); it will
	// miss and store, and its replay must have no body.
	req := httptest.NewRequest(http.MethodHead, "http://example.com/hd", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	req2 := httptest.NewRequest(http.MethodHead, "http://example.com/hd", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("second HEAD should be a cache HIT")
	}
	if rec2.Body.Len() != 0 {
		t.Fatalf("HEAD replay must not include a body, got %q", rec2.Body.String())
	}
}

// TestCacheConcurrentSafe exercises the cache under concurrent readers/writers to
// surface data races (run with -race).
func TestCacheConcurrentSafe(t *testing.T) {
	var hits int32
	h := Cache(enabledCfg())(countingHandler(&hits, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "x")
	}))
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(id int) {
			for j := 0; j < 50; j++ {
				url := fmt.Sprintf("http://example.com/c/%d", (id+j)%5)
				doGET(t, h, url, nil)
			}
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

// sanity: ensure parseCacheControl handles the directives we rely on.
func TestParseCacheControl(t *testing.T) {
	cc := parseCacheControl("max-age=30, stale-while-revalidate=10, private")
	if !cc.private {
		t.Fatalf("private not parsed")
	}
	if !cc.maxAgeSet || cc.maxAge != 30 {
		t.Fatalf("max-age = %d (set=%v), want 30", cc.maxAge, cc.maxAgeSet)
	}
	if cc.swr != 10 {
		t.Fatalf("swr = %d, want 10", cc.swr)
	}
	// s-maxage overrides max-age for a shared cache.
	cc2 := parseCacheControl("max-age=5, s-maxage=" + strconv.Itoa(99))
	if cc2.maxAge != 99 {
		t.Fatalf("s-maxage should override max-age; got %d, want 99", cc2.maxAge)
	}
}
