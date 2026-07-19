package balancer

import (
	"errors"
	"sync"
)

// SWRR implements nginx-style smooth weighted round robin. Unlike the classic
// WeightedRoundRobin (which serves each backend in bursts proportional to its
// weight), SWRR interleaves selections so that a backend with weight 3 is spread
// evenly across the cycle rather than picked three times in a row.
//
// The algorithm keeps a per-backend currentWeight. On each pick it adds every
// backend's effective (configured) weight to its currentWeight, selects the
// backend with the greatest currentWeight, then subtracts the total weight from
// the winner's currentWeight. This yields a smooth, deterministic distribution.
type SWRR struct {
	BaseBalancer
	mu      sync.Mutex
	current map[*Backend]int
}

func NewSWRR() *SWRR {
	return &SWRR{current: make(map[*Backend]int)}
}

func (s *SWRR) Next() (*Backend, error) {
	healthy := s.GetHealthy()
	if len(healthy) == 0 {
		return nil, errors.New("no healthy backends")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	total := 0
	var best *Backend
	for _, b := range healthy {
		w := b.GetWeight()
		if w <= 0 {
			w = 1
		}
		total += w
		s.current[b] += w
		if best == nil || s.current[b] > s.current[best] {
			best = b
		}
	}

	if best == nil {
		best = healthy[0]
	}
	s.current[best] -= total

	// Reserve the slot at selection time; the caller releases via DecrConn.
	best.IncrConn()
	return best, nil
}
