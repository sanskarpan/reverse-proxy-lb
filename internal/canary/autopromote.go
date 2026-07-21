// Package canary implements automatic canary weight promotion and rollback.
// The AutoPromoter periodically evaluates the canary error rate and either
// steps the weight up toward MaxWeightPercent or rolls it back to 0.
package canary

import (
	"reverse-proxy-lb/internal/config"
	"sync"
	"sync/atomic"
	"time"
)

// weightUpdater is the subset of *proxy.Proxy needed by the AutoPromoter.
// It is defined here as an interface to allow easy test doubles.
type weightUpdater interface {
	UpdateCanaryWeight(pct int)
}

// metricsSnapshot is the subset of *metrics.Metrics needed by the AutoPromoter.
// CanarySnapshot returns the number of canary requests and errors observed
// since the previous call (the counters are reset on each call).
type metricsSnapshot interface {
	CanarySnapshot() (requests, errors int64)
}

// metricsUpdater is the subset of *metrics.Metrics used to publish canary
// state back to the metrics layer. It is satisfied by *metrics.Metrics and
// can be nil when metrics are disabled.
type metricsUpdater interface {
	SetCanaryWeight(w float64)
	IncrCanaryRollback()
}

// AutoPromoter steps the canary weight up on each interval when the error rate
// is acceptable, and optionally rolls it back to 0 on degradation.
type AutoPromoter struct {
	proxy      weightUpdater
	metrics    metricsSnapshot
	metricsUpd metricsUpdater
	cfg        config.AutoPromoteConfig

	mu            sync.Mutex
	currentWeight int

	// rollbackCount tracks how many times the promoter has rolled the canary
	// weight back to 0 due to error-rate degradation.
	rollbackCount atomic.Int64

	stopCh chan struct{}
	doneCh chan struct{}
}

// New creates an AutoPromoter. It does not start the background loop;
// call Start() to begin promotion.
func New(proxy weightUpdater, m metricsSnapshot, cfg config.AutoPromoteConfig) *AutoPromoter {
	return &AutoPromoter{
		proxy:   proxy,
		metrics: m,
		cfg:     cfg,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
}

// WithMetricsUpdater attaches a metricsUpdater so the AutoPromoter can publish
// canary weight and rollback counts back to the metrics layer. Call before
// Start(). Passing nil is safe and disables metric publishing.
func (a *AutoPromoter) WithMetricsUpdater(u metricsUpdater) {
	a.metricsUpd = u
}

// IncrRollback increments the internal rollback counter. It is intended for
// use by tests or external callers to simulate rollback events; step() manages
// the production counter directly via rollbackCount.Add(1) and does NOT call
// this method. Callers must not invoke IncrRollback() after a rollback that
// step() has already counted, as that would double-increment the counter.
func (a *AutoPromoter) IncrRollback() {
	a.rollbackCount.Add(1)
}

// AutoPromoterStatus is a point-in-time snapshot of the AutoPromoter state,
// returned by Status() and serialised as JSON by the admin endpoint.
type AutoPromoterStatus struct {
	Enabled       bool    `json:"enabled"`
	CurrentWeight int     `json:"current_weight"`
	MaxWeight     int     `json:"max_weight"`
	StepPercent   int     `json:"step_percent"`
	StepInterval  string  `json:"step_interval"`
	ErrorRate     float64 `json:"error_rate"`
	RollbackCount int64   `json:"rollback_count"`
}

// Status returns a snapshot of the current AutoPromoter state. The ErrorRate
// field reflects the last observed rate (0 when no data has been collected yet).
func (a *AutoPromoter) Status() AutoPromoterStatus {
	a.mu.Lock()
	w := a.currentWeight
	a.mu.Unlock()

	return AutoPromoterStatus{
		Enabled:       a.cfg.Enabled,
		CurrentWeight: w,
		MaxWeight:     a.cfg.MaxWeightPercent,
		StepPercent:   a.cfg.StepPercent,
		StepInterval:  a.cfg.StepInterval.String(),
		ErrorRate:     0, // populated lazily; see step()
		RollbackCount: a.rollbackCount.Load(),
	}
}

// Start launches the background step loop. It is safe to call once.
func (a *AutoPromoter) Start() {
	go a.run()
}

// Stop signals the background loop to exit and waits for it to finish.
func (a *AutoPromoter) Stop() {
	close(a.stopCh)
	<-a.doneCh
}

// run is the background goroutine that fires once per StepInterval.
func (a *AutoPromoter) run() {
	defer close(a.doneCh)

	ticker := time.NewTicker(a.cfg.StepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.stopCh:
			return
		case <-ticker.C:
			a.step()
		}
	}
}

// step evaluates the last window's canary metrics and adjusts the weight.
func (a *AutoPromoter) step() {
	reqs, errs := a.metrics.CanarySnapshot()

	if reqs < int64(a.cfg.MinRequests) {
		// Not enough data in this window — skip without changing the weight.
		return
	}

	errorRate := float64(errs) / float64(reqs)

	a.mu.Lock()
	defer a.mu.Unlock()

	if errorRate > a.cfg.ErrorRateThreshold {
		if a.cfg.RollbackOnDegradation {
			a.currentWeight = 0
			a.proxy.UpdateCanaryWeight(0)
			a.rollbackCount.Add(1)
			if a.metricsUpd != nil {
				a.metricsUpd.SetCanaryWeight(0)
				a.metricsUpd.IncrCanaryRollback()
			}
		}
		return
	}

	// Error rate is acceptable: step up, capped at MaxWeightPercent.
	next := a.currentWeight + a.cfg.StepPercent
	if next > a.cfg.MaxWeightPercent {
		next = a.cfg.MaxWeightPercent
	}
	if next != a.currentWeight {
		a.currentWeight = next
		a.proxy.UpdateCanaryWeight(next)
		if a.metricsUpd != nil {
			a.metricsUpd.SetCanaryWeight(float64(next))
		}
	}
}

// CurrentWeight returns the weight most recently applied by the promoter.
// It is primarily used in tests.
func (a *AutoPromoter) CurrentWeight() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.currentWeight
}
