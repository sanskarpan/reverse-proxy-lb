package balancer

import (
	"errors"
	"sync/atomic"
)

type RoundRobin struct {
	BaseBalancer
	current uint32
}

func NewRoundRobin() *RoundRobin {
	return &RoundRobin{}
}

func (r *RoundRobin) Next() (*Backend, error) {
	healthy := r.GetHealthy()
	if len(healthy) == 0 {
		return nil, errors.New("no healthy backends")
	}

	index := atomic.AddUint32(&r.current, 1) - 1
	selected := healthy[index%uint32(len(healthy))]
	// Reserve the connection slot at selection time so callers see an accurate
	// in-flight count. The caller releases it via DecrConn when the request ends.
	selected.IncrConn()
	return selected, nil
}
