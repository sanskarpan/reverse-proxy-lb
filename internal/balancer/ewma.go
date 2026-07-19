package balancer

import (
	"errors"
	"math"
	"sync"
	"time"
)

// ewmaDecayTau is the time constant for decaying a backend's latency EWMA toward
// its most recent sample. Larger tau = slower adaptation. 10s is a reasonable
// default for request latencies in the millisecond-to-second range.
const ewmaDecayTau = 10 * time.Second

type ewmaState struct {
	value      float64   // current EWMA of latency, in nanoseconds
	lastSample time.Time // when value was last updated
}

// EWMA implements peak-EWMA least-latency selection (the algorithm popularized
// by Twitter Finagle and used by Linkerd). Each backend tracks an
// exponentially-weighted moving average of its observed latency, decayed by the
// time since the last sample. The selection score is that latency EWMA
// multiplied by (in-flight + 1), so a fast-but-busy backend is penalized and a
// slow backend is avoided. The lowest-scoring healthy backend wins.
type EWMA struct {
	BaseBalancer
	mu    sync.Mutex
	state map[*Backend]*ewmaState
	clock func() time.Time // injectable for tests
}

func NewEWMA() *EWMA {
	return &EWMA{
		state: make(map[*Backend]*ewmaState),
		clock: time.Now,
	}
}

// ObserveLatency updates the backend's latency EWMA with a new sample, decaying
// the previous value by the elapsed time since the last sample. It satisfies the
// LatencyObserver capability.
func (e *EWMA) ObserveLatency(b *Backend, d time.Duration) {
	if b == nil {
		return
	}
	now := e.clock()
	e.mu.Lock()
	defer e.mu.Unlock()

	st := e.state[b]
	if st == nil {
		st = &ewmaState{value: float64(d), lastSample: now}
		e.state[b] = st
		return
	}

	elapsed := now.Sub(st.lastSample)
	if elapsed < 0 {
		elapsed = 0
	}
	// Decay weight: w = e^(-elapsed/tau). The older the last sample, the more the
	// new observation dominates.
	w := math.Exp(-float64(elapsed) / float64(ewmaDecayTau))
	st.value = st.value*w + float64(d)*(1-w)
	st.lastSample = now
}

// scoreLocked returns the selection score for b. Backends with no samples yet
// get a score of 0 so they are eagerly probed. Caller holds mu.
func (e *EWMA) scoreLocked(b *Backend) float64 {
	st := e.state[b]
	inflight := float64(b.GetActiveConns()) + 1
	if st == nil {
		return 0
	}
	return st.value * inflight
}

func (e *EWMA) Next() (*Backend, error) {
	healthy := e.GetHealthy()
	if len(healthy) == 0 {
		return nil, errors.New("no healthy backends")
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	var selected *Backend
	var bestScore float64
	for _, b := range healthy {
		score := e.scoreLocked(b)
		if selected == nil || score < bestScore {
			bestScore = score
			selected = b
		}
	}

	// Reserve at selection time so the in-flight penalty reflects this pick for
	// concurrent selections. The caller releases via DecrConn.
	if selected != nil {
		selected.IncrConn()
	}
	return selected, nil
}
