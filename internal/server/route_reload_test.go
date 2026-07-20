package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
)

// TestRouteReload_SwapPaths verifies that after reloadConfig the router
// atomically swaps to the new route table: /a goes to backend B and /b goes to
// backend A after the reload (they were swapped in the new config).
func TestRouteReload_SwapPaths(t *testing.T) {
	backA := newIDBackend("A", nil)
	backB := newIDBackend("B", nil)
	defer backA.close()
	defer backB.close()

	initial := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 18200
backends:
  - url: %q
    weight: 1
routes:
  - name: "route-a"
    path_prefix: "/a"
    algorithm: "round_robin"
    backends:
      - url: %q
        weight: 1
  - name: "route-b"
    path_prefix: "/b"
    algorithm: "round_robin"
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
  enabled: false
metrics:
  enabled: false
compression:
  enabled: false
`, backA.url, backA.url, backB.url)

	path := writeConfig(t, initial)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load initial config: %v", err)
	}
	s := New(cfg, path)

	if s.router == nil {
		t.Fatal("expected router to be installed after initial config with routes")
	}

	h := s.Handler()

	// Before reload: /a -> A, /b -> B.
	if id, code := routedReq(t, h, http.MethodGet, "proxy.test", "/a/foo"); code != http.StatusOK || id != "A" {
		t.Fatalf("before reload: /a got id=%q code=%d, want A/200", id, code)
	}
	if id, code := routedReq(t, h, http.MethodGet, "proxy.test", "/b/foo"); code != http.StatusOK || id != "B" {
		t.Fatalf("before reload: /b got id=%q code=%d, want B/200", id, code)
	}

	// Write new config with routes swapped: /a -> B, /b -> A.
	swapped := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 18200
backends:
  - url: %q
    weight: 1
routes:
  - name: "route-a"
    path_prefix: "/a"
    algorithm: "round_robin"
    backends:
      - url: %q
        weight: 1
  - name: "route-b"
    path_prefix: "/b"
    algorithm: "round_robin"
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
  enabled: false
metrics:
  enabled: false
compression:
  enabled: false
`, backA.url, backB.url, backA.url)

	if err := os.WriteFile(path, []byte(swapped), 0o600); err != nil {
		t.Fatal(err)
	}

	s.reloadConfig()

	// After reload: /a -> B, /b -> A (swapped).
	if id, code := routedReq(t, h, http.MethodGet, "proxy.test", "/a/foo"); code != http.StatusOK || id != "B" {
		t.Fatalf("after reload: /a got id=%q code=%d, want B/200 (routes were swapped)", id, code)
	}
	if id, code := routedReq(t, h, http.MethodGet, "proxy.test", "/b/foo"); code != http.StatusOK || id != "A" {
		t.Fatalf("after reload: /b got id=%q code=%d, want A/200 (routes were swapped)", id, code)
	}
}

// TestRouteReload_NoDroppedRequests verifies that in-flight requests complete
// correctly while a concurrent reloadConfig is running. It fires a stream of
// requests to /a and /b concurrently with reloads that swap the routes,
// and asserts that every response is 200 (no request is dropped/500).
func TestRouteReload_NoDroppedRequests(t *testing.T) {
	backA := newIDBackend("A", nil)
	backB := newIDBackend("B", nil)
	defer backA.close()
	defer backB.close()

	initial := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 18201
backends:
  - url: %q
    weight: 1
routes:
  - name: "route-a"
    path_prefix: "/a"
    algorithm: "round_robin"
    backends:
      - url: %q
        weight: 1
  - name: "route-b"
    path_prefix: "/b"
    algorithm: "round_robin"
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
  enabled: false
metrics:
  enabled: false
compression:
  enabled: false
`, backA.url, backA.url, backB.url)

	swapped := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 18201
backends:
  - url: %q
    weight: 1
routes:
  - name: "route-a"
    path_prefix: "/a"
    algorithm: "round_robin"
    backends:
      - url: %q
        weight: 1
  - name: "route-b"
    path_prefix: "/b"
    algorithm: "round_robin"
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
  enabled: false
metrics:
  enabled: false
compression:
  enabled: false
`, backA.url, backB.url, backA.url)

	path := writeConfig(t, initial)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := New(cfg, path)
	h := s.Handler()

	var (
		errors  atomic.Int64
		stop    atomic.Bool
		wg      sync.WaitGroup
		current atomic.Int32 // 0 = initial, 1 = swapped
	)

	// Request goroutines: fire requests and count non-200s.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		path2 := "/a/test"
		if i%2 == 1 {
			path2 = "/b/test"
		}
		go func(p string) {
			defer wg.Done()
			for !stop.Load() {
				req := httptest.NewRequest(http.MethodGet, "http://proxy.test"+p, nil)
				req.Host = "proxy.test"
				req.RemoteAddr = "127.0.0.1:12345"
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					errors.Add(1)
				}
				_, _ = io.ReadAll(rec.Body)
			}
		}(path2)
	}

	// Reload goroutine: alternately swap between the two configs.
	configs := []string{initial, swapped}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			body := configs[int(current.Load())%2]
			if err2 := os.WriteFile(path, []byte(body), 0o600); err2 == nil {
				s.reloadConfig()
			}
			current.Add(1)
			time.Sleep(5 * time.Millisecond)
		}
	}()

	time.Sleep(150 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	if n := errors.Load(); n > 0 {
		t.Errorf("got %d non-200 responses during concurrent reload; want 0 dropped requests", n)
	}
}

// TestRouteReload_AddRoute verifies that a reload that adds a new route (when
// no routes existed before) emits a warning (no router was built at startup),
// while a reload that modifies an existing router's routes applies live.
func TestRouteReload_AddRouteToExistingRouter(t *testing.T) {
	backA := newIDBackend("A", nil)
	backB := newIDBackend("B", nil)
	backC := newIDBackend("C", nil)
	defer backA.close()
	defer backB.close()
	defer backC.close()

	initial := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 18202
backends:
  - url: %q
    weight: 1
routes:
  - name: "route-a"
    path_prefix: "/a"
    algorithm: "round_robin"
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
  enabled: false
metrics:
  enabled: false
compression:
  enabled: false
`, backA.url, backA.url)

	path := writeConfig(t, initial)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := New(cfg, path)
	h := s.Handler()

	// Initially /a -> A, /c -> fallthrough to default (backA).
	if id, code := routedReq(t, h, http.MethodGet, "proxy.test", "/a/x"); code != http.StatusOK || id != "A" {
		t.Fatalf("before reload: /a got %q/%d, want A/200", id, code)
	}

	// Add a route for /c -> C.
	updated := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 18202
backends:
  - url: %q
    weight: 1
routes:
  - name: "route-a"
    path_prefix: "/a"
    algorithm: "round_robin"
    backends:
      - url: %q
        weight: 1
  - name: "route-c"
    path_prefix: "/c"
    algorithm: "round_robin"
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
  enabled: false
metrics:
  enabled: false
compression:
  enabled: false
`, backA.url, backA.url, backC.url)

	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	s.reloadConfig()

	// After reload: /a -> A (unchanged), /c -> C (new route).
	if id, code := routedReq(t, h, http.MethodGet, "proxy.test", "/a/x"); code != http.StatusOK || id != "A" {
		t.Fatalf("after reload: /a got %q/%d, want A/200", id, code)
	}
	if id, code := routedReq(t, h, http.MethodGet, "proxy.test", "/c/x"); code != http.StatusOK || id != "C" {
		t.Fatalf("after reload: /c got %q/%d, want C/200 (new route applied live)", id, code)
	}
}

// TestRouteReload_Race hammers router.Route concurrently with reloadConfig to
// confirm -race detects no data races on the Router's internal slice.
func TestRouteReload_Race(t *testing.T) {
	backA := newIDBackend("A", nil)
	backB := newIDBackend("B", nil)
	defer backA.close()
	defer backB.close()

	cfgA := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 18203
backends:
  - url: %q
    weight: 1
routes:
  - name: "route-a"
    path_prefix: "/a"
    algorithm: "round_robin"
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
  enabled: false
metrics:
  enabled: false
compression:
  enabled: false
`, backA.url, backA.url)

	cfgB := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 18203
backends:
  - url: %q
    weight: 1
routes:
  - name: "route-b"
    path_prefix: "/b"
    algorithm: "round_robin"
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
  enabled: false
metrics:
  enabled: false
compression:
  enabled: false
`, backA.url, backB.url)

	path := writeConfig(t, cfgA)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := New(cfg, path)
	h := s.Handler()

	var stop atomic.Bool
	var wg sync.WaitGroup

	// Sender goroutines: issue requests concurrently.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				req := httptest.NewRequest(http.MethodGet, "http://proxy.test/a/x", nil)
				req.Host = "proxy.test"
				req.RemoteAddr = "127.0.0.1:12345"
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				_, _ = io.ReadAll(rec.Body)
			}
		}()
	}

	// Reloader goroutine: flip between cfgA and cfgB.
	cfgs := []string{cfgA, cfgB}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			body := cfgs[i%2]
			if err2 := os.WriteFile(path, []byte(body), 0o600); err2 == nil {
				s.reloadConfig()
			}
		}
	}()

	time.Sleep(80 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
}
