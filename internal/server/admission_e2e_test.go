package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
)

// admissionBackend starts an httptest.Server that sleeps for d before replying
// 200 OK. It simulates a slow upstream so concurrent requests accumulate
// in-flight pressure on the admission gate.
func admissionBackend(t *testing.T, d time.Duration) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(d)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(s.Close)
	return s
}

// admissionConfig returns a minimal *config.Config with only the admission
// ceiling set, wired to the given backend URL.
//
// It mirrors the QueueTimeout default applied by config.Load(): when maxQueue > 0
// and no explicit QueueTimeout has been set, Load() defaults to 5s. Because this
// helper constructs config.Config directly (bypassing Load), it applies the same
// default so future callers that enable queueing do not silently get a zero
// QueueTimeout (which would cause the admission gate to reject immediately).
func admissionConfig(backendURL string, maxInflight, maxQueue int) *config.Config {
	queueTimeout := time.Duration(0)
	if maxQueue > 0 {
		queueTimeout = 5 * time.Second
	}
	return &config.Config{
		Server: config.ServerConfig{
			Host:                "127.0.0.1",
			Port:                8080,
			MaxInflightRequests: maxInflight,
			MaxInflightQueue:    maxQueue,
			QueueTimeout:        queueTimeout,
		},
		Backends: []config.BackendConfig{
			{URL: backendURL, Weight: 1, MaxConns: 100},
		},
		LoadBalancer: config.LoadBalancerConfig{
			Algorithm: "round_robin",
		},
		Logging: config.LoggingConfig{Level: "error", Format: "text"},
	}
}

// TestE2EAdmissionCeiling starts a server with MaxInflightRequests=2 and no
// queue, then fires 10 concurrent requests at a 100ms-slow backend. At most 2
// requests can be in-flight concurrently; the rest must be rejected with 503.
func TestE2EAdmissionCeiling(t *testing.T) {
	be := admissionBackend(t, 100*time.Millisecond)

	cfg := admissionConfig(be.URL, 2, 0)
	h := New(cfg, "").Handler()

	const concurrent = 10
	var wg sync.WaitGroup
	codes := make([]int, concurrent)

	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil)
			req.RemoteAddr = "127.0.0.1:12345"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			codes[idx] = rec.Code
		}(i)
	}
	wg.Wait()

	ok, rejected := 0, 0
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusServiceUnavailable:
			rejected++
		default:
			t.Logf("unexpected status %d", code)
		}
	}

	t.Logf("results: %d ok, %d rejected (503) out of %d concurrent requests", ok, rejected, concurrent)

	// With a ceiling of 2, at most 2 requests should succeed.
	if ok > 2 {
		t.Errorf("got %d successful (200) requests, want at most 2 (admission ceiling is 2)", ok)
	}
	// At least 8 requests should be rejected with 503.
	if rejected < 8 {
		t.Errorf("got %d rejected (503) requests, want at least 8", rejected)
	}
}
