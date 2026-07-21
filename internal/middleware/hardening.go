package middleware

import (
	"net/http"
	"runtime/debug"

	"reverse-proxy-lb/internal/logging"
)

// Recover wraps next with a deferred recover so that a panic in a downstream
// handler does not crash the whole server. On a panic it logs the recovered
// value together with a stack trace and, if the handler has not written a
// response yet, replies with 500 Internal Server Error.
//
// Recover deliberately does NOT wrap the ResponseWriter. Wrapping would break
// optional interfaces such as http.Flusher and http.Hijacker that streaming
// responses and WebSocket upgrades rely on. Because we cannot observe whether
// anything was written, we recover the panic, log it, and best-effort emit a
// 500. If the panic occurred after headers were already flushed, the
// WriteHeader call below is a harmless no-op (net/http logs a superfluous
// header warning at most) and no corrupt body is appended.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// http.ErrAbortHandler is the sanctioned way for a handler to
				// abort a request; propagate it so net/http can handle it.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				logging.Error("Recovered from panic in handler", map[string]interface{}{
					"panic":  rec,
					"method": r.Method,
					"url":    r.URL.String(),
					"stack":  string(debug.Stack()),
				})
				// Best-effort: if headers were already sent this is a no-op.
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// MaxBytes returns middleware that limits the size of request bodies to limit
// bytes. It rejects immediately with 413 when the Content-Length header
// advertises a body that exceeds the limit (fast path, no body read required)
// and wraps r.Body with http.MaxBytesReader for streaming bodies that don't
// declare Content-Length or that understate it.
//
// The body is only checked when r.Body is non-nil, so bodyless requests
// (GET, HEAD, ...) are unaffected. WebSocket upgrades are unaffected because
// they operate on the hijacked connection rather than r.Body. A non-positive
// limit disables the check.
func MaxBytes(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limit > 0 && r.Body != nil {
				// Fast-path: reject before reading if Content-Length already exceeds limit.
				if r.ContentLength > limit {
					http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
					return
				}
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}
