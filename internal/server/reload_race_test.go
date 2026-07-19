package server

import (
	"os"
	"sync"
	"testing"

	"reverse-proxy-lb/internal/config"
)

// ENHANCEMENTS 0.12: concurrent reloads (SIGHUP + POST /reload, or two /reload
// requests) must not race on s.cfg. Run under -race.
func TestConcurrentReloadNoRace(t *testing.T) {
	const yaml = `
server:
  host: "127.0.0.1"
  port: 18080
backends:
  - url: "http://127.0.0.1:18001"
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
`
	f, err := os.CreateTemp("", "reload-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(yaml); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := New(cfg, f.Name())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				s.reloadConfig()
			}
		}()
	}
	wg.Wait()
}
