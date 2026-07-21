package middleware

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// slowHandler returns an http.Handler that sleeps for d before writing 200 OK.
func slowHandler(d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(d)
		w.WriteHeader(http.StatusOK)
	})
}

// instantOKHandler returns an http.Handler that immediately writes 200 OK.
func instantOKHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// TestAdmissionAllowsUnderLimit verifies that requests below the concurrency
// ceiling all succeed.
func TestAdmissionAllowsUnderLimit(t *testing.T) {
	const maxRequests = 5
	const concurrent = 3

	h := Admission(maxRequests, 0, 0, nil)(slowHandler(20 * time.Millisecond))

	var wg sync.WaitGroup
	codes := make([]int, concurrent)
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			h.ServeHTTP(rec, req)
			codes[idx] = rec.Code
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("request %d: got status %d, want 200", i, code)
		}
	}
}

// TestAdmissionRejectsOverLimit verifies that when the concurrency ceiling is
// reached and no queue is configured, excess requests are immediately rejected
// with 503.
func TestAdmissionRejectsOverLimit(t *testing.T) {
	const maxRequests = 2
	const concurrent = 5

	// slow handler holds slots long enough for all goroutines to hit the gate.
	h := Admission(maxRequests, 0, 0, nil)(slowHandler(100 * time.Millisecond))

	var wg sync.WaitGroup
	codes := make([]int, concurrent)
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			h.ServeHTTP(rec, req)
			codes[idx] = rec.Code
		}(i)
	}
	wg.Wait()

	ok, rejected := 0, 0
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusServiceUnavailable:
			rejected++
		default:
			t.Errorf("unexpected status %d", code)
		}
	}

	if ok > maxRequests {
		t.Errorf("too many successes: got %d, want at most %d", ok, maxRequests)
	}
	if rejected < concurrent-maxRequests {
		t.Errorf("too few rejections: got %d, want at least %d", rejected, concurrent-maxRequests)
	}
}

// TestAdmissionQueuesAndDrains verifies that when a queue is configured, excess
// requests wait for a slot and eventually succeed.
func TestAdmissionQueuesAndDrains(t *testing.T) {
	const maxRequests = 1
	const maxQueue = 5
	const queueTimeout = 500 * time.Millisecond
	const concurrent = 3

	h := Admission(maxRequests, maxQueue, queueTimeout, nil)(slowHandler(50 * time.Millisecond))

	var wg sync.WaitGroup
	codes := make([]int, concurrent)
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			h.ServeHTTP(rec, req)
			codes[idx] = rec.Code
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("request %d: got status %d, want 200 (should have queued and drained)", i, code)
		}
	}
}

// TestAdmissionQueueTimeout verifies that requests waiting in the queue are
// rejected with 503 when the queue timeout expires before a slot becomes
// available.
func TestAdmissionQueueTimeout(t *testing.T) {
	const maxRequests = 1
	const maxQueue = 2
	const queueTimeout = 50 * time.Millisecond
	const concurrent = 3

	// Handler takes 200ms, well beyond the 50ms queue timeout.
	h := Admission(maxRequests, maxQueue, queueTimeout, nil)(slowHandler(200 * time.Millisecond))

	var wg sync.WaitGroup
	codes := make([]int, concurrent)
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			h.ServeHTTP(rec, req)
			codes[idx] = rec.Code
		}(i)
	}
	wg.Wait()

	ok, rejected := 0, 0
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusServiceUnavailable:
			rejected++
		default:
			t.Errorf("unexpected status %d", code)
		}
	}

	// Exactly 1 request gets the semaphore immediately; the other 2 queue and
	// time out before the 200ms handler finishes.
	if ok != 1 {
		t.Errorf("got %d successful requests, want exactly 1", ok)
	}
	if rejected < 2 {
		t.Errorf("got %d rejections, want at least 2 (queued requests should time out)", rejected)
	}
}

// TestAdmissionDisabledWhenZero verifies that a maxRequests <= 0 is a no-op
// and all requests pass through.
func TestAdmissionDisabledWhenZero(t *testing.T) {
	h := Admission(0, 0, 0, nil)(instantOKHandler())

	const n = 10
	for i := 0; i < n; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("request %d: got %d, want 200", i, rec.Code)
		}
	}
}
