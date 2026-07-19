package health

import (
	"net"
	"net/http"
	"net/http/httptest"
	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/config"
	"testing"
	"time"
)

// newBalancer builds a round-robin balancer seeded with backends for the given
// URLs. Backends start healthy (NewBackend default).
func newBalancer(urls ...string) balancer.Balancer {
	b := balancer.NewRoundRobin()
	for _, u := range urls {
		b.Add(balancer.NewBackend(config.BackendConfig{URL: u, Weight: 1, MaxConns: 100}))
	}
	return b
}

func backendFor(b balancer.Balancer, url string) *balancer.Backend {
	for _, bk := range b.All() {
		if bk.URL == url {
			return bk
		}
	}
	return nil
}

// checkOnce runs a single synchronous pass so tests are deterministic and fast
// without depending on the ticker.
func (h *HealthChecker) checkOnce() {
	h.checkAll()
}

func baseConfig() config.HealthCheckConfig {
	return config.HealthCheckConfig{
		Enabled:            true,
		Interval:           time.Millisecond,
		Timeout:            time.Second,
		Path:               "/health",
		Type:               "http",
		Method:             http.MethodGet,
		HealthyThreshold:   2,
		UnhealthyThreshold: 3,
		Jitter:             0.1,
	}
}

func TestHTTPHealthy2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b := newBalancer(srv.URL)
	be := backendFor(b, srv.URL)
	be.SetHealthy(false) // start unhealthy to observe promotion

	cfg := baseConfig()
	cfg.HealthyThreshold = 2
	hc := NewHealthChecker(b, cfg, nil, nil)
	hc.startedAt = time.Now()

	hc.checkOnce()
	if be.IsHealthy() {
		t.Fatalf("backend healthy after 1 success, want still unhealthy (threshold=2)")
	}
	hc.checkOnce()
	if !be.IsHealthy() {
		t.Fatalf("backend not healthy after 2 successes")
	}
}

func TestHTTPUnhealthy500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	b := newBalancer(srv.URL)
	be := backendFor(b, srv.URL)

	cfg := baseConfig()
	cfg.UnhealthyThreshold = 3
	hc := NewHealthChecker(b, cfg, nil, nil)
	hc.startedAt = time.Now()

	hc.checkOnce()
	hc.checkOnce()
	if !be.IsHealthy() {
		t.Fatalf("backend unhealthy after 2 failures, want still healthy (threshold=3)")
	}
	hc.checkOnce()
	if be.IsHealthy() {
		t.Fatalf("backend still healthy after 3 failures")
	}
}

func TestExpectedStatusesCustom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent) // 204
	}))
	defer srv.Close()

	// Default (any 2xx) accepts 204.
	b := newBalancer(srv.URL)
	be := backendFor(b, srv.URL)
	be.SetHealthy(false)
	cfg := baseConfig()
	cfg.HealthyThreshold = 1
	hc := NewHealthChecker(b, cfg, nil, nil)
	hc.startedAt = time.Now()
	hc.checkOnce()
	if !be.IsHealthy() {
		t.Fatalf("204 not accepted by default 2xx rule")
	}

	// Explicit ExpectedStatuses that excludes 204 -> failure.
	b2 := newBalancer(srv.URL)
	be2 := backendFor(b2, srv.URL)
	cfg2 := baseConfig()
	cfg2.UnhealthyThreshold = 1
	cfg2.ExpectedStatuses = []int{200}
	hc2 := NewHealthChecker(b2, cfg2, nil, nil)
	hc2.startedAt = time.Now()
	hc2.checkOnce()
	if be2.IsHealthy() {
		t.Fatalf("204 accepted when ExpectedStatuses=[200]")
	}

	// Explicit ExpectedStatuses that includes 204 -> success.
	b3 := newBalancer(srv.URL)
	be3 := backendFor(b3, srv.URL)
	be3.SetHealthy(false)
	cfg3 := baseConfig()
	cfg3.HealthyThreshold = 1
	cfg3.ExpectedStatuses = []int{204}
	hc3 := NewHealthChecker(b3, cfg3, nil, nil)
	hc3.startedAt = time.Now()
	hc3.checkOnce()
	if !be3.IsHealthy() {
		t.Fatalf("204 not accepted when ExpectedStatuses=[204]")
	}
}

func TestExpectedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("status: OK and ready"))
	}))
	defer srv.Close()

	// Match.
	b := newBalancer(srv.URL)
	be := backendFor(b, srv.URL)
	be.SetHealthy(false)
	cfg := baseConfig()
	cfg.HealthyThreshold = 1
	cfg.ExpectedBody = "ready"
	hc := NewHealthChecker(b, cfg, nil, nil)
	hc.startedAt = time.Now()
	hc.checkOnce()
	if !be.IsHealthy() {
		t.Fatalf("body match not treated as healthy")
	}

	// Mismatch.
	b2 := newBalancer(srv.URL)
	be2 := backendFor(b2, srv.URL)
	cfg2 := baseConfig()
	cfg2.UnhealthyThreshold = 1
	cfg2.ExpectedBody = "DEGRADED"
	hc2 := NewHealthChecker(b2, cfg2, nil, nil)
	hc2.startedAt = time.Now()
	hc2.checkOnce()
	if be2.IsHealthy() {
		t.Fatalf("body mismatch not treated as unhealthy")
	}
}

func TestHostHeaderSent(t *testing.T) {
	gotHost := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost <- r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b := newBalancer(srv.URL)
	cfg := baseConfig()
	cfg.HealthyThreshold = 1
	cfg.Host = "virtual.example.com"
	hc := NewHealthChecker(b, cfg, nil, nil)
	hc.startedAt = time.Now()
	hc.checkOnce()

	select {
	case h := <-gotHost:
		if h != "virtual.example.com" {
			t.Fatalf("Host header = %q, want virtual.example.com", h)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not receive request")
	}
}

func TestHeadersSent(t *testing.T) {
	gotAuth := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("X-Probe")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b := newBalancer(srv.URL)
	cfg := baseConfig()
	cfg.HealthyThreshold = 1
	cfg.Headers = map[string]string{"X-Probe": "abc"}
	hc := NewHealthChecker(b, cfg, nil, nil)
	hc.startedAt = time.Now()
	hc.checkOnce()

	select {
	case v := <-gotAuth:
		if v != "abc" {
			t.Fatalf("X-Probe header = %q, want abc", v)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not receive request")
	}
}

func TestTCPCheckSuccess(t *testing.T) {
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
			c.Close()
		}
	}()

	url := "http://" + ln.Addr().String()
	b := newBalancer(url)
	be := backendFor(b, url)
	be.SetHealthy(false)
	cfg := baseConfig()
	cfg.Type = "tcp"
	cfg.HealthyThreshold = 1
	hc := NewHealthChecker(b, cfg, nil, nil)
	hc.startedAt = time.Now()
	hc.checkOnce()
	if !be.IsHealthy() {
		t.Fatalf("TCP dial to live listener not treated as healthy")
	}
}

func TestTCPCheckFailure(t *testing.T) {
	// Bind then close to obtain a port that is very likely closed.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	url := "http://" + addr
	b := newBalancer(url)
	be := backendFor(b, url)
	cfg := baseConfig()
	cfg.Type = "tcp"
	cfg.Timeout = 200 * time.Millisecond
	cfg.UnhealthyThreshold = 1
	hc := NewHealthChecker(b, cfg, nil, nil)
	hc.startedAt = time.Now()
	hc.checkOnce()
	if be.IsHealthy() {
		t.Fatalf("TCP dial to closed port not treated as unhealthy")
	}
}

func TestRiseThresholdConsecutive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b := newBalancer(srv.URL)
	be := backendFor(b, srv.URL)
	be.SetHealthy(false)
	cfg := baseConfig()
	cfg.HealthyThreshold = 3
	hc := NewHealthChecker(b, cfg, nil, nil)
	hc.startedAt = time.Now()

	hc.checkOnce()
	hc.checkOnce()
	if be.IsHealthy() {
		t.Fatalf("healthy after 2 successes, want threshold=3")
	}
	hc.checkOnce()
	if !be.IsHealthy() {
		t.Fatalf("not healthy after 3 successes")
	}
}

func TestFallThresholdConsecutive(t *testing.T) {
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b := newBalancer(srv.URL)
	be := backendFor(b, srv.URL)
	cfg := baseConfig()
	cfg.UnhealthyThreshold = 3
	hc := NewHealthChecker(b, cfg, nil, nil)
	hc.startedAt = time.Now()

	fail = true
	hc.checkOnce()
	hc.checkOnce()
	if !be.IsHealthy() {
		t.Fatalf("unhealthy after 2 failures, want threshold=3")
	}
	hc.checkOnce()
	if be.IsHealthy() {
		t.Fatalf("still healthy after 3 failures")
	}
}

func TestNonConsecutiveDoesNotEject(t *testing.T) {
	state := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(state)
	}))
	defer srv.Close()

	b := newBalancer(srv.URL)
	be := backendFor(b, srv.URL)
	cfg := baseConfig()
	cfg.UnhealthyThreshold = 3
	hc := NewHealthChecker(b, cfg, nil, nil)
	hc.startedAt = time.Now()

	// fail, fail, success, fail, fail -> never 3 consecutive failures.
	state = http.StatusInternalServerError
	hc.checkOnce()
	hc.checkOnce()
	state = http.StatusOK
	hc.checkOnce()
	state = http.StatusInternalServerError
	hc.checkOnce()
	hc.checkOnce()
	if !be.IsHealthy() {
		t.Fatalf("ejected without 3 consecutive failures")
	}
}

func TestStartupGraceSuppressesEjection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	b := newBalancer(srv.URL)
	be := backendFor(b, srv.URL)
	cfg := baseConfig()
	cfg.UnhealthyThreshold = 1
	cfg.StartupGracePeriod = 200 * time.Millisecond
	hc := NewHealthChecker(b, cfg, nil, nil)
	hc.startedAt = time.Now()

	// Within grace: repeated failures must not eject.
	hc.checkOnce()
	hc.checkOnce()
	hc.checkOnce()
	if !be.IsHealthy() {
		t.Fatalf("backend ejected during startup grace period")
	}

	// After grace elapses, failures eject.
	hc.startedAt = time.Now().Add(-time.Second)
	hc.checkOnce()
	if be.IsHealthy() {
		t.Fatalf("backend not ejected after grace period elapsed")
	}
}

func TestStartupGraceAllowsPromotion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b := newBalancer(srv.URL)
	be := backendFor(b, srv.URL)
	be.SetHealthy(false)
	cfg := baseConfig()
	cfg.HealthyThreshold = 1
	cfg.StartupGracePeriod = time.Second
	hc := NewHealthChecker(b, cfg, nil, nil)
	hc.startedAt = time.Now()

	hc.checkOnce()
	if !be.IsHealthy() {
		t.Fatalf("promotion suppressed during startup grace, want allowed")
	}
}

func TestPerBackendOverride(t *testing.T) {
	// Global backend returns 200 (healthy under global rule); override backend
	// requires a body substring the server does not send (so it stays down).
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	}))
	defer okSrv.Close()

	b := newBalancer(okSrv.URL)
	be := backendFor(b, okSrv.URL)

	global := baseConfig()
	global.HealthyThreshold = 1
	global.UnhealthyThreshold = 1

	override := baseConfig()
	override.HealthyThreshold = 1
	override.UnhealthyThreshold = 1
	override.ExpectedBody = "not-present"

	overrides := map[string]config.HealthCheckConfig{okSrv.URL: override}
	hc := NewHealthChecker(b, global, overrides, nil)
	hc.startedAt = time.Now()

	hc.checkOnce()
	if be.IsHealthy() {
		t.Fatalf("override (ExpectedBody mismatch) ignored; backend stayed healthy")
	}

	// Without the override, the same server is healthy under the global rule.
	b2 := newBalancer(okSrv.URL)
	be2 := backendFor(b2, okSrv.URL)
	be2.SetHealthy(false)
	hc2 := NewHealthChecker(b2, global, nil, nil)
	hc2.startedAt = time.Now()
	hc2.checkOnce()
	if !be2.IsHealthy() {
		t.Fatalf("global config did not mark backend healthy")
	}
}

func TestStartStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b := newBalancer(srv.URL)
	cfg := baseConfig()
	cfg.Interval = 5 * time.Millisecond
	hc := NewHealthChecker(b, cfg, nil, nil)
	hc.Start()
	time.Sleep(30 * time.Millisecond)
	hc.Stop()
}
