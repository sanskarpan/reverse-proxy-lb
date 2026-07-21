// Package canary implements automatic canary weight promotion and rollback.
// The AutoPromoter periodically evaluates the canary error rate and either
// steps the weight up toward MaxWeightPercent or rolls it back to 0.
package canary

import (
	"reverse-proxy-lb/internal/config"
	"sync"
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

// AutoPromoter steps the canary weight up on each interval when the error rate
// is acceptable, and optionally rolls it back to 0 on degradation.
type AutoPromoter struct {
	proxy   weightUpdater
	metrics metricsSnapshot
	cfg     config.AutoPromoteConfig

	mu            sync.Mutex
	currentWeight int

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
	}
}

// CurrentWeight returns the weight most recently applied by the promoter.
// It is primarily used in tests.
func (a *AutoPromoter) CurrentWeight() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.currentWeight
}
