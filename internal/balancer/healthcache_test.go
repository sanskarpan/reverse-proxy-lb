package balancer

import (
	"reverse-proxy-lb/internal/config"
	"testing"
)

// The cached GetHealthy must reflect a health change immediately (no staleness).
func TestGetHealthyCacheReflectsChanges(t *testing.T) {
	rr := NewRoundRobin()
	a := NewBackend(config.BackendConfig{URL: "http://a"})
	b := NewBackend(config.BackendConfig{URL: "http://b"})
	rr.Add(a)
	rr.Add(b)

	if got := len(rr.GetHealthy()); got != 2 {
		t.Fatalf("initial healthy = %d, want 2", got)
	}
	// second call returns the cached slice (same backing array)
	h1, h2 := rr.GetHealthy(), rr.GetHealthy()
	if len(h1) != 2 || len(h2) != 2 {
		t.Fatalf("cached healthy len mismatch")
	}
	a.SetHealthy(false)
	if got := len(rr.GetHealthy()); got != 1 { // must rebuild
		t.Errorf("after ejecting a: healthy = %d, want 1", got)
	}
	a.SetHealthy(true)
	if got := len(rr.GetHealthy()); got != 2 {
		t.Errorf("after reinstating a: healthy = %d, want 2", got)
	}
	// topology change invalidates too
	rr.Remove(b)
	if got := len(rr.GetHealthy()); got != 1 {
		t.Errorf("after removing b: healthy = %d, want 1", got)
	}
}
