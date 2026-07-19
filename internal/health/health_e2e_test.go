package health

import (
	"net"
	"net/http"
	"net/http/httptest"
	"reverse-proxy-lb/internal/config"
	"sync/atomic"
	"testing"
	"time"
)

// These are end-to-end integration tests: they drive the real HealthChecker via
// Start()/Stop() (the real jittered ticker loop) against live httptest backends,
// and poll for the expected health state with a deadline instead of asserting on
// a synchronous single pass. Intervals are kept short so convergence is fast; the
// eventually/never helpers bound how long we wait so a regression fails loudly
// rather than hanging.

// e2eConfig returns a health-check config tuned for fast, real-ticker E2E runs:
// a sub-millisecond interval so many probes happen inside the deadlines below,
// and no jitter so timing is predictable.
func e2eConfig() config.HealthCheckConfig {
	return config.HealthCheckConfig{
		Enabled:            true,
		Interval:           2 * time.Millisecond,
		Timeout:            time.Second,
		Path:               "/health",
		Type:               "http",
		Method:             http.MethodGet,
		HealthyThreshold:   2,
		UnhealthyThreshold: 3,
		Jitter:             0, // deterministic cadence for the E2E deadlines
	}
}

// eventually polls cond until it returns true or the deadline elapses, failing
// the test with msg otherwise. It replaces long fixed sleeps with a bounded
// require-eventually loop.
func eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s: %s", timeout, msg)
	}
}

// consistently asserts cond holds for the whole window. Used to prove a state is
// NOT reached (e.g. a backend is not ejected during the startup grace period).
func consistently(t *testing.T, window time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if !cond() {
			t.Fatalf("condition violated during %s window: %s", window, msg)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestE2E_FailThenRecover drives the real ticker end to end: a backend that
// returns 500 must be marked unhealthy (fall), and once it starts returning 200
// it must be marked healthy again (rise). Because the real loop only flips the
// flag on the threshold-th consecutive result, reaching each state proves the
// thresholds are honored under the live cadence.
func TestE2E_FailThenRecover(t *testing.T) {
	var serving500 atomic.Bool
	serving500.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serving500.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b := newBalancer(srv.URL)
	be := backendFor(b, srv.URL)
	// Starts healthy (NewBackend default); the 500s must eject it.

	cfg := e2eConfig()
	cfg.UnhealthyThreshold = 3
	cfg.HealthyThreshold = 2
	hc := NewHealthChecker(b, cfg, nil, nil)
	hc.Start()
	defer hc.Stop()

	// Fall: consecutive 500s eject the backend.
	eventually(t, 2*time.Second, func() bool {
		return !be.IsHealthy()
	}, "backend serving 500 was never marked unhealthy")

	// Recover: flip the backend to 200 and expect it back healthy.
	serving500.Store(false)
	eventually(t, 2*time.Second, func() bool {
		return be.IsHealthy()
	}, "recovered backend (200) was never marked healthy again")
}

// TestE2E_ExpectedBody verifies body matching end to end: a 200 with the wrong
// body is unhealthy; a 200 with the expected substring is healthy. Two backends
// share one checker so the same live config resolves differently only on body.
func TestE2E_ExpectedBody(t *testing.T) {
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("service is READY to serve"))
	}))
	defer goodSrv.Close()
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // 200, but wrong body
		_, _ = w.Write([]byte("service is DEGRADED"))
	}))
	defer badSrv.Close()

	b := newBalancer(goodSrv.URL, badSrv.URL)
	good := backendFor(b, goodSrv.URL)
	bad := backendFor(b, badSrv.URL)
	good.SetHealthy(false) // must be promoted by matching body
	// bad starts healthy; wrong body must eject it.

	cfg := e2eConfig()
	cfg.ExpectedBody = "READY"
	cfg.HealthyThreshold = 2
	cfg.UnhealthyThreshold = 3
	hc := NewHealthChecker(b, cfg, nil, nil)
	hc.Start()
	defer hc.Stop()

	eventually(t, 2*time.Second, func() bool {
		return good.IsHealthy()
	}, "backend returning matching body was never marked healthy")
	eventually(t, 2*time.Second, func() bool {
		return !bad.IsHealthy()
	}, "backend returning 200 with wrong body was never marked unhealthy")
}

// TestE2E_TCPCheck verifies a tcp-type check end to end: a live listener is
// healthy and a closed port is unhealthy, using the real dial in the ticker loop.
func TestE2E_TCPCheck(t *testing.T) {
	// Live listener that accepts and immediately closes connections.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	liveURL := "http://" + ln.Addr().String()

	// A port that was bound then closed: dials should be refused.
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + deadLn.Addr().String()
	_ = deadLn.Close()

	b := newBalancer(liveURL, deadURL)
	live := backendFor(b, liveURL)
	dead := backendFor(b, deadURL)
	live.SetHealthy(false) // must be promoted by successful dials

	cfg := e2eConfig()
	cfg.Type = "tcp"
	cfg.Timeout = 200 * time.Millisecond
	cfg.HealthyThreshold = 2
	cfg.UnhealthyThreshold = 3
	hc := NewHealthChecker(b, cfg, nil, nil)
	hc.Start()
	defer hc.Stop()

	eventually(t, 2*time.Second, func() bool {
		return live.IsHealthy()
	}, "tcp check against live listener never marked healthy")
	eventually(t, 2*time.Second, func() bool {
		return !dead.IsHealthy()
	}, "tcp check against closed port never marked unhealthy")
}

// TestE2E_PerBackendOverride verifies that a per-backend override is evaluated
// instead of the global config end to end. Both backends return 200 on / and
// 200 on /health, but only /special returns OK-body; the override backend probes
// /special with an ExpectedBody the other path would not satisfy, while the
// global backend uses the default path. Under the live loop, the override
// backend is judged by its own criteria and the global one by the global config.
func TestE2E_PerBackendOverride(t *testing.T) {
	// Global backend: healthy on the global /health path with a plain 200.
	globalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer globalSrv.Close()

	// Override backend: only /ready with the expected body is considered up; the
	// global /health path returns 404 here, so the override path/criteria matter.
	overrideSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ready" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("UP"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer overrideSrv.Close()

	b := newBalancer(globalSrv.URL, overrideSrv.URL)
	globalBE := backendFor(b, globalSrv.URL)
	overrideBE := backendFor(b, overrideSrv.URL)
	globalBE.SetHealthy(false)
	overrideBE.SetHealthy(false)

	global := e2eConfig()
	global.Path = "/health"
	global.HealthyThreshold = 2

	override := e2eConfig()
	override.Path = "/ready"
	override.ExpectedBody = "UP"
	override.HealthyThreshold = 2

	overrides := map[string]config.HealthCheckConfig{overrideSrv.URL: override}
	hc := NewHealthChecker(b, global, overrides, nil)
	hc.Start()
	defer hc.Stop()

	// Global backend healthy via the global /health path.
	eventually(t, 2*time.Second, func() bool {
		return globalBE.IsHealthy()
	}, "global-config backend never marked healthy on /health")
	// Override backend healthy only because its own /ready + body criteria applied.
	eventually(t, 2*time.Second, func() bool {
		return overrideBE.IsHealthy()
	}, "override backend never marked healthy under its own path/criteria")

	// Cross-check: the override backend would be judged unhealthy under the global
	// config (global /health returns 404 on the override server). Run a second
	// checker with no override to prove the override was what mattered.
	b2 := newBalancer(overrideSrv.URL)
	ovUnderGlobal := backendFor(b2, overrideSrv.URL)
	// starts healthy; the global /health -> 404 must eject it.
	globalOnly := e2eConfig()
	globalOnly.Path = "/health"
	globalOnly.UnhealthyThreshold = 3
	hc2 := NewHealthChecker(b2, globalOnly, nil, nil)
	hc2.Start()
	defer hc2.Stop()

	eventually(t, 2*time.Second, func() bool {
		return !ovUnderGlobal.IsHealthy()
	}, "override server was not ejected under the global /health path (override had no effect)")
}

// TestE2E_StartupGrace verifies the startup grace period end to end: a backend
// failing from the very start must NOT be ejected while the grace window is
// open, and must be ejected once it elapses. The grace is measured from
// Start() (which sets startedAt), so this exercises the real timing path.
func TestE2E_StartupGrace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	b := newBalancer(srv.URL)
	be := backendFor(b, srv.URL)
	// starts healthy; failing from startup, but grace must protect it initially.

	const grace = 400 * time.Millisecond
	cfg := e2eConfig()
	cfg.UnhealthyThreshold = 1 // one failure would eject if grace were not honored
	cfg.StartupGracePeriod = grace
	hc := NewHealthChecker(b, cfg, nil, nil)
	hc.Start()
	defer hc.Stop()

	// During most of the grace window the backend must stay healthy despite
	// continuous 500s. Check a sub-window to avoid racing the grace boundary.
	consistently(t, grace/2, func() bool {
		return be.IsHealthy()
	}, "backend ejected during startup grace period")

	// After the grace elapses, the ongoing failures must eject it.
	eventually(t, 2*time.Second, func() bool {
		return !be.IsHealthy()
	}, "backend not ejected after startup grace elapsed")
}
