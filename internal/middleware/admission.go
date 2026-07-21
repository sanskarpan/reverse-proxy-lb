package middleware

import (
	"net/http"
	"sync/atomic"
	"time"

	"reverse-proxy-lb/internal/metrics"
)

// Admission returns middleware that limits concurrent in-flight requests to
// maxRequests. When maxRequests is reached:
//   - If maxQueue > 0 and queueTimeout > 0: queue the request up to maxQueue deep;
//     reject with 503 if no slot opens within queueTimeout.
//   - Otherwise: reject immediately with 503.
//
// A non-positive maxRequests disables the check.
func Admission(maxRequests, maxQueue int, queueTimeout time.Duration, m *metrics.Metrics) func(http.Handler) http.Handler {
	if maxRequests <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	sem := make(chan struct{}, maxRequests)
	var queued atomic.Int64
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Fast path: try to acquire a slot immediately.
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
				next.ServeHTTP(w, r)
				return
			default:
			}

			// Slow path: queue if configured.
			if maxQueue <= 0 || queueTimeout <= 0 {
				if m != nil {
					m.IncrRateLimited()
				}
				http.Error(w, "service unavailable: too many requests in flight", http.StatusServiceUnavailable)
				return
			}
			q := int(queued.Add(1))
			defer queued.Add(-1)
			if q > maxQueue {
				if m != nil {
					m.IncrRateLimited()
				}
				http.Error(w, "service unavailable: request queue full", http.StatusServiceUnavailable)
				return
			}
			timer := time.NewTimer(queueTimeout)
			defer timer.Stop()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
				next.ServeHTTP(w, r)
			case <-timer.C:
				if m != nil {
					m.IncrRateLimited()
				}
				http.Error(w, "service unavailable: timed out waiting for slot", http.StatusServiceUnavailable)
			case <-r.Context().Done():
				// Client disconnected while waiting; nothing to write.
			}
		})
	}
}
