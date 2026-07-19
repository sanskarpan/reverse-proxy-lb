package middleware

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"reverse-proxy-lb/internal/logging"
)

// DefaultRequestIDHeader is the header used by RequestID when no header name is
// supplied. It is also the conventional header proxied clients and upstreams use
// to correlate a single request across hops.
const DefaultRequestIDHeader = "X-Request-ID"

// contextKey is an unexported type for context keys defined in this package so
// that they never collide with keys from other packages.
type contextKey int

const requestIDKey contextKey = iota

// RequestIDFromContext returns the request id previously stored on ctx by the
// RequestID middleware, or "" if none is present. Handlers and downstream
// middleware (such as AccessLog) use it to correlate log lines with a request.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// contextWithRequestID returns a copy of ctx carrying id under the package's
// unexported request-id key.
func contextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// newRequestID returns a random 128-bit request id encoded as lowercase hex. It
// falls back to a timestamp-derived value only if the system CSPRNG fails, which
// should never happen in practice.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Extremely unlikely; derive a best-effort unique-ish value so we never
		// emit an empty id.
		return "ffffffff" + hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

// RequestID returns middleware that ensures every request carries a correlation
// id under headerName (defaulting to DefaultRequestIDHeader when empty).
//
// If the incoming request lacks the header, a random hex id is generated. The id
// is written back onto the request header (so it is forwarded to the upstream),
// set on the response header (so the client can see it), and stashed in the
// request context where RequestIDFromContext can retrieve it (used by AccessLog
// and downstream handlers).
func RequestID(headerName string) func(http.Handler) http.Handler {
	if headerName == "" {
		headerName = DefaultRequestIDHeader
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(headerName)
			if id == "" {
				id = newRequestID()
				// Set it on the request header so the id is forwarded upstream.
				r.Header.Set(headerName, id)
			}
			// Always echo the (possibly client-provided) id on the response and
			// make it available to downstream handlers via context.
			w.Header().Set(headerName, id)
			r = r.WithContext(contextWithRequestID(r.Context(), id))
			next.ServeHTTP(w, r)
		})
	}
}

// AccessLog returns middleware that emits exactly one structured access-log line
// per (sampled) request after the downstream handler returns. The line carries
// method, path, status, duration_ms, bytes, client_ip and request_id.
//
// Sampling: one out of every sampleN requests is logged; sampleN <= 1 logs every
// request. Sampling uses a lock-free atomic counter so it is safe under
// concurrent load; the wrapper still captures status/bytes for every request but
// only emits a log line for sampled ones.
//
// The wrapper forwards Flush and Hijack to the underlying ResponseWriter so
// streaming responses (SSE) and WebSocket upgrades continue to work.
func AccessLog(sampleN int) func(http.Handler) http.Handler {
	var counter uint64
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			cw := &captureResponseWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(cw, r)

			if sampleN > 1 {
				// Log the 1st request and every sampleN-th thereafter.
				n := atomic.AddUint64(&counter, 1)
				if (n-1)%uint64(sampleN) != 0 {
					return
				}
			}

			logging.Info("access", map[string]interface{}{
				"method":      r.Method,
				"path":        r.URL.Path,
				"status":      cw.status,
				"duration_ms": time.Since(start).Milliseconds(),
				"bytes":       cw.bytes,
				"client_ip":   r.RemoteAddr,
				"request_id":  RequestIDFromContext(r.Context()),
			})
		})
	}
}

// captureResponseWriter wraps an http.ResponseWriter to record the final status
// code and the number of body bytes written, while forwarding the optional
// Flusher and Hijacker interfaces so streaming and WebSocket upgrades keep
// working.
type captureResponseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (c *captureResponseWriter) WriteHeader(code int) {
	if !c.wroteHeader {
		c.status = code
		c.wroteHeader = true
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureResponseWriter) Write(b []byte) (int, error) {
	if !c.wroteHeader {
		// Mirror net/http: an implicit 200 is the effective status.
		c.wroteHeader = true
	}
	n, err := c.ResponseWriter.Write(b)
	c.bytes += n
	return n, err
}

// Flush forwards to the underlying writer so SSE/streaming responses are not
// buffered by this wrapper.
func (c *captureResponseWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying writer so WebSocket upgrades keep working
// with this wrapper in the chain.
func (c *captureResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := c.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
}

// Compile-time assertions that the capture writer preserves the optional
// interfaces streaming and WebSocket handlers rely on.
var (
	_ http.Flusher  = (*captureResponseWriter)(nil)
	_ http.Hijacker = (*captureResponseWriter)(nil)
)
