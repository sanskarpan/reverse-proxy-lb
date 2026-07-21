package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"reverse-proxy-lb/internal/canary"
	"reverse-proxy-lb/internal/config"
)

// TestHandleAdminCanaryStatus_Disabled verifies that when no auto-promoter is
// configured the endpoint returns HTTP 200 with {"enabled":false}.
func TestHandleAdminCanaryStatus_Disabled(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	s := opsTestServer(t, be.URL)
	// autoPromoter is nil by default (canary not configured).
	if s.autoPromoter != nil {
		t.Fatal("expected autoPromoter to be nil in opsTestServer")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/canary/status", nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type: want application/json, got %q", ct)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal body: %v (body=%q)", err, rec.Body.String())
	}
	if enabled, ok := got["enabled"]; !ok || enabled != false {
		t.Errorf("want enabled=false, got %v", got)
	}
}

// TestHandleAdminCanaryStatus_MethodNotAllowed verifies that non-GET methods
// are rejected with 405.
func TestHandleAdminCanaryStatus_MethodNotAllowed(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	s := opsTestServer(t, be.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/canary/status", nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rec.Code)
	}
}

// mockWeightUpdater is a minimal weightUpdater test double used so we can
// construct a canary.AutoPromoter without a real proxy.
type mockWeightUpdater struct{}

func (m *mockWeightUpdater) UpdateCanaryWeight(_ int) {}

// mockMetricsSnap is a minimal metricsSnapshot double that returns 0 traffic.
type mockMetricsSnap struct{}

func (m *mockMetricsSnap) CanarySnapshot() (int64, int64) { return 0, 0 }

// TestHandleAdminCanaryStatus_Enabled verifies that when an autoPromoter is
// wired up the endpoint returns a valid JSON AutoPromoterStatus payload.
func TestHandleAdminCanaryStatus_Enabled(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer be.Close()

	s := opsTestServer(t, be.URL)

	cfg := config.AutoPromoteConfig{
		Enabled:               true,
		StepPercent:           15,
		MaxWeightPercent:      80,
		ErrorRateThreshold:    0.05,
		MinRequests:           50,
		RollbackOnDegradation: true,
	}
	// Use a 1s step interval so the background goroutine does not fire during
	// the test; we call Status() directly via the handler.
	cfg.StepInterval = 1<<63 - 1 // max duration ≈ 292 years

	ap := canary.New(&mockWeightUpdater{}, &mockMetricsSnap{}, cfg)
	s.autoPromoter = ap

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/canary/status", nil)
	s.metricsMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type: want application/json, got %q", ct)
	}

	var got canary.AutoPromoterStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal body: %v (body=%q)", err, rec.Body.String())
	}
	if !got.Enabled {
		t.Errorf("want enabled=true, got %v", got.Enabled)
	}
	if got.CurrentWeight != 0 {
		t.Errorf("want current_weight=0, got %d", got.CurrentWeight)
	}
	if got.MaxWeight != 80 {
		t.Errorf("want max_weight=80, got %d", got.MaxWeight)
	}
	if got.StepPercent != 15 {
		t.Errorf("want step_percent=15, got %d", got.StepPercent)
	}
	if got.RollbackCount != 0 {
		t.Errorf("want rollback_count=0, got %d", got.RollbackCount)
	}
}
