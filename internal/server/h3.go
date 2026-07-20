package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/quic-go/quic-go/http3"
	"reverse-proxy-lb/internal/logging"
)

// startH3 creates and starts an HTTP/3 (QUIC) server using the same TLS
// configuration as the main HTTPS listener. It binds a UDP listener on
// cfg.Server.HTTP3.Port (already defaulted to the main server port) and serves
// in a background goroutine. The caller is responsible for calling Close() on
// the returned *http3.Server during shutdown.
//
// An error is returned immediately (without starting) when:
//   - TLS is not configured on the HTTP server (s.httpServer.TLSConfig == nil),
//     because QUIC mandates TLS 1.3 and cannot operate without a certificate.
func (s *Server) startH3(handler http.Handler) (*http3.Server, error) {
	tlsCfg := s.httpServer.TLSConfig
	if tlsCfg == nil {
		return nil, fmt.Errorf("http3: TLS config is required for HTTP/3 (QUIC mandates TLS)")
	}

	port := s.cfg.Server.HTTP3.Port
	if port == 0 {
		port = s.cfg.Server.Port
	}
	addr := net.JoinHostPort(s.cfg.Server.Host, strconv.Itoa(port))

	h3srv := &http3.Server{
		Addr:      addr,
		TLSConfig: tlsCfg,
		Handler:   handler,
	}

	go func() {
		if err := h3srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logging.Error("HTTP/3 server error", map[string]interface{}{
				"error": err.Error(),
			})
		}
	}()

	return h3srv, nil
}

// altSvcMiddleware returns an HTTP middleware that injects the Alt-Svc response
// header on every response, advertising HTTP/3 availability to clients. The
// header value follows RFC 7838: "h3=":PORT"; ma=86400".
//
// This should only be installed when HTTP/3 is enabled so that clients receive
// the upgrade hint exclusively when the QUIC listener is actually running.
func altSvcMiddleware(h3Port int) func(http.Handler) http.Handler {
	headerVal := fmt.Sprintf(`h3=":%d"; ma=86400`, h3Port)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Alt-Svc", headerVal)
			next.ServeHTTP(w, r)
		})
	}
}

// stopH3 gracefully shuts down the HTTP/3 server with the given timeout. It is
// safe to call when srv is nil (no-op). Errors are logged but not propagated
// because HTTP/3 is an optional transport; the primary HTTP/HTTPS listener
// shutdown is the authoritative path.
func stopH3(srv *http3.Server, timeout time.Duration) {
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil && err != http.ErrServerClosed {
		logging.Error("HTTP/3 shutdown error", map[string]interface{}{
			"error": err.Error(),
		})
	}
}
