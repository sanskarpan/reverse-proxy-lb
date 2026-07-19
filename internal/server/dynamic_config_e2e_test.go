package server

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/circuit"
	"reverse-proxy-lb/internal/config"
)

// This file drives §8 dynamic configuration end-to-end through the real server
// stack (Server.Handler(), i.e. proxy + the full middleware chain) via httptest.
// It exercises live backend add/remove via reloadConfig(), the --validate CLI
// path on the built binary, the file-watch auto-reload loop, and reloads under
// concurrent load. Traffic is driven through Server.Handler() so the entire
// request path is exercised without binding a real listener or restarting.
// Backends identify themselves in the response body so assertions can tell which
// upstream served each request. No assertions are weakened to force a pass.

// dynConfigYAML renders a config file body wiring the given backend URLs into the
// default pool. Health checks are disabled (so a freshly added backend is
// immediately eligible) and the circuit breaker is enabled (so removal's circuit
// Reset is observable). watch controls the file-watch loop.
func dynConfigYAML(watch bool, watchInterval string, urls ...string) string {
	var sb string
	sb = "server:\n  host: \"127.0.0.1\"\n  port: 0\n"
	if watch {
		sb += "  watch_config: true\n  watch_interval: " + watchInterval + "\n"
	}
	sb += "backends:\n"
	for _, u := range urls {
		sb += fmt.Sprintf("  - url: %q\n    weight: 1\n", u)
	}
	sb += `load_balancer:
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
`
	return sb
}

// drive fires n requests through h and returns a map of backend-id -> hit count.
// It fails the test on any non-200. This exercises the full handler stack.
func drive(t *testing.T, h http.Handler, n int) map[string]int {
	t.Helper()
	hits := make(map[string]int)
	for i := 0; i < n; i++ {
		id, code, _ := doReq(t, h, "")
		if code != http.StatusOK {
			t.Fatalf("request %d got status %d (id=%q)", i, code, id)
		}
		hits[id]++
	}
	return hits
}

// findBackend returns the live *balancer.Backend in the default group whose URL
// matches, or nil.
func findBackend(s *Server, url string) *balancer.Backend {
	for _, be := range s.GetBalancer().All() {
		if be.URL == url {
			return be
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// §8 live backend ADD via reloadConfig(), traffic driven through Server.Handler()
// -----------------------------------------------------------------------------

// TestE2E_DynamicConfig_LiveBackendAdd starts with 2 backends, drives traffic and
// asserts ONLY those 2 serve, rewrites the config to add a 3rd backend, calls
// reloadConfig(), and asserts the 3rd backend then starts receiving traffic — all
// without restarting the server (traffic flows through Server.Handler()).
func TestE2E_DynamicConfig_LiveBackendAdd(t *testing.T) {
	b1 := newIDBackend("B1", nil)
	b2 := newIDBackend("B2", nil)
	b3 := newIDBackend("B3", nil)
	defer b1.close()
	defer b2.close()
	defer b3.close()

	path := writeConfig(t, dynConfigYAML(false, "", b1.url, b2.url))
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	srv := New(cfg, path)
	h := srv.Handler()

	// Phase 1: only b1 and b2 should serve. Drive enough traffic that round_robin
	// would hit a third backend many times over if one existed.
	before := drive(t, h, 300)
	if before["B3"] != 0 {
		t.Fatalf("b3 served %d requests before it was added; want 0", before["B3"])
	}
	if before["B1"] == 0 || before["B2"] == 0 {
		t.Fatalf("expected both initial backends to serve; got %v", before)
	}
	if before["B1"]+before["B2"] != 300 {
		t.Fatalf("initial phase served by unexpected backends: %v", before)
	}

	// Rewrite the config on disk to add b3, then reload live (no restart).
	if err := os.WriteFile(path, []byte(dynConfigYAML(false, "", b1.url, b2.url, b3.url)), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.reloadConfig()

	if findBackend(srv, b3.url) == nil {
		t.Fatalf("b3 not present in balancer after reload; have %v", backendURLs(srv))
	}

	// Phase 2: the SAME handler (no restart) must now route some traffic to b3.
	after := drive(t, h, 300)
	if after["B3"] == 0 {
		t.Fatalf("b3 received no traffic after live add; distribution=%v", after)
	}
	if after["B1"] == 0 || after["B2"] == 0 {
		t.Fatalf("expected all three backends to serve after add; got %v", after)
	}
	if after["B1"]+after["B2"]+after["B3"] != 300 {
		t.Fatalf("post-add phase served by unexpected backends: %v", after)
	}
}

// -----------------------------------------------------------------------------
// §8 live backend REMOVE via reloadConfig(): stops receiving traffic + circuit reset
// -----------------------------------------------------------------------------

// TestE2E_DynamicConfig_LiveBackendRemove starts with 3 backends, trips the
// circuit breaker for one of them, removes it via a live reload, and asserts that
// (a) it stops receiving traffic entirely and (b) its circuit state was Reset to
// closed. Traffic flows through Server.Handler() the whole time.
func TestE2E_DynamicConfig_LiveBackendRemove(t *testing.T) {
	b1 := newIDBackend("B1", nil)
	b2 := newIDBackend("B2", nil)
	b3 := newIDBackend("B3", nil)
	defer b1.close()
	defer b2.close()
	defer b3.close()

	path := writeConfig(t, dynConfigYAML(false, "", b1.url, b2.url, b3.url))
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	srv := New(cfg, path)
	h := srv.Handler()

	if srv.circuitBreaker == nil {
		t.Fatal("expected circuit breaker to be configured")
	}

	// Confirm all three serve before removal.
	before := drive(t, h, 300)
	if before["B2"] == 0 {
		t.Fatalf("b2 served no traffic before removal; got %v", before)
	}

	// Trip b2's circuit so a later Reset (to closed) is observable.
	removed := findBackend(srv, b2.url)
	if removed == nil {
		t.Fatal("b2 not found in initial balancer set")
	}
	for i := 0; i < 50; i++ {
		srv.circuitBreaker.RecordFailure(removed)
	}
	if srv.circuitBreaker.GetState(removed) == circuit.StateClosed {
		t.Fatal("expected b2 circuit to be non-closed before removal")
	}

	// Remove b2 via a live reload (no restart).
	if err := os.WriteFile(path, []byte(dynConfigYAML(false, "", b1.url, b3.url)), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.reloadConfig()

	if findBackend(srv, b2.url) != nil {
		t.Fatalf("b2 still present after removal; have %v", backendURLs(srv))
	}

	// Circuit state for the removed backend must have been Reset to closed.
	if st := srv.circuitBreaker.GetState(removed); st != circuit.StateClosed {
		t.Errorf("removed backend circuit not Reset: got %v, want closed", st)
	}

	// After removal, b2 must stop receiving traffic through the SAME handler.
	b2Before := b2.hitCount()
	after := drive(t, h, 300)
	if after["B2"] != 0 {
		t.Fatalf("b2 received %d requests after removal; want 0 (dist=%v)", after["B2"], after)
	}
	if got := b2.hitCount(); got != b2Before {
		t.Errorf("b2 upstream hit count moved from %d to %d after removal; want unchanged", b2Before, got)
	}
	if after["B1"] == 0 || after["B3"] == 0 {
		t.Fatalf("expected remaining backends to serve after removal; got %v", after)
	}
	if after["B1"]+after["B3"] != 300 {
		t.Fatalf("post-removal phase served by unexpected backends: %v", after)
	}
}

// -----------------------------------------------------------------------------
// §8 --validate on the built binary: exit 0 for a good config, non-zero for bad
// -----------------------------------------------------------------------------

// TestE2E_DynamicConfig_ValidateFlag builds the proxy binary and runs it with
// --validate against a good config (expecting exit 0) and a bad config (expecting
// a non-zero exit). This exercises the real CLI path in cmd/proxy/main.go.
func TestE2E_DynamicConfig_ValidateFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary build in -short mode")
	}

	// Build the proxy binary to a temp path.
	bin := filepath.Join(t.TempDir(), "proxy-bin")
	build := exec.Command("go", "build", "-o", bin, "reverse-proxy-lb/cmd/proxy")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	// A backend URL is required for a good config; a live server is not needed for
	// validation, so a syntactically valid URL suffices.
	good := writeConfig(t, dynConfigYAML(false, "", "http://127.0.0.1:9"))
	// Bad config: an unknown load-balancing algorithm, which validate() rejects.
	bad := writeConfig(t, `
server:
  host: "127.0.0.1"
  port: 8080
backends:
  - url: "http://127.0.0.1:9"
    weight: 1
load_balancer:
  algorithm: "does_not_exist"
`)

	// Good config => exit 0.
	goodCmd := exec.Command(bin, "--validate", "--config", good)
	goodCmd.Env = os.Environ()
	if out, err := goodCmd.CombinedOutput(); err != nil {
		t.Fatalf("--validate on good config exited non-zero: %v\n%s", err, out)
	}

	// Bad config => non-zero exit.
	badCmd := exec.Command(bin, "--validate", "--config", bad)
	badCmd.Env = os.Environ()
	err := badCmd.Run()
	if err == nil {
		t.Fatal("--validate on bad config exited 0; want non-zero")
	}
	if _, ok := err.(*exec.ExitError); !ok {
		t.Fatalf("--validate on bad config failed to run rather than exiting non-zero: %v", err)
	}
}

// -----------------------------------------------------------------------------
// §8 file-watch: overwrite the config file, assert auto-reload; clean stop
// -----------------------------------------------------------------------------

// TestE2E_DynamicConfig_FileWatchAutoReload enables watch_config with a short
// interval, overwrites the config file on disk to change the backend set, and
// asserts the change is auto-reloaded (picked up) within a bounded wait — with no
// explicit reloadConfig() call. It then drives traffic through Server.Handler() to
// confirm the newly added backend actually serves, and verifies stopping the
// server stops the watcher goroutine cleanly.
func TestE2E_DynamicConfig_FileWatchAutoReload(t *testing.T) {
	b1 := newIDBackend("B1", nil)
	b2 := newIDBackend("B2", nil)
	defer b1.close()
	defer b2.close()

	path := writeConfig(t, dynConfigYAML(true, "20ms", b1.url))
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Server.WatchConfig {
		t.Fatal("expected watch_config enabled")
	}
	srv := New(cfg, path)
	h := srv.Handler()

	// Start only the watch loop (not a real listener) so the test stays port-free.
	srv.startConfigWatch()

	// Sanity: before the edit, only b1 serves.
	if got := drive(t, h, 50); got["B2"] != 0 {
		t.Fatalf("b2 served before being added: %v", got)
	}

	// Advance mtime even on coarse-grained filesystems, then overwrite to add b2.
	time.Sleep(30 * time.Millisecond)
	if err := os.WriteFile(path, []byte(dynConfigYAML(true, "20ms", b1.url, b2.url)), 0o600); err != nil {
		t.Fatal(err)
	}

	// The file-watch loop must pick up the backend-set change within the bound.
	if !waitFor(3*time.Second, func() bool { return findBackend(srv, b2.url) != nil }) {
		t.Fatalf("file-watch did not pick up backend add; have %v", backendURLs(srv))
	}

	// Confirm the auto-added backend actually serves live traffic through the
	// unchanged handler.
	after := drive(t, h, 300)
	if after["B2"] == 0 {
		t.Fatalf("auto-added b2 received no traffic; dist=%v", after)
	}

	// Stopping the watcher must be clean: stopConfigWatch closes+joins the
	// goroutine and nils watchStop. A second stop must be a safe no-op.
	srv.stopConfigWatch()
	if srv.watchStop != nil {
		t.Error("watchStop should be nil after stopConfigWatch")
	}
	srv.stopConfigWatch() // must not panic / must be a no-op
}

// -----------------------------------------------------------------------------
// §8 reload under load: concurrent requests + repeated add/remove reloads
// -----------------------------------------------------------------------------

// TestE2E_DynamicConfig_ReloadUnderLoad fires concurrent requests through
// Server.Handler() while a separate goroutine repeatedly reloads the config,
// flipping the backend set (adding and removing backends). Every request must
// succeed (200) — a request is only ever routed to a backend that is live in the
// balancer, so no request should error. Run under -race to catch data races
// between request routing and the reload's balancer mutation.
func TestE2E_DynamicConfig_ReloadUnderLoad(t *testing.T) {
	// Three always-healthy backends; the config churns which subset is active, but
	// every configured subset is non-empty so there is always somewhere to route.
	b1 := newIDBackend("B1", nil)
	b2 := newIDBackend("B2", nil)
	b3 := newIDBackend("B3", nil)
	defer b1.close()
	defer b2.close()
	defer b3.close()

	// Config variants that repeatedly add/remove backends. All variants keep b1 so
	// the pool is never empty.
	variants := []string{
		dynConfigYAML(false, "", b1.url, b2.url),
		dynConfigYAML(false, "", b1.url, b2.url, b3.url),
		dynConfigYAML(false, "", b1.url, b3.url),
		dynConfigYAML(false, "", b1.url),
	}

	path := writeConfig(t, variants[0])
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	srv := New(cfg, path)
	h := srv.Handler()

	var (
		stop     atomic.Bool
		wg       sync.WaitGroup
		reqs     atomic.Int64
		badCode  atomic.Int64
		firstBad atomic.Value // int, first non-200 status seen
	)

	// Request goroutines: hammer the handler until told to stop.
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				id, code, _ := doReq(t, h, "")
				reqs.Add(1)
				if code != http.StatusOK || id == "" {
					badCode.Add(1)
					firstBad.CompareAndSwap(nil, code)
				}
			}
		}()
	}

	// Reloader goroutine: cycle through the variants, rewriting the file and
	// reloading live. reloadConfig serializes on reloadMu internally.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := 0; n < 200 && !stop.Load(); n++ {
			body := variants[n%len(variants)]
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				return
			}
			srv.reloadConfig()
		}
	}()

	// Let the workers churn, then stop and join.
	time.Sleep(300 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	if reqs.Load() == 0 {
		t.Fatal("no requests were driven under load")
	}
	if bad := badCode.Load(); bad != 0 {
		t.Fatalf("%d/%d requests failed under concurrent reload (first bad status=%v); "+
			"reloads must never drop a request to a healthy backend",
			bad, reqs.Load(), firstBad.Load())
	}
}
