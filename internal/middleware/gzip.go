package middleware

import (
	"bufio"
	"compress/gzip"
	"errors"
	"net"
	"net/http"
	"strings"

	"reverse-proxy-lb/internal/config"
)

// Gzip returns middleware that compresses responses with gzip when the client
// advertises support via Accept-Encoding. It skips WebSocket upgrade requests and
// never double-compresses responses that already carry a Content-Encoding.
//
// This is the zero-config entry point and preserves the original behavior
// (compress every eligible response). Callers wanting the Content-Type allowlist
// or minimum-size threshold use GzipWithConfig.
func Gzip(next http.Handler) http.Handler {
	return GzipWithConfig(config.CompressionConfig{}, next)
}

// GzipWithConfig is Gzip with the compression policy applied:
//
//   - cfg.ContentTypes, when non-empty, is an allowlist of Content-Type prefixes
//     (matched case-insensitively against the response Content-Type up to any
//     ";"). Responses whose type is not on the list are passed through
//     uncompressed. Empty means compress all eligible responses (current
//     behavior).
//   - cfg.MinSize, when > 0, is the minimum body size in bytes worth
//     compressing. The writer buffers up to MinSize bytes; if the whole response
//     turns out to be smaller (it finishes or flushes below the threshold) it is
//     written through uncompressed. MinSize == 0 disables buffering (current
//     behavior: compress immediately).
//
// With cfg zero-valued, GzipWithConfig behaves exactly like the original Gzip.
func GzipWithConfig(cfg config.CompressionConfig, next http.Handler) http.Handler {
	allow := normalizeContentTypes(cfg.ContentTypes)
	minSize := cfg.MinSize
	if minSize < 0 {
		minSize = 0
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !clientAcceptsGzip(r) || isWebSocketUpgrade(r) {
			next.ServeHTTP(w, r)
			return
		}

		gzw := &gzipResponseWriter{
			ResponseWriter: w,
			allowTypes:     allow,
			minSize:        minSize,
		}
		defer gzw.Close()
		next.ServeHTTP(gzw, r)
	})
}

// normalizeContentTypes lowercases and trims the allowlist so matching is a cheap
// case-insensitive prefix test. Returns nil for an empty list (meaning "all").
func normalizeContentTypes(types []string) []string {
	if len(types) == 0 {
		return nil
	}
	out := make([]string, 0, len(types))
	for _, t := range types {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func clientAcceptsGzip(r *http.Request) bool {
	for _, enc := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if strings.EqualFold(strings.TrimSpace(strings.SplitN(enc, ";", 2)[0]), "gzip") {
			return true
		}
	}
	return false
}

func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		return false
	}
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// contentTypeAllowed reports whether the response Content-Type is eligible for
// compression under the allowlist. An empty allowlist allows everything (current
// behavior). A response with no Content-Type is allowed only when the allowlist
// is empty.
func contentTypeAllowed(allow []string, h http.Header) bool {
	if len(allow) == 0 {
		return true
	}
	ct := h.Get("Content-Type")
	if ct == "" {
		return false
	}
	// Compare only the media type, ignoring any "; charset=..." parameters.
	ct = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	for _, prefix := range allow {
		if strings.HasPrefix(ct, prefix) {
			return true
		}
	}
	return false
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
	useGzip     bool

	// Compression policy.
	allowTypes []string
	minSize    int

	// Deferred-decision state used when minSize > 0. Until we have seen at least
	// minSize bytes (or a Flush/Close arrives), we buffer writes and the status
	// code so we can still choose to send them uncompressed if the body stays
	// small.
	deferring   bool
	pendingCode int
	buf         []byte
}

// decideAndWriteHeader determines (once) whether to compress this response and writes
// the status line. Responses that are already encoded, have no body semantics, are
// protocol upgrades, or whose Content-Type is not on the allowlist are passed
// through uncompressed.
func (g *gzipResponseWriter) decideAndWriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true

	h := g.ResponseWriter.Header()
	switch {
	case code == http.StatusSwitchingProtocols,
		code == http.StatusNoContent,
		code == http.StatusNotModified:
		g.useGzip = false
	case h.Get("Content-Encoding") != "":
		g.useGzip = false
	case !contentTypeAllowed(g.allowTypes, h):
		g.useGzip = false
	default:
		g.useGzip = true
		h.Del("Content-Length") // length changes after compression
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		g.gz = gzip.NewWriter(g.ResponseWriter)
	}

	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	// With a size threshold, defer the header/compression decision until we know
	// whether the body reaches minSize. We still capture the intended status.
	if g.minSize > 0 && !g.wroteHeader && g.gzipEligiblePreBody() {
		g.deferring = true
		g.pendingCode = code
		return
	}
	g.decideAndWriteHeader(code)
}

// gzipEligiblePreBody reports whether, based only on headers known before the
// body is written, this response could still be compressed. Used to decide
// whether to buffer for the minSize threshold. When it is already clear the
// response will not be gzipped, we skip buffering and write straight through.
func (g *gzipResponseWriter) gzipEligiblePreBody() bool {
	h := g.ResponseWriter.Header()
	if h.Get("Content-Encoding") != "" {
		return false
	}
	return contentTypeAllowed(g.allowTypes, h)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if g.deferring {
		return g.writeDeferred(b)
	}
	if !g.wroteHeader {
		// No status was written explicitly. If a size threshold applies and this
		// response could be compressed, start deferring so a tiny body stays
		// uncompressed.
		if g.minSize > 0 && g.gzipEligiblePreBody() {
			g.deferring = true
			g.pendingCode = http.StatusOK
			return g.writeDeferred(b)
		}
		g.decideAndWriteHeader(http.StatusOK)
	}
	if g.useGzip {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

// writeDeferred accumulates body bytes while under the minSize threshold. Once
// the buffer reaches minSize we commit to compression, flush the buffer through
// the gzip writer, and stop deferring. The full length of b is always reported
// as written (it is either buffered or forwarded).
func (g *gzipResponseWriter) writeDeferred(b []byte) (int, error) {
	g.buf = append(g.buf, b...)
	if len(g.buf) < g.minSize {
		return len(b), nil
	}
	// Threshold reached: commit to gzip and drain the buffer.
	if err := g.commit(true); err != nil {
		return 0, err
	}
	return len(b), nil
}

// commit finalizes a deferred response. gzipIt selects whether the buffered
// bytes are compressed (threshold reached) or written through as-is (body stayed
// below minSize). It writes the captured status code, then the buffered bytes,
// and clears the deferring state.
func (g *gzipResponseWriter) commit(gzipIt bool) error {
	if !g.deferring {
		return nil
	}
	g.deferring = false

	if gzipIt {
		g.decideAndWriteHeader(g.pendingCode)
	} else {
		// Small body: send uncompressed. Force useGzip off regardless of type.
		g.wroteHeader = true
		g.useGzip = false
		g.ResponseWriter.WriteHeader(g.pendingCode)
	}

	if len(g.buf) == 0 {
		return nil
	}
	buf := g.buf
	g.buf = nil
	if g.useGzip {
		_, err := g.gz.Write(buf)
		return err
	}
	_, err := g.ResponseWriter.Write(buf)
	return err
}

func (g *gzipResponseWriter) Flush() {
	// A flush means the client wants bytes now; we can no longer wait to reach
	// minSize, so commit to compression (a flush implies a streaming response
	// where compression is generally desired).
	if g.deferring {
		_ = g.commit(true)
	}
	if g.useGzip && g.gz != nil {
		g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying writer so WebSocket upgrades keep working even if
// this wrapper is in the chain (it will not be for detected upgrades, but a handler
// may upgrade on its own).
func (g *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := g.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
}

func (g *gzipResponseWriter) Close() error {
	// A response that stayed under minSize (or never wrote at all) is flushed out
	// uncompressed here.
	if g.deferring {
		if err := g.commit(false); err != nil {
			return err
		}
	}
	if g.useGzip && g.gz != nil {
		return g.gz.Close()
	}
	return nil
}
