package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/config"
	"testing"
	"time"
)

// Regression (ISSUES 29): when the primary responds before the hedge delay fires,
// the extra backends were reserved (IncrConn) but never launched, and their
// DecrConn lived in a goroutine that never started — leaking one reservation per
// hedged request. Leaked reservations eventually trip the MaxConns bulkhead and
// exclude the backend. After a hedged request, every backend's ActiveConns must
// return to zero.
func TestHedgePrimaryWinsNoReservationLeak(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "P")
	}))
	defer primary.Close()
	extra := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "E")
	}))
	defer extra.Close()

	rr := balancer.NewRoundRobin()
	pB := balancer.NewBackend(config.BackendConfig{URL: primary.URL, MaxConns: 100})
	eB := balancer.NewBackend(config.BackendConfig{URL: extra.URL, MaxConns: 100})
	rr.Add(pB)
	rr.Add(eB)

	// Large hedge delay so the fast primary always wins before extras are launched.
	retry := config.RetryConfig{
		Hedge: config.HedgeConfig{Enabled: true, Delay: 300 * time.Millisecond, MaxExtra: 1},
	}
	p := New(rr, nil, retry, "round_robin", nil, nil, config.UpstreamConfig{})

	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
		req.RemoteAddr = "203.0.113.9:5555"
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, rec.Code)
		}
	}

	// Allow any in-flight goroutines to settle.
	time.Sleep(50 * time.Millisecond)

	if c := pB.GetActiveConns(); c != 0 {
		t.Errorf("primary leaked reservations: ActiveConns=%d, want 0", c)
	}
	if c := eB.GetActiveConns(); c != 0 {
		t.Errorf("extra (hedge) leaked reservations: ActiveConns=%d, want 0", c)
	}
}
