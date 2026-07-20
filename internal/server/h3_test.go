package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
)

// generateSelfSignedCert creates an in-memory self-signed TLS certificate for
// testing. It uses ECDSA P-256 for speed; no files are written to disk.
func generateSelfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create x509 certificate: %v", err)
	}

	privDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal EC private key: %v", err)
	}

	return tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  priv,
		// Supply the parsed leaf so tls.Config does not have to parse it again.
		Leaf: func() *x509.Certificate {
			c, _ := x509.ParseCertificate(certDER)
			_ = privDER // used above; referenced to satisfy the compiler
			return c
		}(),
	}
}

// TestAltSvcMiddleware verifies that altSvcMiddleware injects the correct
// Alt-Svc header on every response regardless of the downstream handler.
func TestAltSvcMiddleware(t *testing.T) {
	const h3Port = 4433

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := altSvcMiddleware(h3Port)(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	wrapped.ServeHTTP(rec, req)

	altSvc := rec.Header().Get("Alt-Svc")
	if altSvc == "" {
		t.Fatal("Alt-Svc header not set")
	}
	if want := `h3=":4433"`; !containsStr(altSvc, "h3=") {
		t.Errorf("Alt-Svc header %q does not contain \"h3=\"; want substring of %q", altSvc, want)
	}
	if !containsStr(altSvc, "4433") {
		t.Errorf("Alt-Svc header %q does not contain port 4433", altSvc)
	}
}

// TestAltSvcMiddlewarePort verifies the port appears in the Alt-Svc header.
func TestAltSvcMiddlewarePort(t *testing.T) {
	const h3Port = 8443

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	wrapped := altSvcMiddleware(h3Port)(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	wrapped.ServeHTTP(rec, req)

	altSvc := rec.Header().Get("Alt-Svc")
	if !containsStr(altSvc, "8443") {
		t.Errorf("Alt-Svc header %q does not contain port 8443", altSvc)
	}
}

// TestStartH3RequiresTLS asserts that startH3 returns an error when the HTTP
// server has no TLS configuration (s.httpServer.TLSConfig == nil).
func TestStartH3RequiresTLS(t *testing.T) {
	// Build a minimal Server whose httpServer has no TLS config.
	cfg := minimalConfig()
	srv := &Server{
		cfg: cfg,
		httpServer: &http.Server{
			Addr:      cfg.GetAddr(),
			TLSConfig: nil, // deliberately nil: no TLS
		},
	}

	h3srv, err := srv.startH3(http.NotFoundHandler())
	if err == nil {
		// Clean up if somehow it started (should not happen).
		if h3srv != nil {
			_ = h3srv.Close()
		}
		t.Fatal("expected error from startH3 with no TLS config, got nil")
	}
}

// TestH3ServerStartStop verifies that an HTTP/3 server can be started and
// immediately shut down cleanly using an in-memory self-signed certificate.
func TestH3ServerStartStop(t *testing.T) {
	cert := generateSelfSignedCert(t)
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}

	cfg := minimalConfig()
	cfg.Server.HTTP3.Enabled = true
	cfg.Server.HTTP3.Port = 0 // will be overridden below

	// Pick an ephemeral-ish port for the test to avoid conflicts.
	cfg.Server.HTTP3.Port = cfg.Server.Port

	srv := &Server{
		cfg: cfg,
		httpServer: &http.Server{
			Addr:      cfg.GetAddr(),
			TLSConfig: tlsCfg,
		},
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h3srv, err := srv.startH3(handler)
	if err != nil {
		t.Fatalf("startH3 returned unexpected error: %v", err)
	}
	if h3srv == nil {
		t.Fatal("startH3 returned nil server without error")
	}

	// Immediately close — the server starts a background goroutine, so Close
	// must not block indefinitely when no connections have been accepted.
	if err := h3srv.Close(); err != nil {
		t.Errorf("h3srv.Close() returned error: %v", err)
	}
}

// minimalConfig returns the smallest valid *config.Config sufficient for server
// construction in unit tests (one backend, default port, no TLS).
func minimalConfig() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			Port:            19443,
			ShutdownTimeout: 5 * time.Second,
			HTTP3:           config.HTTP3Config{Port: 19443},
		},
		Backends: []config.BackendConfig{
			{URL: "http://127.0.0.1:9999", Weight: 1, MaxConns: 100},
		},
		LoadBalancer: config.LoadBalancerConfig{
			Algorithm: "round_robin",
			HealthCheck: config.HealthCheckConfig{
				Type:               "http",
				Method:             "GET",
				HealthyThreshold:   2,
				UnhealthyThreshold: 3,
				Jitter:             0.1,
			},
		},
		CircuitBreaker: config.CircuitBreakerConfig{
			Mode:               "consecutive",
			RollingWindow:      10 * time.Second,
			ErrorRateThreshold: 0.5,
			MinRequests:        20,
			TripOn:             []string{"connect", "timeout"},
		},
		Retry: config.RetryConfig{
			HonorRetryAfter: true,
			RetryOn:         []string{"connect", "timeout"},
		},
		Metrics: config.MetricsConfig{
			Host: "127.0.0.1",
		},
		TLS: config.TLSConfig{
			MinVersion: "1.2",
			ClientAuth: "none",
			ACME:       config.ACMEConfig{HTTPChallengePort: 80},
		},
		Security: config.SecurityConfig{
			Auth: config.AuthConfig{
				Header: "X-API-Key",
				JWTAlg: "HS256",
			},
		},
		RateLimiter: config.RateLimiterConfig{
			Algorithm:         "token_bucket",
			Key:               "ip",
			RetryAfterSeconds: 1,
			Message:           "Rate limit exceeded",
			SharedStore: config.SharedStoreConfig{
				Backend: "memory",
				Key:     "__global__",
				Redis:   config.RedisStoreConfig{Prefix: "rplb:rl"},
			},
		},
	}
}

// containsStr is a helper that reports whether substr appears in s.
func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}())
}
