package middleware

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// --- RequestID -------------------------------------------------------------

func TestRequestIDGeneratesWhenAbsent(t *testing.T) {
	var (
		gotHeaderOnReq string
		gotCtx         string
	)
	h := RequestID("")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaderOnReq = r.Header.Get(DefaultRequestIDHeader)
		gotCtx = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	respID := rec.Header().Get(DefaultRequestIDHeader)
	if respID == "" {
		t.Fatal("expected a generated request id on the response header")
	}
	if len(respID) != 32 { // 16 random bytes -> 32 hex chars
		t.Fatalf("expected 32-char hex id, got %q (len %d)", respID, len(respID))
	}
	if gotHeaderOnReq != respID {
		t.Fatalf("request header id %q != response header id %q (must be forwarded upstream)", gotHeaderOnReq, respID)
	}
	if gotCtx != respID {
		t.Fatalf("context id %q != response header id %q", gotCtx, respID)
	}
}

func TestRequestIDPreservesProvided(t *testing.T) {
	const provided = "client-supplied-id-123"
	var gotCtx string
	h := RequestID("")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCtx = RequestIDFromContext(r.Context())
		if got := r.Header.Get(DefaultRequestIDHeader); got != provided {
			t.Errorf("request header mutated: got %q want %q", got, provided)
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(DefaultRequestIDHeader, provided)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get(DefaultRequestIDHeader); got != provided {
		t.Fatalf("response header id: got %q want %q", got, provided)
	}
	if gotCtx != provided {
		t.Fatalf("context id: got %q want %q", gotCtx, provided)
	}
}

func TestRequestIDCustomHeaderName(t *testing.T) {
	const hdr = "X-Trace-Id"
	h := RequestID(hdr)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get(hdr) == "" {
		t.Fatalf("expected id under custom header %q", hdr)
	}
	if rec.Header().Get(DefaultRequestIDHeader) != "" {
		t.Fatalf("did not expect an id under the default header")
	}
}

func TestRequestIDFromContextNil(t *testing.T) {
	if got := RequestIDFromContext(nil); got != "" {
		t.Fatalf("expected empty id for nil context, got %q", got)
	}
}

// --- AccessLog: capture -----------------------------------------------------

func TestAccessLogCapturesStatusAndBytes(t *testing.T) {
	body := "hello world payload"
	h := AccessLog(1)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, body)
	}))

	req := httptest.NewRequest(http.MethodGet, "/thing", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status not propagated: got %d", rec.Code)
	}
	if rec.Body.String() != body {
		t.Fatalf("body not propagated: got %q", rec.Body.String())
	}
}

// captureAndAssert exercises the wrapper directly to assert status/byte capture.
func TestCaptureResponseWriterRecords(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := &captureResponseWriter{ResponseWriter: rec, status: http.StatusOK}

	// Implicit 200 via Write only.
	n, err := cw.Write([]byte("abcde"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 || cw.bytes != 5 {
		t.Fatalf("byte count: write=%d captured=%d want 5", n, cw.bytes)
	}
	if cw.status != http.StatusOK {
		t.Fatalf("implicit status: got %d want 200", cw.status)
	}

	// A later WriteHeader after a write must not overwrite the captured status.
	cw.WriteHeader(http.StatusInternalServerError)
	if cw.status != http.StatusOK {
		t.Fatalf("status changed after first write: got %d want 200", cw.status)
	}
}

func TestCaptureResponseWriterExplicitStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := &captureResponseWriter{ResponseWriter: rec, status: http.StatusOK}
	cw.WriteHeader(http.StatusNotFound)
	_, _ = cw.Write([]byte("xy"))
	if cw.status != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", cw.status)
	}
	if cw.bytes != 2 {
		t.Fatalf("bytes: got %d want 2", cw.bytes)
	}
}

// --- AccessLog: sampling ----------------------------------------------------

// countingLogWriter records how many access-log lines the default logger emits.
// We temporarily redirect the default logger's output to it.
func TestAccessLogSampling(t *testing.T) {
	cases := []struct {
		name    string
		sampleN int
		reqs    int
		want    int
	}{
		{"sample_all_zero", 0, 5, 5},
		{"sample_all_one", 1, 5, 5},
		{"sample_every_third", 3, 9, 3},
		{"sample_every_third_partial", 3, 7, 3}, // logs reqs 1,4,7
		{"sample_larger_than_reqs", 10, 4, 1},   // only first logged
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := redirectDefaultLogger(t)

			h := AccessLog(tc.sampleN)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			for i := 0; i < tc.reqs; i++ {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			}

			// Restore fd 1 BEFORE asserting so t.Fatalf output is visible.
			restore()

			got := countAccessLines(t)
			if got != tc.want {
				t.Fatalf("sampleN=%d reqs=%d: logged %d lines, want %d", tc.sampleN, tc.reqs, got, tc.want)
			}
		})
	}
}

// logCapture is the buffer the default logger is redirected to during a test.
var (
	logCaptureMu sync.Mutex
	logCapture   *strings.Builder
)

// redirectDefaultLogger captures everything written to file descriptor 1 (the
// underlying fd that the default logger's cached *os.File wraps). The logging
// package stores a reference to the original os.Stdout at init time and exposes
// no output setter, so swapping the os.Stdout variable would not redirect it;
// instead we dup a pipe over fd 1 and restore it afterwards.
func redirectDefaultLogger(t *testing.T) func() {
	t.Helper()
	logCaptureMu.Lock()
	logCapture = &strings.Builder{}
	logCaptureMu.Unlock()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	origFd, err := syscall.Dup(1)
	if err != nil {
		t.Fatalf("dup fd 1: %v", err)
	}
	if err := syscall.Dup2(int(w.Fd()), 1); err != nil {
		t.Fatalf("dup2 onto fd 1: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				logCaptureMu.Lock()
				logCapture.Write(buf[:n])
				logCaptureMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	return func() {
		// Restore fd 1, then close the pipe write end so the reader drains.
		_ = syscall.Dup2(origFd, 1)
		_ = syscall.Close(origFd)
		_ = w.Close()
		<-done
		_ = r.Close()
	}
}

func countAccessLines(t *testing.T) int {
	t.Helper()
	logCaptureMu.Lock()
	defer logCaptureMu.Unlock()
	count := 0
	for _, line := range strings.Split(logCapture.String(), "\n") {
		if strings.Contains(line, `"access"`) {
			count++
		}
	}
	return count
}

// --- Interface preservation -------------------------------------------------

// hijackableRecorder is an httptest.ResponseRecorder that also implements
// http.Hijacker so we can assert the wrapper delegates Hijack.
type hijackableRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	c1, _ := net.Pipe()
	return c1, bufio.NewReadWriter(bufio.NewReader(c1), bufio.NewWriter(c1)), nil
}

func TestCaptureWriterImplementsOptionalInterfaces(t *testing.T) {
	var w http.ResponseWriter = &captureResponseWriter{ResponseWriter: httptest.NewRecorder()}
	if _, ok := w.(http.Flusher); !ok {
		t.Error("captureResponseWriter must implement http.Flusher")
	}
	if _, ok := w.(http.Hijacker); !ok {
		t.Error("captureResponseWriter must implement http.Hijacker")
	}
}

func TestCaptureWriterHijackDelegates(t *testing.T) {
	hr := &hijackableRecorder{ResponseRecorder: httptest.NewRecorder()}
	cw := &captureResponseWriter{ResponseWriter: hr}

	conn, rw, err := cw.Hijack()
	if err != nil {
		t.Fatalf("hijack: %v", err)
	}
	if !hr.hijacked {
		t.Fatal("hijack was not delegated to the underlying writer")
	}
	if conn == nil || rw == nil {
		t.Fatal("hijack returned nil conn/readwriter")
	}
	_ = conn.Close()
}

func TestCaptureWriterHijackUnsupported(t *testing.T) {
	// httptest.ResponseRecorder does not implement http.Hijacker.
	cw := &captureResponseWriter{ResponseWriter: httptest.NewRecorder()}
	if _, _, err := cw.Hijack(); err == nil {
		t.Fatal("expected an error when underlying writer does not support hijacking")
	}
}

// flushRecorder tracks Flush delegation.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (f *flushRecorder) Flush() { f.flushed = true }

func TestCaptureWriterFlushDelegates(t *testing.T) {
	fr := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	cw := &captureResponseWriter{ResponseWriter: fr}
	cw.Flush()
	if !fr.flushed {
		t.Fatal("flush was not delegated to the underlying writer")
	}
}
