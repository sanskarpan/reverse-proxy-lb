package balancer

import (
	"reverse-proxy-lb/internal/config"
	"testing"
)

func TestNewBackend(t *testing.T) {
	cfg := config.BackendConfig{
		URL:      "http://localhost:8080",
		Weight:   2,
		MaxConns: 50,
	}

	backend := NewBackend(cfg)

	if backend.URL != cfg.URL {
		t.Errorf("Expected URL %s, got %s", cfg.URL, backend.URL)
	}
	if backend.GetWeight() != cfg.Weight {
		t.Errorf("Expected Weight %d, got %d", cfg.Weight, backend.GetWeight())
	}
	if backend.MaxConns != cfg.MaxConns {
		t.Errorf("Expected MaxConns %d, got %d", cfg.MaxConns, backend.MaxConns)
	}
	if !backend.IsHealthy() {
		t.Error("Expected backend to be healthy")
	}
}

func TestBackendConnectionTracking(t *testing.T) {
	backend := NewBackend(config.BackendConfig{
		URL: "http://localhost:8080",
	})

	backend.IncrConn()
	if backend.GetActiveConns() != 1 {
		t.Errorf("Expected 1 active connection, got %d", backend.GetActiveConns())
	}

	backend.IncrConn()
	if backend.GetActiveConns() != 2 {
		t.Errorf("Expected 2 active connections, got %d", backend.GetActiveConns())
	}

	backend.DecrConn()
	if backend.GetActiveConns() != 1 {
		t.Errorf("Expected 1 active connection after decrement, got %d", backend.GetActiveConns())
	}
}

func TestBackendFailureTracking(t *testing.T) {
	backend := NewBackend(config.BackendConfig{
		URL: "http://localhost:8080",
	})

	backend.RecordFailure()
	if backend.GetFailures() != 1 {
		t.Errorf("Expected 1 failure, got %d", backend.GetFailures())
	}

	backend.RecordSuccess()
	if backend.GetFailures() != 0 {
		t.Errorf("Expected 0 failures after success, got %d", backend.GetFailures())
	}
	if backend.GetSuccesses() != 1 {
		t.Errorf("Expected 1 success, got %d", backend.GetSuccesses())
	}
}

func TestRoundRobin(t *testing.T) {
	rr := NewRoundRobin()

	backend1 := NewBackend(config.BackendConfig{URL: "http://localhost:8001"})
	backend2 := NewBackend(config.BackendConfig{URL: "http://localhost:8002"})
	backend3 := NewBackend(config.BackendConfig{URL: "http://localhost:8003"})

	rr.Add(backend1)
	rr.Add(backend2)
	rr.Add(backend3)

	results := make(map[string]int)
	for i := 0; i < 9; i++ {
		b, err := rr.Next()
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		results[b.URL]++
	}

	expected := 3
	for url, count := range results {
		if count != expected {
			t.Errorf("Expected %s to have %d requests, got %d", url, expected, count)
		}
	}
}

func TestRoundRobinNoHealthyBackends(t *testing.T) {
	rr := NewRoundRobin()

	backend := NewBackend(config.BackendConfig{URL: "http://localhost:8001"})
	backend.SetHealthy(false)
	rr.Add(backend)

	_, err := rr.Next()
	if err == nil {
		t.Error("Expected error when no healthy backends")
	}
}

func TestLeastConnections(t *testing.T) {
	lc := NewLeastConnections()

	backend1 := NewBackend(config.BackendConfig{URL: "http://localhost:8001"})
	backend2 := NewBackend(config.BackendConfig{URL: "http://localhost:8002"})

	lc.Add(backend1)
	lc.Add(backend2)

	backend1.IncrConn()
	backend1.IncrConn()

	b, err := lc.Next()
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if b.URL != "http://localhost:8002" {
		t.Errorf("Expected backend2 (least connections), got %s", b.URL)
	}
}

func TestWeightedRoundRobin(t *testing.T) {
	wrr := NewWeightedRoundRobin()

	backend1 := NewBackend(config.BackendConfig{URL: "http://localhost:8001", Weight: 1})
	backend2 := NewBackend(config.BackendConfig{URL: "http://localhost:8002", Weight: 2})

	wrr.Add(backend1)
	wrr.Add(backend2)

	results := make(map[string]int)
	for i := 0; i < 6; i++ {
		b, err := wrr.Next()
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		results[b.URL]++
	}

	if results["http://localhost:8001"] != 2 {
		t.Errorf("Expected server1 to have 2 requests (weight 1), got %d", results["http://localhost:8001"])
	}
	if results["http://localhost:8002"] != 4 {
		t.Errorf("Expected server2 to have 4 requests (weight 2), got %d", results["http://localhost:8002"])
	}
}

func TestBalancerAddRemove(t *testing.T) {
	rr := NewRoundRobin()

	backend1 := NewBackend(config.BackendConfig{URL: "http://localhost:8001"})
	backend2 := NewBackend(config.BackendConfig{URL: "http://localhost:8002"})

	rr.Add(backend1)
	rr.Add(backend2)

	if len(rr.All()) != 2 {
		t.Errorf("Expected 2 backends, got %d", len(rr.All()))
	}

	rr.Remove(backend1)

	if len(rr.All()) != 1 {
		t.Errorf("Expected 1 backend after removal, got %d", len(rr.All()))
	}

	b, err := rr.Next()
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if b.URL != backend2.URL {
		t.Errorf("Expected backend2, got %s", b.URL)
	}
}

// ID 4 + ID 10: exercise concurrent health toggling, selection, and reservation so
// the race detector actually validates the atomic Healthy flag and reserve-on-select.
func TestConcurrentSelectionAndHealthToggle(t *testing.T) {
	for _, b := range []Balancer{NewRoundRobin(), NewLeastConnections(), NewWeightedRoundRobin()} {
		bal := b
		be1 := NewBackend(config.BackendConfig{URL: "http://localhost:8001", Weight: 1, MaxConns: 1000})
		be2 := NewBackend(config.BackendConfig{URL: "http://localhost:8002", Weight: 2, MaxConns: 1000})
		bal.Add(be1)
		bal.Add(be2)

		done := make(chan struct{})
		go func() {
			for i := 0; i < 2000; i++ {
				be1.SetHealthy(i%2 == 0)
			}
			close(done)
		}()

		for i := 0; i < 2000; i++ {
			if sel, err := bal.Next(); err == nil && sel != nil {
				sel.DecrConn() // release the reservation Next made
			}
			bal.GetHealthy()
		}
		<-done
		be1.SetHealthy(true)
	}
}
