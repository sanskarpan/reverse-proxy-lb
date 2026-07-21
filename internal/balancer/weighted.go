package balancer

import (
	"errors"
	"sync/atomic"
)

type WeightedRoundRobin struct {
	BaseBalancer
	current uint32
}

func NewWeightedRoundRobin() *WeightedRoundRobin {
	return &WeightedRoundRobin{}
}

func (w *WeightedRoundRobin) Next() (*Backend, error) {
	healthy := w.GetHealthy()
	if len(healthy) == 0 {
		return nil, errors.New("no healthy backends")
	}

	totalWeight := 0
	for _, b := range healthy {
		if b.GetWeight() > 0 {
			totalWeight += b.GetWeight()
		}
	}

	if totalWeight == 0 {
		selected := healthy[0]
		selected.IncrConn()
		return selected, nil
	}

	index := atomic.AddUint32(&w.current, 1) - 1
	sequence := index % uint32(totalWeight) // #nosec G115 -- totalWeight is sum of small positive weights

	current := uint32(0) // #nosec G115
	for _, b := range healthy {
		if b.GetWeight() <= 0 {
			continue
		}
		current += uint32(b.GetWeight()) // #nosec G115
		if sequence < current {
			b.IncrConn()
			return b, nil
		}
	}

	selected := healthy[0]
	selected.IncrConn()
	return selected, nil
}
