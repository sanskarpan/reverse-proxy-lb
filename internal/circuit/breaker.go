package circuit

import (
	"errors"
	"reverse-proxy-lb/internal/balancer"
	"sync"
	"time"
)

type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Mode selects the tripping strategy.
type Mode int

const (
	// ModeConsecutive trips after failureThreshold consecutive failures. This is
	// the original, default behavior.
	ModeConsecutive Mode = iota
	// ModeRolling trips when, over a sliding time window, the total number of
	// recorded requests reaches minRequests AND the failure rate exceeds
	// errorRateThreshold.
	ModeRolling
)

// clock abstracts time so rolling-window behavior can be tested deterministically.
type clock func() time.Time

// StateChangeHook is invoked whenever a backend's circuit transitions between
// states. It is called outside the breaker's lock so it may safely log or emit
// metrics without risking deadlock or lock contention.
type StateChangeHook func(backend *balancer.Backend, from, to State)

// Config carries the parameters needed to construct a CircuitBreaker. It mirrors
// the relevant fields of config.CircuitBreakerConfig without importing it, so the
// circuit package stays free of a config dependency in its constructor surface.
type Config struct {
	Mode               Mode
	FailureThreshold   int
	SuccessThreshold   int
	Timeout            time.Duration
	RollingWindow      time.Duration
	ErrorRateThreshold float64
	MinRequests        int
}

// rollingBuckets defines how finely the rolling window is subdivided. More
// buckets means smoother aging of old events at the cost of a little memory.
const rollingBuckets = 10

type CircuitBreaker struct {
	mu               sync.Mutex
	mode             Mode
	failureThreshold int
	successThreshold int
	timeout          time.Duration

	// rolling-mode parameters
	rollingWindow      time.Duration
	errorRateThreshold float64
	minRequests        int

	now           clock
	onStateChange StateChangeHook

	backendStates map[*balancer.Backend]*backendState
}

type bucket struct {
	start     time.Time
	successes int
	failures  int
}

type backendState struct {
	state            State
	failures         int
	successes        int
	halfOpenInFlight int
	lastFailure      time.Time

	// rolling-mode sliding window, kept as a ring of fixed-duration buckets.
	buckets []bucket
}

// NewCircuitBreaker preserves the original constructor and consecutive-failure
// behavior. Existing callers and tests keep working unchanged.
func NewCircuitBreaker(failureThreshold, successThreshold int, timeout time.Duration) *CircuitBreaker {
	return NewCircuitBreakerWithConfig(Config{
		Mode:             ModeConsecutive,
		FailureThreshold: failureThreshold,
		SuccessThreshold: successThreshold,
		Timeout:          timeout,
	})
}

// NewCircuitBreakerWithConfig builds a breaker for either mode. In rolling mode
// the window/rate/min-requests fields are honored; in consecutive mode they are
// ignored. Missing or invalid values fall back to safe defaults.
func NewCircuitBreakerWithConfig(cfg Config) *CircuitBreaker {
	if cfg.FailureThreshold < 1 {
		cfg.FailureThreshold = 1
	}
	if cfg.SuccessThreshold < 1 {
		cfg.SuccessThreshold = 1
	}
	if cfg.RollingWindow <= 0 {
		cfg.RollingWindow = 10 * time.Second
	}
	if cfg.ErrorRateThreshold <= 0 || cfg.ErrorRateThreshold > 1 {
		cfg.ErrorRateThreshold = 0.5
	}
	if cfg.MinRequests <= 0 {
		cfg.MinRequests = 20
	}
	return &CircuitBreaker{
		mode:               cfg.Mode,
		failureThreshold:   cfg.FailureThreshold,
		successThreshold:   cfg.SuccessThreshold,
		timeout:            cfg.Timeout,
		rollingWindow:      cfg.RollingWindow,
		errorRateThreshold: cfg.ErrorRateThreshold,
		minRequests:        cfg.MinRequests,
		now:                time.Now,
		backendStates:      make(map[*balancer.Backend]*backendState),
	}
}

// SetOnStateChange registers a hook invoked on every state transition. It is safe
// to call before the breaker is in use; setting it to nil disables notifications.
func (c *CircuitBreaker) SetOnStateChange(hook StateChangeHook) {
	c.mu.Lock()
	c.onStateChange = hook
	c.mu.Unlock()
}

// setClock overrides the time source; used by tests for deterministic windows.
func (c *CircuitBreaker) setClock(fn clock) {
	c.mu.Lock()
	c.now = fn
	c.mu.Unlock()
}

// maxProbes bounds the number of concurrent trial requests allowed while half-open,
// preventing a thundering herd against a still-recovering backend.
func (c *CircuitBreaker) maxProbes() int {
	if c.successThreshold < 1 {
		return 1
	}
	return c.successThreshold
}

func (c *CircuitBreaker) stateFor(backend *balancer.Backend) *backendState {
	st, ok := c.backendStates[backend]
	if !ok {
		st = &backendState{state: StateClosed}
		c.backendStates[backend] = st
	}
	return st
}

// transition mutates the state and, if it actually changed, records the pending
// notification onto notify (a from,to pair) so the caller can fire the hook once
// the lock is released. Passing a nil notify skips notification bookkeeping.
func (c *CircuitBreaker) transition(state *backendState, to State, notify *[2]State) {
	from := state.state
	if from == to {
		return
	}
	state.state = to
	if notify != nil {
		notify[0] = from
		notify[1] = to
	}
}

// Allow decides, under a single lock, whether a request may proceed to the backend.
// The read, decision, and any state transition happen atomically to avoid the TOCTOU
// race present in the original implementation.
func (c *CircuitBreaker) Allow(backend *balancer.Backend) error {
	c.mu.Lock()

	state := c.stateFor(backend)

	var changed bool
	var pending [2]State
	var err error

	switch state.state {
	case StateOpen:
		if c.now().Sub(state.lastFailure) >= c.timeout {
			c.transition(state, StateHalfOpen, &pending)
			changed = true
			state.successes = 0
			state.halfOpenInFlight = 1
			err = nil
		} else {
			err = errors.New("circuit breaker is open")
		}
	case StateHalfOpen:
		if state.halfOpenInFlight >= c.maxProbes() {
			err = errors.New("circuit breaker is half-open (probe limit reached)")
		} else {
			state.halfOpenInFlight++
			err = nil
		}
	default: // StateClosed
		err = nil
	}

	hook := c.onStateChange
	c.mu.Unlock()

	if changed && hook != nil {
		hook(backend, pending[0], pending[1])
	}
	return err
}

func (c *CircuitBreaker) RecordSuccess(backend *balancer.Backend) {
	c.mu.Lock()

	state := c.stateFor(backend)

	var changed bool
	var pending [2]State

	switch state.state {
	case StateHalfOpen:
		if state.halfOpenInFlight > 0 {
			state.halfOpenInFlight--
		}
		state.successes++
		if state.successes >= c.successThreshold {
			c.transition(state, StateClosed, &pending)
			changed = true
			state.failures = 0
			state.successes = 0
			state.halfOpenInFlight = 0
			c.resetWindow(state)
			backend.SetHealthy(true)
		}
	case StateClosed:
		state.failures = 0
		state.successes = 0
		if c.mode == ModeRolling {
			c.recordWindow(state, true)
		}
	}

	hook := c.onStateChange
	c.mu.Unlock()

	if changed && hook != nil {
		hook(backend, pending[0], pending[1])
	}
}

func (c *CircuitBreaker) RecordFailure(backend *balancer.Backend) {
	c.mu.Lock()

	state := c.stateFor(backend)

	state.failures++
	state.lastFailure = c.now()

	var changed bool
	var pending [2]State

	switch {
	case state.state == StateHalfOpen:
		// A failure during recovery re-opens the circuit immediately.
		c.transition(state, StateOpen, &pending)
		changed = true
		state.successes = 0
		state.halfOpenInFlight = 0
		backend.SetHealthy(false)

	case c.mode == ModeRolling:
		c.recordWindow(state, false)
		if state.state == StateClosed && c.rollingShouldTrip(state) {
			c.transition(state, StateOpen, &pending)
			changed = true
			backend.SetHealthy(false)
		}

	default: // consecutive mode, closed/open
		if state.state == StateClosed && state.failures >= c.failureThreshold {
			c.transition(state, StateOpen, &pending)
			changed = true
			backend.SetHealthy(false)
		}
	}

	hook := c.onStateChange
	c.mu.Unlock()

	if changed && hook != nil {
		hook(backend, pending[0], pending[1])
	}
}

// recordWindow adds an event to the current bucket, allocating the ring on first
// use and rolling the active bucket forward as time advances.
func (c *CircuitBreaker) recordWindow(state *backendState, success bool) {
	now := c.now()
	if state.buckets == nil {
		state.buckets = make([]bucket, rollingBuckets)
	}
	bucketDur := c.rollingWindow / time.Duration(rollingBuckets)
	if bucketDur <= 0 {
		bucketDur = time.Nanosecond
	}
	idx := int(now.UnixNano()/int64(bucketDur)) % rollingBuckets
	if idx < 0 {
		idx += rollingBuckets
	}
	b := &state.buckets[idx]
	// If this bucket belongs to a prior window rotation, reset it before use.
	if now.Sub(b.start) >= c.rollingWindow || b.start.IsZero() {
		b.start = now
		b.successes = 0
		b.failures = 0
	}
	if success {
		b.successes++
	} else {
		b.failures++
	}
}

// rollingShouldTrip reports whether the aggregate over the live window meets the
// minimum-request and error-rate conditions.
func (c *CircuitBreaker) rollingShouldTrip(state *backendState) bool {
	total, failures := c.windowTotals(state)
	if total < c.minRequests {
		return false
	}
	rate := float64(failures) / float64(total)
	return rate > c.errorRateThreshold
}

// windowTotals sums successes+failures across buckets that are still within the
// rolling window relative to now, ignoring aged-out buckets.
func (c *CircuitBreaker) windowTotals(state *backendState) (total, failures int) {
	now := c.now()
	for i := range state.buckets {
		b := &state.buckets[i]
		if b.start.IsZero() {
			continue
		}
		if now.Sub(b.start) >= c.rollingWindow {
			continue
		}
		total += b.successes + b.failures
		failures += b.failures
	}
	return total, failures
}

func (c *CircuitBreaker) resetWindow(state *backendState) {
	state.buckets = nil
}

func (c *CircuitBreaker) GetState(backend *balancer.Backend) State {
	c.mu.Lock()
	defer c.mu.Unlock()

	if state, ok := c.backendStates[backend]; ok {
		return state.state
	}
	return StateClosed
}

func (c *CircuitBreaker) Reset(backend *balancer.Backend) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.backendStates, backend)
}
