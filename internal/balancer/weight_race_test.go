package balancer

import (
	"reverse-proxy-lb/internal/config"
	"sync"
	"testing"
)

// Regression (ISSUES): admin /admin/weight (UpdateWeight) must be race-safe against
// the weighted selection hot path. Backend.Weight is now atomic. Run under -race.
func TestConcurrentUpdateWeightAndSelect(t *testing.T) {
	for _, bal := range []Balancer{NewSWRR(), NewWeightedRandom(), NewWeightedRoundRobin(), NewWeightedLeastConn()} {
		b := bal
		be1 := NewBackend(config.BackendConfig{URL: "http://a", Weight: 1, MaxConns: 1000})
		be2 := NewBackend(config.BackendConfig{URL: "http://b", Weight: 2, MaxConns: 1000})
		b.Add(be1)
		b.Add(be2)

		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			for i := 0; i < 3000; i++ {
				b.UpdateWeight(be1, (i%5)+1)
			}
		}()
		for g := 0; g < 2; g++ {
			go func() {
				defer wg.Done()
				for i := 0; i < 3000; i++ {
					if sel, err := b.Next(); err == nil && sel != nil {
						sel.DecrConn()
					}
				}
			}()
		}
		wg.Wait()
	}
}
