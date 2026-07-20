package balancer

import (
	"errors"
	"math/rand"
	"sync"
	"time"
)

// P2C implements the power-of-two-choices least-connections algorithm. Rather
// than scanning every backend for the global minimum (which causes herding under
// concurrency), it samples two distinct healthy backends at random and picks the
// one with fewer active connections. This gives near-optimal load spreading with
// O(1) work per pick and far less contention than a full least-connections scan.
type P2C struct {
	BaseBalancer
	mu  sync.Mutex
	rng *rand.Rand
}

func NewP2C() *P2C {
	return &P2C{rng: rand.New(rand.NewSource(time.Now().UnixNano()))} // #nosec G404 -- non-crypto load balancing selection
}

func (p *P2C) Next() (*Backend, error) {
	healthy := p.GetHealthy()
	if len(healthy) == 0 {
		return nil, errors.New("no healthy backends")
	}

	if len(healthy) == 1 {
		selected := healthy[0]
		selected.IncrConn()
		return selected, nil
	}

	p.mu.Lock()
	i := p.rng.Intn(len(healthy))
	j := p.rng.Intn(len(healthy) - 1)
	p.mu.Unlock()
	// Map j into the range excluding i so the two picks are always distinct.
	if j >= i {
		j++
	}

	a, b := healthy[i], healthy[j]
	selected := a
	if b.GetActiveConns() < a.GetActiveConns() {
		selected = b
	}

	// Reserve the slot at selection time; the caller releases via DecrConn.
	selected.IncrConn()
	return selected, nil
}
