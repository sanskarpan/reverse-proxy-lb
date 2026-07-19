package balancer

import (
	"errors"
	"sync"
)

type LeastConnections struct {
	BaseBalancer
	mu sync.Mutex
}

func NewLeastConnections() *LeastConnections {
	return &LeastConnections{}
}

func (l *LeastConnections) Next() (*Backend, error) {
	healthy := l.GetHealthy()
	if len(healthy) == 0 {
		return nil, errors.New("no healthy backends")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	var selected *Backend
	minConns := int(^uint(0) >> 1)

	for _, b := range healthy {
		conns := b.GetActiveConns()
		if conns < minConns {
			minConns = conns
			selected = b
		}
	}

	// Reserve the slot while still holding the lock so concurrent selections observe
	// the updated count. Without this, many goroutines would pick the same backend
	// (thundering herd) before any increment landed. The caller releases via DecrConn.
	if selected != nil {
		selected.IncrConn()
	}

	return selected, nil
}
