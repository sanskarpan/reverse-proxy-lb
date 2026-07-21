package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecoverReturns500OnPanic(t *testing.T) {
	panicking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	// The test must not crash even though the handler panics.
	Recover(panicking).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d", http.StatusInternalServerError, rec.Code)
	}
}

func TestRecoverPassesThroughNormalResponse(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "hello")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	Recover(ok).ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("expected status %d, got %d", http.StatusTeapot, rec.Code)
	}
	if got := rec.Body.String(); got != "hello" {
		t.Fatalf("expected body %q, got %q", "hello", got)
	}
}

func TestRecoverPropagatesErrAbortHandler(t *testing.T) {
	aborting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Fatalf("expected ErrAbortHandler to propagate, got %v", r)
		}
	}()

	Recover(aborting).ServeHTTP(rec, req)
	t.Fatal("expected panic to propagate")
}

// readAllHandler drains r.Body and records the read error (if any) via the
// provided callback so the test can assert on it.
func readAllHandler(onResult func(n int, err error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		onResult(len(data), err)
		if err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestMaxBytesAllowsUnderLimit(t *testing.T) {
	const limit = 16
	body := strings.Repeat("a", limit) // exactly at the limit is allowed

	var readErr error
	var readN int
	h := MaxBytes(limit)(readAllHandler(func(n int, err error) {
		readN, readErr = n, err
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	h.ServeHTTP(rec, req)

	if readErr != nil {
		t.Fatalf("expected no error under limit, got %v", readErr)
	}
	if readN != limit {
		t.Fatalf("expected to read %d bytes, got %d", limit, readN)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
}

func TestMaxBytesRejectsOverLimit(t *testing.T) {
	const limit = 16
	body := strings.Repeat("a", limit+64) // well over the limit

	// When Content-Length is known and exceeds the limit, the fast-path
	// short-circuits before calling the inner handler and returns 413 directly.
	// (httptest.NewRequest derives ContentLength from *strings.Reader.)
	handlerCalled := false
	h := MaxBytes(limit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	h.ServeHTTP(rec, req)

	if handlerCalled {
		t.Fatal("expected handler NOT to be called when Content-Length exceeds limit")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status %d, got %d", http.StatusRequestEntityTooLarge, rec.Code)
	}
}

// TestMaxBytesRejectsStreamOverLimit covers the streaming case where
// Content-Length is -1 (unknown) but the actual body exceeds the limit.
// In this case the inner handler IS called but gets an error on Read.
func TestMaxBytesRejectsStreamOverLimit(t *testing.T) {
	const limit = 16

	var readErr error
	h := MaxBytes(limit)(readAllHandler(func(n int, err error) {
		readErr = err
	}))

	rec := httptest.NewRecorder()
	// Use an io.NopCloser-wrapped reader so Content-Length is -1 (unknown).
	body := io.NopCloser(strings.NewReader(strings.Repeat("a", limit+64)))
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.ContentLength = -1 // force unknown length
	h.ServeHTTP(rec, req)

	if readErr == nil {
		t.Fatal("expected an error reading a streaming body that exceeds the limit, got nil")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status %d, got %d", http.StatusRequestEntityTooLarge, rec.Code)
	}
}

func TestMaxBytesNilBodyIsSafe(t *testing.T) {
	called := false
	h := MaxBytes(16)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Body = nil // simulate a request with no body

	h.ServeHTTP(rec, req)

	if !called {
		t.Fatal("expected next handler to be called for nil body")
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d", http.StatusNoContent, rec.Code)
	}
}
