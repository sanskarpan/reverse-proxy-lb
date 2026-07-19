package middleware

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"reverse-proxy-lb/internal/config"
)

// transformRNG is the package-level random source used by FaultInjection and
// Mirror to decide which fraction of requests to affect. It is seeded from the
// clock by default; tests may swap it out via SetTransformRand for determinism.
var (
	transformRNGMu sync.Mutex
	transformRNG   = rand.New(rand.NewSource(time.Now().UnixNano()))
)

// SetTransformRand replaces the package rng used by FaultInjection and Mirror so
// tests can make sampling decisions deterministic. Passing nil restores a
// clock-seeded source. It returns the previous source so callers can restore it.
func SetTransformRand(r *rand.Rand) *rand.Rand {
	transformRNGMu.Lock()
	defer transformRNGMu.Unlock()
	prev := transformRNG
	if r == nil {
		r = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	transformRNG = r
	return prev
}

// sampleHit reports whether an event with the given percent chance (0..100)
// fires for this call, using the package rng. percent <= 0 never fires;
// percent >= 100 always fires.
func sampleHit(percent int) bool {
	if percent <= 0 {
		return false
	}
	if percent >= 100 {
		return true
	}
	transformRNGMu.Lock()
	defer transformRNGMu.Unlock()
	return transformRNG.Intn(100) < percent
}

// Rewrite returns middleware that mutates the request before proxying and the
// response headers before they are written, per cfg. It is a no-op passthrough
// when cfg leaves every knob at its zero value (no header edits, no path strip,
// no HTTPS redirect), preserving current behavior.
//
// Request side (applied before next.ServeHTTP):
//   - RequestHeadersRemove entries are deleted from r.Header.
//   - RequestHeadersSet entries are set on r.Header (overwriting).
//   - StripPathPrefix, when a prefix of r.URL.Path, is trimmed from the path
//     (and RawPath) so upstreams see the shortened path. The path always keeps a
//     leading "/".
//   - HTTPSRedirect: when the request is not TLS (r.TLS == nil and the
//     X-Forwarded-Proto header is not "https"), the client is 308-redirected to
//     the https:// equivalent of the same host+URI and next is not called.
//
// Response side: the ResponseWriter is wrapped so that at WriteHeader time
// ResponseHeadersRemove are deleted and ResponseHeadersSet are applied to the
// outgoing header map. Flush/Hijack are forwarded so streaming and upgrades keep
// working.
func Rewrite(cfg config.RewriteConfig) func(http.Handler) http.Handler {
	stripPrefix := cfg.StripPathPrefix
	active := cfg.HTTPSRedirect ||
		stripPrefix != "" ||
		len(cfg.RequestHeadersSet) > 0 ||
		len(cfg.RequestHeadersRemove) > 0 ||
		len(cfg.ResponseHeadersSet) > 0 ||
		len(cfg.ResponseHeadersRemove) > 0
	rewriteResp := len(cfg.ResponseHeadersSet) > 0 || len(cfg.ResponseHeadersRemove) > 0

	return func(next http.Handler) http.Handler {
		if !active {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg.HTTPSRedirect && !requestIsHTTPS(r) {
				redirectToHTTPS(w, r)
				return
			}

			for _, name := range cfg.RequestHeadersRemove {
				r.Header.Del(name)
			}
			for name, val := range cfg.RequestHeadersSet {
				r.Header.Set(name, val)
			}

			if stripPrefix != "" {
				stripRequestPath(r, stripPrefix)
			}

			if rewriteResp {
				rw := &rewriteResponseWriter{
					ResponseWriter: w,
					setHeaders:     cfg.ResponseHeadersSet,
					removeHeaders:  cfg.ResponseHeadersRemove,
				}
				next.ServeHTTP(rw, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requestIsHTTPS reports whether the request reached us over TLS, either
// directly (r.TLS set) or via a trusted terminator that set
// X-Forwarded-Proto: https.
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// redirectToHTTPS issues a 308 to the https:// equivalent of the request,
// preserving host, path, and query. A 308 keeps the method and body so
// non-idempotent requests are not silently downgraded to GET.
func redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	target := "https://" + host + r.URL.RequestURI()
	http.Redirect(w, r, target, http.StatusPermanentRedirect)
}

// stripRequestPath trims prefix from the request path (and RawPath when set),
// always leaving a leading slash so upstreams receive a valid absolute path.
func stripRequestPath(r *http.Request, prefix string) {
	p := r.URL.Path
	if !strings.HasPrefix(p, prefix) {
		return
	}
	trimmed := strings.TrimPrefix(p, prefix)
	if trimmed == "" || trimmed[0] != '/' {
		trimmed = "/" + trimmed
	}
	r.URL.Path = trimmed

	if r.URL.RawPath != "" && strings.HasPrefix(r.URL.RawPath, prefix) {
		rawTrimmed := strings.TrimPrefix(r.URL.RawPath, prefix)
		if rawTrimmed == "" || rawTrimmed[0] != '/' {
			rawTrimmed = "/" + rawTrimmed
		}
		r.URL.RawPath = rawTrimmed
	}
}

// rewriteResponseWriter applies response-header set/remove edits exactly once,
// when the status line is written (or on first Write), then forwards to the
// underlying writer.
type rewriteResponseWriter struct {
	http.ResponseWriter
	setHeaders    map[string]string
	removeHeaders []string
	wroteHeader   bool
}

func (rw *rewriteResponseWriter) applyHeaders() {
	h := rw.ResponseWriter.Header()
	for _, name := range rw.removeHeaders {
		h.Del(name)
	}
	for name, val := range rw.setHeaders {
		h.Set(name, val)
	}
}

func (rw *rewriteResponseWriter) WriteHeader(code int) {
	if !rw.wroteHeader {
		rw.wroteHeader = true
		rw.applyHeaders()
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *rewriteResponseWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.wroteHeader = true
		rw.applyHeaders()
	}
	return rw.ResponseWriter.Write(b)
}

func (rw *rewriteResponseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *rewriteResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := rw.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
}

// FaultInjection returns middleware that injects synthetic latency and/or
// aborts for a configured fraction of requests, for resilience testing. It is a
// no-op passthrough when cfg.Enabled is false, preserving current behavior.
//
// For DelayPercent of requests it sleeps cfg.Delay before continuing, aborting
// the sleep early if the request context is cancelled. For AbortPercent of
// requests it short-circuits with cfg.AbortStatus (a plain response) and does
// not call next. Delay is evaluated before abort, so a request may be both
// delayed and aborted. Sampling uses the package rng (see SetTransformRand) so
// tests are deterministic.
func FaultInjection(cfg config.FaultConfig) func(http.Handler) http.Handler {
	abortStatus := cfg.AbortStatus
	if abortStatus <= 0 {
		abortStatus = http.StatusServiceUnavailable
	}
	return func(next http.Handler) http.Handler {
		if !cfg.Enabled {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg.Delay > 0 && sampleHit(cfg.DelayPercent) {
				if !sleepCtx(r.Context(), cfg.Delay) {
					// Context cancelled during the injected delay: stop here
					// rather than proxying a request the client abandoned.
					return
				}
			}
			if sampleHit(cfg.AbortPercent) {
				http.Error(w, http.StatusText(abortStatus), abortStatus)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// sleepCtx sleeps for d or until ctx is done. It returns true if the full
// duration elapsed and false if the context was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// mirrorBodyCap bounds how much of a request body is buffered for mirroring so a
// large upload cannot exhaust memory. Requests whose bodies exceed the cap are
// still served normally to the primary; only the mirror copy is affected (it is
// best-effort by design).
const mirrorBodyCap = 1 << 20 // 1 MiB

// Mirror returns middleware that, for SamplePercent of requests, fires a
// fire-and-forget shadow copy of the request to cfg.URL. It is a no-op
// passthrough when cfg.Enabled is false, preserving current behavior.
//
// The primary request is never affected: the body is buffered (up to
// mirrorBodyCap) so both the primary and the shadow can read it, the shadow runs
// on its own bounded-timeout context in a separate goroutine, and its response
// and any error are discarded. A failing or slow mirror can never block or fail
// the client.
func Mirror(cfg config.MirrorConfig) func(http.Handler) http.Handler {
	client := &http.Client{Timeout: mirrorTimeout(cfg)}
	return func(next http.Handler) http.Handler {
		if !cfg.Enabled || cfg.URL == "" {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !sampleHit(cfg.SamplePercent) {
				next.ServeHTTP(w, r)
				return
			}

			// Buffer the body (bounded) so both primary and mirror can read it.
			var buffered []byte
			overCap := false
			if r.Body != nil {
				limited := io.LimitReader(r.Body, mirrorBodyCap+1)
				b, err := io.ReadAll(limited)
				if err == nil {
					if len(b) > mirrorBodyCap {
						// Body exceeds the cap: keep the primary correct by
						// re-joining the read bytes with the untouched rest, and
						// skip mirroring this request.
						overCap = true
						r.Body = struct {
							io.Reader
							io.Closer
						}{io.MultiReader(bytes.NewReader(b), r.Body), r.Body}
					} else {
						buffered = b
						r.Body = io.NopCloser(bytes.NewReader(b))
					}
				}
			}

			if !overCap {
				fireMirror(client, cfg.URL, r, buffered)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// mirrorTimeout returns the per-mirror-request timeout, defaulting to a small
// bound when unset so a hung shadow target cannot pin goroutines indefinitely.
func mirrorTimeout(cfg config.MirrorConfig) time.Duration {
	if cfg.Timeout > 0 {
		return cfg.Timeout
	}
	return 5 * time.Second
}

// fireMirror builds and sends a shadow copy of r to url on a background
// goroutine, discarding the response. It never touches the primary request or
// its writer. body is the buffered request body (may be nil for bodyless
// requests).
func fireMirror(client *http.Client, url string, r *http.Request, body []byte) {
	method := r.Method
	headers := r.Header.Clone()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), client.Timeout)
		defer cancel()

		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, reader)
		if err != nil {
			return
		}
		// Carry the original headers so the shadow target sees a faithful copy;
		// Host is intentionally left to the mirror URL.
		req.Header = headers
		req.Header.Set("X-Mirrored-From", r.Host)

		resp, err := client.Do(req)
		if err != nil {
			return
		}
		// Drain and close so connections can be reused; response is discarded.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
}
