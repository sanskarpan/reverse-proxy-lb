package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
)

func cacheMW() func(http.Handler) http.Handler {
	return Cache(config.CacheConfig{Enabled: true, DefaultTTL: time.Minute, MaxEntries: 100, MaxBodyBytes: 1 << 20, Methods: []string{"GET", "HEAD"}})
}

// Bug #1: a Cache-Control: no-cache response must NOT be served as a fresh stale HIT.
func TestCacheNoCacheResponseNotServedStale(t *testing.T) {
	var ver int32
	h := cacheMW()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprintf(w, "version-%d", atomic.AddInt32(&ver, 1))
	}))
	do := func() string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x/a", nil))
		return rec.Body.String()
	}
	if a, b := do(), do(); a == b {
		t.Errorf("no-cache response served stale: both %q (should re-fetch)", a)
	}
}

// Security (RFC 7234 §3.2): a shared cache must not serve one authenticated user's
// cached response to another user (unless the response is explicitly public).
func TestCacheAuthorizationNotSharedAcrossUsers(t *testing.T) {
	h := cacheMW()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No Vary / no cache-control: naive backend keyed on the auth token.
		fmt.Fprintf(w, "auth=%s", r.Header.Get("Authorization"))
	}))
	get := func(tok string) (string, string) {
		req := httptest.NewRequest(http.MethodGet, "http://x/acct", nil)
		req.Header.Set("Authorization", tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Body.String(), rec.Header().Get("X-Cache")
	}
	if b, _ := get("Bearer alice"); b != "auth=Bearer alice" {
		t.Fatalf("alice got %q", b)
	}
	if b, xc := get("Bearer bob"); b != "auth=Bearer bob" {
		t.Errorf("cross-user cache leak: bob got %q (X-Cache=%q)", b, xc)
	}
}

// A public response MAY be shared even for authorized requests.
func TestCachePublicSharedForAuthorized(t *testing.T) {
	var hits int32
	h := cacheMW()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=100")
		atomic.AddInt32(&hits, 1)
		fmt.Fprint(w, "shared")
	}))
	for _, tok := range []string{"Bearer a", "Bearer b"} {
		req := httptest.NewRequest(http.MethodGet, "http://x/pub", nil)
		req.Header.Set("Authorization", tok)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("public response should be shared across authorized users: backend hits=%d, want 1", got)
	}
}

// Bug #2 (observed): once the cache stores a Vary response, distinct Vary values get
// distinct entries (no mis-keyed variant), and a Vary-set change purges old variants.
func TestCacheVaryKeyedOnceObserved(t *testing.T) {
	h := cacheMW()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "X-User")
		fmt.Fprintf(w, "user=%s", r.Header.Get("X-User"))
	}))
	get := func(user string) string {
		req := httptest.NewRequest(http.MethodGet, "http://x/v", nil)
		req.Header.Set("X-User", user)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if got := get("alice"); got != "user=alice" {
		t.Fatalf("alice got %q", got)
	}
	if got := get("bob"); got != "user=bob" { // different variant, must not get alice's
		t.Errorf("Vary variant leak: bob got %q", got)
	}
	if got := get("alice"); got != "user=alice" { // alice's variant still correct
		t.Errorf("alice variant corrupted: got %q", got)
	}
}
