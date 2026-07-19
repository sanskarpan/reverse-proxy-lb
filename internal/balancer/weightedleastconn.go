package balancer

import (
	"errors"
	"sync"
)

// WeightedLeastConn selects the healthy backend with the smallest load
// normalized by weight, i.e. min(activeConns / weight). A backend with weight 2
// can therefore carry twice the connections of a weight-1 backend before being
// considered equally loaded. This balances both connection count and capacity.
type WeightedLeastConn struct {
	BaseBalancer
	mu sync.Mutex
}

func NewWeightedLeastConn() *WeightedLeastConn {
	return &WeightedLeastConn{}
}

func (w *WeightedLeastConn) Next() (*Backend, error) {
	healthy := w.GetHealthy()
	if len(healthy) == 0 {
		return nil, errors.New("no healthy backends")
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	var selected *Backend
	var bestScore float64
	for _, b := range healthy {
		weight := b.GetWeight()
		if weight <= 0 {
			weight = 1
		}
		score := float64(b.GetActiveConns()) / float64(weight)
		if selected == nil || score < bestScore {
			bestScore = score
			selected = b
		}
	}

	// Reserve while holding the lock so concurrent selections observe the update
	// and don't herd onto the same backend. The caller releases via DecrConn.
	if selected != nil {
		selected.IncrConn()
	}
	return selected, nil
}
