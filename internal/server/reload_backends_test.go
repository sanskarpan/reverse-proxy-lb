package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/circuit"
	"reverse-proxy-lb/internal/config"
)

// writeConfig writes yaml to a fresh temp file and returns its path. The caller
// registers cleanup via t.Cleanup.
func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "reload-backends-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(yaml); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

// backendURLs returns the set of URLs currently in the default balancer group.
func backendURLs(s *Server) map[string]int {
	m := make(map[string]int)
	for _, b := range s.GetBalancer().All() {
		m[b.URL] = b.GetWeight()
	}
	return m
}

// TestReloadBackendsLive builds a Server with two httptest backends, then reloads
// a config that adds a third backend, removes one of the originals, and changes a
// weight. It asserts the balancer reflects the new set (by URL) and weights, and
// that the removed backend's circuit state was Reset.
func TestReloadBackendsLive(t *testing.T) {
	b1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer b1.Close()
	b2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer b2.Close()
	b3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer b3.Close()

	initial := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 18090
backends:
  - url: %q
    weight: 1
  - url: %q
    weight: 1
load_balancer:
  algorithm: "round_robin"
  health_check:
    enabled: false
circuit_breaker:
  enabled: true
rate_limiter:
  enabled: false
metrics:
  enabled: false
compression:
  enabled: false
`, b1.URL, b2.URL)

	path := writeConfig(t, initial)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := New(cfg, path)

	if s.circuitBreaker == nil {
		t.Fatal("expected circuit breaker to be configured")
	}

	// Drive the removed backend (b2) into an OPEN circuit so a later Reset is
	// observable: find its *Backend and record failures against it.
	var removed *balancer.Backend
	for _, be := range s.GetBalancer().All() {
		if be.URL == b2.URL {
			removed = be
		}
	}
	if removed == nil {
		t.Fatalf("b2 not found in initial balancer set")
	}
	// Trip the breaker for the removed backend so its state is non-closed.
	for i := 0; i < 50; i++ {
		s.circuitBreaker.RecordFailure(removed)
	}
	if s.circuitBreaker.GetState(removed) == circuit.StateClosed {
		t.Fatalf("expected removed backend circuit to be non-closed before reload, got closed")
	}

	// New config: drop b2, keep b1 with a new weight, add b3.
	updated := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 18090
backends:
  - url: %q
    weight: 5
  - url: %q
    weight: 2
load_balancer:
  algorithm: "round_robin"
  health_check:
    enabled: false
circuit_breaker:
  enabled: true
rate_limiter:
  enabled: false
metrics:
  enabled: false
compression:
  enabled: false
`, b1.URL, b3.URL)

	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}

	s.reloadConfig()

	got := backendURLs(s)
	if len(got) != 2 {
		t.Fatalf("expected 2 backends after reload, got %d: %v", len(got), got)
	}
	if w, ok := got[b1.URL]; !ok || w != 5 {
		t.Errorf("b1: expected weight 5, got %d (present=%v)", w, ok)
	}
	if w, ok := got[b3.URL]; !ok || w != 2 {
		t.Errorf("b3: expected weight 2, got %d (present=%v)", w, ok)
	}
	if _, ok := got[b2.URL]; ok {
		t.Errorf("b2 should have been removed but is still present")
	}

	// The removed backend's circuit state must have been Reset to closed.
	if st := s.circuitBreaker.GetState(removed); st != circuit.StateClosed {
		t.Errorf("expected removed backend circuit Reset to closed, got %v", st)
	}
}

// TestReloadBackendsConcurrent hammers balancer.Next() from goroutines while
// reloadConfig() runs repeatedly. Run under -race: it must produce no data race
// and no panic.
func TestReloadBackendsConcurrent(t *testing.T) {
	b1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer b1.Close()
	b2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer b2.Close()
	b3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer b3.Close()

	cfgA := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 18091
backends:
  - url: %q
    weight: 1
  - url: %q
    weight: 1
load_balancer:
  algorithm: "round_robin"
  health_check:
    enabled: false
circuit_breaker:
  enabled: true
rate_limiter:
  enabled: false
metrics:
  enabled: false
compression:
  enabled: false
`, b1.URL, b2.URL)

	cfgB := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 18091
backends:
  - url: %q
    weight: 3
  - url: %q
    weight: 1
load_balancer:
  algorithm: "round_robin"
  health_check:
    enabled: false
circuit_breaker:
  enabled: true
rate_limiter:
  enabled: false
metrics:
  enabled: false
compression:
  enabled: false
`, b1.URL, b3.URL)

	path := writeConfig(t, cfgA)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := New(cfg, path)

	var stop atomic.Bool
	var wg sync.WaitGroup

	// Selector goroutines: continuously pick backends.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				_, _ = s.GetBalancer().Next()
			}
		}()
	}

	// Reloader goroutines: flip between the two configs and reload.
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 40; n++ {
				body := cfgA
				if n%2 == 0 {
					body = cfgB
				}
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					return
				}
				s.reloadConfig()
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	// After the churn settle to a known config so the final assertion is stable.
	if err := os.WriteFile(path, []byte(cfgA), 0o600); err != nil {
		t.Fatal(err)
	}
	s.reloadConfig()
	if len(s.GetBalancer().All()) == 0 {
		t.Fatal("expected a non-empty backend set after final reload")
	}
}

// TestConfigWatchReload exercises the file-watch loop: with WatchConfig enabled a
// change to the config file on disk triggers a live backend reload without any
// explicit reloadConfig call.
func TestConfigWatchReload(t *testing.T) {
	b1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer b1.Close()
	b2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer b2.Close()

	initial := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 0
  watch_config: true
  watch_interval: 20ms
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
`, b1.URL)

	path := writeConfig(t, initial)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Server.WatchConfig {
		t.Fatal("expected watch_config to be enabled")
	}
	s := New(cfg, path)

	// Start only the watch loop (not the whole listener) so the test stays fast
	// and port-free.
	s.startConfigWatch()
	defer s.stopConfigWatch()

	updated := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 0
  watch_config: true
  watch_interval: 20ms
backends:
  - url: %q
    weight: 1
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
`, b1.URL, b2.URL)

	// Ensure the mtime advances even on coarse-grained filesystems.
	time.Sleep(30 * time.Millisecond)
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		if len(s.GetBalancer().All()) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("watch did not pick up backend add; have %d backends", len(s.GetBalancer().All()))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
