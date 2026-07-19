package middleware

import (
	"compress/gzip"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
)

// sameHandler reports whether two http.Handlers are the same underlying value.
// Handlers backed by http.HandlerFunc are function values, which are not
// comparable with ==, so identity is checked via their code pointers.
func sameHandler(a, b http.Handler) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// withDeterministicRand installs a fixed-seed rng for the duration of the test so
// sampling decisions are reproducible, restoring the previous source afterward.
func withDeterministicRand(t *testing.T, seed int64) {
	t.Helper()
	prev := SetTransformRand(rand.New(rand.NewSource(seed)))
	t.Cleanup(func() { SetTransformRand(prev) })
}

func TestRewriteSetsAndRemovesRequestHeadersAndStripsPath(t *testing.T) {
	var gotPath string
	var gotHeader string
	var removedPresent bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeader = r.Header.Get("X-Add")
		_, removedPresent = r.Header["X-Kill"]
		w.WriteHeader(http.StatusOK)
	})

	cfg := config.RewriteConfig{
		RequestHeadersSet:    map[string]string{"X-Add": "yes"},
		RequestHeadersRemove: []string{"X-Kill"},
		StripPathPrefix:      "/api",
	}
	h := Rewrite(cfg)(next)

	req := httptest.NewRequest(http.MethodGet, "http://x/api/users/7", nil)
	req.Header.Set("X-Kill", "please")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if gotPath != "/users/7" {
		t.Errorf("path prefix not stripped: got %q want %q", gotPath, "/users/7")
	}
	if gotHeader != "yes" {
		t.Errorf("request header not set: got %q", gotHeader)
	}
	if removedPresent {
		t.Errorf("request header X-Kill should have been removed")
	}
}

func TestRewriteStripPathKeepsLeadingSlashWhenPrefixIsWholePath(t *testing.T) {
	var gotPath string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	})
	h := Rewrite(config.RewriteConfig{StripPathPrefix: "/api"})(next)
	req := httptest.NewRequest(http.MethodGet, "http://x/api", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if gotPath != "/" {
		t.Errorf("expected %q, got %q", "/", gotPath)
	}
}

func TestRewriteSetsAndRemovesResponseHeaders(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Remove-Me", "old")
		w.Header().Set("Keep", "kept")
		w.WriteHeader(http.StatusTeapot)
		io.WriteString(w, "body")
	})
	cfg := config.RewriteConfig{
		ResponseHeadersSet:    map[string]string{"X-Server": "edge"},
		ResponseHeadersRemove: []string{"X-Remove-Me"},
	}
	h := Rewrite(cfg)(next)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x/", nil))

	if rec.Code != http.StatusTeapot {
		t.Errorf("status not forwarded: got %d", rec.Code)
	}
	if rec.Header().Get("X-Server") != "edge" {
		t.Errorf("response header not set: got %q", rec.Header().Get("X-Server"))
	}
	if rec.Header().Get("X-Remove-Me") != "" {
		t.Errorf("response header should be removed, got %q", rec.Header().Get("X-Remove-Me"))
	}
	if rec.Header().Get("Keep") != "kept" {
		t.Errorf("unrelated header should be preserved")
	}
	if rec.Body.String() != "body" {
		t.Errorf("body altered: got %q", rec.Body.String())
	}
}

func TestRewriteHTTPSRedirect(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	h := Rewrite(config.RewriteConfig{HTTPSRedirect: true})(next)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://example.com/path?q=1", nil)
	h.ServeHTTP(rec, req)

	if called {
		t.Fatalf("next handler must not run on redirect")
	}
	if rec.Code != http.StatusPermanentRedirect {
		t.Errorf("expected 308, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://example.com/path?q=1" {
		t.Errorf("bad redirect Location: %q", loc)
	}
}

func TestRewriteHTTPSRedirectSkippedWhenForwardedHTTPS(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := Rewrite(config.RewriteConfig{HTTPSRedirect: true})(next)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	h.ServeHTTP(rec, req)

	if !called {
		t.Fatalf("next handler should run when already https via X-Forwarded-Proto")
	}
	if rec.Code == http.StatusPermanentRedirect {
		t.Errorf("should not redirect when X-Forwarded-Proto=https")
	}
}

func TestRewriteDisabledIsPassthrough(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if got := Rewrite(config.RewriteConfig{})(next); !sameHandler(got, next) {
		t.Errorf("empty Rewrite config should return the next handler unchanged")
	}
}

func TestFaultInjectionDisabledIsPassthrough(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if got := FaultInjection(config.FaultConfig{})(next); !sameHandler(got, next) {
		t.Errorf("disabled FaultInjection should return next unchanged")
	}
}

func TestFaultInjectionAbortsConfiguredFraction(t *testing.T) {
	withDeterministicRand(t, 1)

	var served int
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		w.WriteHeader(http.StatusOK)
	})
	cfg := config.FaultConfig{Enabled: true, AbortPercent: 50, AbortStatus: 503}
	h := FaultInjection(cfg)(next)

	const n = 1000
	aborts := 0
	for i := 0; i < n; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x/", nil))
		if rec.Code == 503 {
			aborts++
		}
	}
	if served+aborts != n {
		t.Fatalf("every request should be served or aborted: served=%d aborts=%d", served, aborts)
	}
	// ~50% of 1000; allow generous slack for the rng.
	if aborts < 400 || aborts > 600 {
		t.Errorf("abort fraction out of expected range: got %d/1000", aborts)
	}
}

func TestFaultInjectionAbortUsesDefaultStatusWhenUnset(t *testing.T) {
	withDeterministicRand(t, 2)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("next must not run when every request aborts")
	})
	cfg := config.FaultConfig{Enabled: true, AbortPercent: 100} // AbortStatus 0 -> 503
	h := FaultInjection(cfg)(next)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected default 503, got %d", rec.Code)
	}
}

func TestFaultInjectionDelaysConfiguredFraction(t *testing.T) {
	withDeterministicRand(t, 3)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	delay := 20 * time.Millisecond
	cfg := config.FaultConfig{Enabled: true, DelayPercent: 100, Delay: delay}
	h := FaultInjection(cfg)(next)

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x/", nil))
	if elapsed := time.Since(start); elapsed < delay {
		t.Errorf("expected at least %v delay, got %v", delay, elapsed)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("delayed request should still succeed, got %d", rec.Code)
	}

	// With DelayPercent 0 there should be no meaningful delay.
	cfg.DelayPercent = 0
	h2 := FaultInjection(cfg)(next)
	start = time.Now()
	h2.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://x/", nil))
	if elapsed := time.Since(start); elapsed >= delay {
		t.Errorf("no delay expected with DelayPercent=0, got %v", elapsed)
	}
}

func TestMirrorDisabledIsPassthrough(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if got := Mirror(config.MirrorConfig{})(next); !sameHandler(got, next) {
		t.Errorf("disabled Mirror should return next unchanged")
	}
}

func TestMirrorSendsShadowWithoutAlteringPrimary(t *testing.T) {
	withDeterministicRand(t, 4)

	var (
		mirrorHits   int32
		gotBody      string
		gotHeader    string
		mirrorDone   = make(chan struct{})
		mirrorOnce   sync.Once
		mirrorMethod string
	)
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&mirrorHits, 1)
		mirrorMethod = r.Method
		gotHeader = r.Header.Get("X-Trace")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		mirrorOnce.Do(func() { close(mirrorDone) })
	}))
	defer mirror.Close()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Primary must see the same body.
		b, _ := io.ReadAll(r.Body)
		if string(b) != "payload" {
			t.Errorf("primary body corrupted: got %q", string(b))
		}
		w.Header().Set("X-Primary", "1")
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, "primary-response")
	})

	cfg := config.MirrorConfig{Enabled: true, URL: mirror.URL, SamplePercent: 100, Timeout: 2 * time.Second}
	h := Mirror(cfg)(next)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/thing", strings.NewReader("payload"))
	req.Header.Set("X-Trace", "abc")
	h.ServeHTTP(rec, req)

	// Primary response is intact and unaffected.
	if rec.Code != http.StatusAccepted {
		t.Errorf("primary status altered: got %d", rec.Code)
	}
	if rec.Body.String() != "primary-response" {
		t.Errorf("primary body altered: got %q", rec.Body.String())
	}
	if rec.Header().Get("X-Primary") != "1" {
		t.Errorf("primary header lost")
	}

	select {
	case <-mirrorDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("mirror backend never received the shadow request")
	}
	if atomic.LoadInt32(&mirrorHits) != 1 {
		t.Errorf("expected exactly one mirror hit, got %d", mirrorHits)
	}
	if gotBody != "payload" {
		t.Errorf("mirror body wrong: got %q", gotBody)
	}
	if gotHeader != "abc" {
		t.Errorf("mirror did not carry original headers: X-Trace=%q", gotHeader)
	}
	if mirrorMethod != http.MethodPost {
		t.Errorf("mirror method wrong: got %q", mirrorMethod)
	}
}

func TestMirrorFailureDoesNotBreakPrimary(t *testing.T) {
	withDeterministicRand(t, 5)

	served := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})
	// Unreachable/invalid mirror URL: client.Do will error, must be swallowed.
	cfg := config.MirrorConfig{Enabled: true, URL: "http://127.0.0.1:1/never", SamplePercent: 100, Timeout: 200 * time.Millisecond}
	h := Mirror(cfg)(next)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://x/", strings.NewReader("data"))
	h.ServeHTTP(rec, req)

	if !served {
		t.Fatalf("primary handler must run even when the mirror fails")
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("primary response affected by mirror failure: code=%d body=%q", rec.Code, rec.Body.String())
	}
	// Give the background goroutine a moment; it must not panic/affect anything.
	time.Sleep(50 * time.Millisecond)
}

func TestMirrorSamplePercentZeroNeverMirrors(t *testing.T) {
	withDeterministicRand(t, 6)
	var hits int32
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer mirror.Close()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	cfg := config.MirrorConfig{Enabled: true, URL: mirror.URL, SamplePercent: 0}
	h := Mirror(cfg)(next)
	for i := 0; i < 20; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://x/", nil))
	}
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("SamplePercent=0 should never mirror, got %d hits", hits)
	}
}

// --- Compression polish tests (gzip.go) ---

func TestGzipSkipsNonAllowlistedContentType(t *testing.T) {
	cfg := config.CompressionConfig{ContentTypes: []string{"text/", "application/json"}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		io.WriteString(w, strings.Repeat("x", 500))
	})
	h := GzipWithConfig(cfg, next)

	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") == "gzip" {
		t.Errorf("non-allowlisted content-type must not be gzipped")
	}
	if rec.Body.Len() != 500 {
		t.Errorf("body should be passed through uncompressed, got %d bytes", rec.Body.Len())
	}
}

func TestGzipCompressesAllowlistedContentType(t *testing.T) {
	cfg := config.CompressionConfig{ContentTypes: []string{"text/"}}
	body := strings.Repeat("hello ", 200)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, body)
	})
	h := GzipWithConfig(cfg, next)

	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("allowlisted text/plain should be gzipped")
	}
	gz, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("invalid gzip: %v", err)
	}
	got, _ := io.ReadAll(gz)
	if string(got) != body {
		t.Errorf("decompressed mismatch")
	}
}

func TestGzipSkipsTinyBodyBelowMinSize(t *testing.T) {
	cfg := config.CompressionConfig{MinSize: 1024}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "tiny") // 4 bytes, well below 1024
	})
	h := GzipWithConfig(cfg, next)

	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") == "gzip" {
		t.Errorf("body below MinSize must not be gzipped")
	}
	if rec.Body.String() != "tiny" {
		t.Errorf("small body should pass through unchanged, got %q", rec.Body.String())
	}
}

func TestGzipCompressesBodyAtOrAboveMinSize(t *testing.T) {
	cfg := config.CompressionConfig{MinSize: 64}
	body := strings.Repeat("a", 512)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, body)
	})
	h := GzipWithConfig(cfg, next)

	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("body above MinSize should be gzipped")
	}
	gz, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("invalid gzip: %v", err)
	}
	got, _ := io.ReadAll(gz)
	if string(got) != body {
		t.Errorf("decompressed mismatch")
	}
}

func TestGzipDefaultConfigMatchesLegacyBehavior(t *testing.T) {
	body := strings.Repeat("z", 300)
	h := GzipWithConfig(config.CompressionConfig{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("zero-config gzip should compress all eligible responses (legacy behavior)")
	}
}
