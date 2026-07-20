package balancer

import (
	"errors"
	"math/rand"
	"sync"
	"time"
)

// WeightedRandom selects a healthy backend at random with probability
// proportional to its configured weight. Over many picks the distribution
// converges to the weight ratios, with no ordering or burst artifacts.
type WeightedRandom struct {
	BaseBalancer
	mu  sync.Mutex
	rng *rand.Rand
}

func NewWeightedRandom() *WeightedRandom {
	return &WeightedRandom{rng: rand.New(rand.NewSource(time.Now().UnixNano()))} // #nosec G404 -- non-crypto weighted selection
}

func (w *WeightedRandom) Next() (*Backend, error) {
	healthy := w.GetHealthy()
	if len(healthy) == 0 {
		return nil, errors.New("no healthy backends")
	}

	total := 0
	for _, b := range healthy {
		weight := b.GetWeight()
		if weight <= 0 {
			weight = 1
		}
		total += weight
	}

	w.mu.Lock()
	r := w.rng.Intn(total)
	w.mu.Unlock()

	var selected *Backend
	for _, b := range healthy {
		weight := b.GetWeight()
		if weight <= 0 {
			weight = 1
		}
		r -= weight
		if r < 0 {
			selected = b
			break
		}
	}
	if selected == nil {
		selected = healthy[len(healthy)-1]
	}

	// Reserve the slot at selection time; the caller releases via DecrConn.
	selected.IncrConn()
	return selected, nil
}
