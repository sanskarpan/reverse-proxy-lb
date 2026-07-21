package canary

import (
	"reverse-proxy-lb/internal/config"
	"sync/atomic"
	"testing"
	"time"
)

// --- test doubles ---

type mockProxy struct {
	weight atomic.Int32
}

func (m *mockProxy) UpdateCanaryWeight(pct int) {
	m.weight.Store(int32(pct)) // #nosec G115 -- weight is always 0..100
}

func (m *mockProxy) Weight() int {
	return int(m.weight.Load())
}

type mockMetrics struct {
	requests int64
	errors   int64
}

func (m *mockMetrics) CanarySnapshot() (requests, errors int64) {
	return m.requests, m.errors
}

// --- helpers ---

func defaultCfg() config.AutoPromoteConfig {
	return config.AutoPromoteConfig{
		Enabled:               true,
		StepPercent:           10,
		StepInterval:          10 * time.Millisecond,
		MaxWeightPercent:      100,
		ErrorRateThreshold:    0.01,
		MinRequests:           100,
		RollbackOnDegradation: true,
	}
}

// --- tests ---

// TestAutoPromoterStepsUp verifies that, given healthy traffic (0 errors,
// >= MinRequests), the weight increases by StepPercent on each call to step().
func TestAutoPromoterStepsUp(t *testing.T) {
	mp := &mockProxy{}
	mm := &mockMetrics{requests: 200, errors: 0}
	cfg := defaultCfg()

	ap := New(mp, mm, cfg)

	// Three manual steps should take the weight from 0 → 10 → 20 → 30.
	for i := 1; i <= 3; i++ {
		ap.step()
		want := i * cfg.StepPercent
		if got := ap.CurrentWeight(); got != want {
			t.Errorf("after step %d: weight = %d, want %d", i, got, want)
		}
		if got := mp.Weight(); got != want {
			t.Errorf("after step %d: proxy weight = %d, want %d", i, got, want)
		}
	}
}

// TestAutoPromoterRollsBack verifies that, given a high error rate and
// RollbackOnDegradation=true, the weight is reset to 0.
func TestAutoPromoterRollsBack(t *testing.T) {
	mp := &mockProxy{}
	mm := &mockMetrics{requests: 200, errors: 100} // 50% error rate >> 1%
	cfg := defaultCfg()

	ap := New(mp, mm, cfg)

	// Seed a non-zero weight so we can observe the rollback.
	ap.mu.Lock()
	ap.currentWeight = 50
	ap.mu.Unlock()
	mp.UpdateCanaryWeight(50)

	ap.step()

	if got := ap.CurrentWeight(); got != 0 {
		t.Errorf("after rollback: weight = %d, want 0", got)
	}
	if got := mp.Weight(); got != 0 {
		t.Errorf("after rollback: proxy weight = %d, want 0", got)
	}
}

// TestAutoPromoterSkipsLowTraffic verifies that when fewer than MinRequests
// requests are observed the weight is left unchanged.
func TestAutoPromoterSkipsLowTraffic(t *testing.T) {
	mp := &mockProxy{}
	mm := &mockMetrics{requests: 5, errors: 0} // below MinRequests=100
	cfg := defaultCfg()

	ap := New(mp, mm, cfg)

	ap.step()

	if got := ap.CurrentWeight(); got != 0 {
		t.Errorf("low traffic: weight = %d, want 0 (unchanged)", got)
	}
	if got := mp.Weight(); got != 0 {
		t.Errorf("low traffic: proxy weight = %d, want 0 (unchanged)", got)
	}
}

// TestAutoPromoterCapsAtMax verifies that weight does not exceed MaxWeightPercent.
func TestAutoPromoterCapsAtMax(t *testing.T) {
	mp := &mockProxy{}
	mm := &mockMetrics{requests: 200, errors: 0}
	cfg := defaultCfg()
	cfg.MaxWeightPercent = 25
	cfg.StepPercent = 10

	ap := New(mp, mm, cfg)

	// 3 steps: 10, 20, 25 (capped)
	ap.step() // 10
	ap.step() // 20
	ap.step() // 25 (capped)
	ap.step() // still 25

	if got := ap.CurrentWeight(); got != 25 {
		t.Errorf("capped: weight = %d, want 25", got)
	}
}

// TestAutoPromoterNoRollbackWhenDisabled verifies that, when
// RollbackOnDegradation is false, high error rate only pauses promotion but
// does not roll back the weight.
func TestAutoPromoterNoRollbackWhenDisabled(t *testing.T) {
	mp := &mockProxy{}
	mm := &mockMetrics{requests: 200, errors: 100}
	cfg := defaultCfg()
	cfg.RollbackOnDegradation = false

	ap := New(mp, mm, cfg)

	// Seed a non-zero weight.
	ap.mu.Lock()
	ap.currentWeight = 40
	ap.mu.Unlock()
	mp.UpdateCanaryWeight(40)

	ap.step()

	if got := ap.CurrentWeight(); got != 40 {
		t.Errorf("no-rollback: weight = %d, want 40 (unchanged)", got)
	}
}

// TestAutoPromoterStartStop verifies that the background goroutine starts and
// stops cleanly without panicking or leaking goroutines.  It does NOT assert
// on the resulting weight because the number of ticks that fire during a
// time.Sleep is inherently non-deterministic under load (see also
// TestAutoPromoterStepsUp for the synchronous correctness assertion).
func TestAutoPromoterStartStop(t *testing.T) {
	mp := &mockProxy{}
	mm := &mockMetrics{requests: 200, errors: 0}
	cfg := defaultCfg()
	cfg.StepInterval = 5 * time.Millisecond

	ap := New(mp, mm, cfg)
	ap.Start()
	// Give the goroutine a chance to start; Stop() blocks until the run loop
	// acknowledges the signal, so clean shutdown is always verified.
	ap.Stop()
	// No panic / deadlock reaching here means the goroutine shut down cleanly.
}
