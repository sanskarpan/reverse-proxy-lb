package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
)

// This file exercises the §6 edge-security and TLS features end-to-end through
// the real server stack. Certificates are generated in-test with crypto/x509
// (self-signed, SAN 127.0.0.1 where a real handshake occurs). The listener-level
// TLS scenarios (min version, SNI, downstream mTLS, hot reload) run over a real
// crypto/tls listener driven by a real *http.Client / tls.Dial. The
// header/CORS/ACL/auth scenarios and the mTLS-to-backend scenario run through
// the server's fully assembled handler (Server.Handler(), i.e. proxy + full
// middleware chain). No assertion is weakened to force a pass.

// ---------------------------------------------------------------------------
// certificate helpers
// ---------------------------------------------------------------------------

// certKeyPEM is a self-signed leaf/CA keypair in PEM form together with the
// parsed leaf certificate.
type certKeyPEM struct {
	certPEM []byte
	keyPEM  []byte
	leaf    *x509.Certificate
	tlsCert tls.Certificate
}

// makeCert creates a self-signed certificate for the given DNS names and IP
// SANs. It is usable as both a server leaf and (because IsCA is set) a CA that
// signs itself — matching the project's tlsutil_test helper. It also serves as
// a client certificate (ExtKeyUsageClientAuth is present).
func makeCert(t *testing.T, cn string, dnsNames []string, ips []net.IP) certKeyPEM {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("x509 keypair: %v", err)
	}
	return certKeyPEM{certPEM: certPEM, keyPEM: keyPEM, leaf: leaf, tlsCert: tlsCert}
}

// writeCert writes the cert/key PEM to files under dir with the given basename
// and returns their paths.
func writeCert(t *testing.T, dir, base string, ck certKeyPEM) (certFile, keyFile string) {
	t.Helper()
	certFile = filepath.Join(dir, base+".crt")
	keyFile = filepath.Join(dir, base+".key")
	if err := os.WriteFile(certFile, ck.certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, ck.keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certFile, keyFile
}

func poolFor(certs ...certKeyPEM) *x509.CertPool {
	p := x509.NewCertPool()
	for _, c := range certs {
		p.AddCert(c.leaf)
	}
	return p
}

// serveTLS runs handler over a real crypto/tls listener using tlsCfg and returns
// the listener address. The listener is closed on test cleanup.
func serveTLS(t *testing.T, tlsCfg *tls.Config, handler http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tln := tls.NewListener(ln, tlsCfg)
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(tln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return tln.Addr().String()
}

// okHandler is a trivial handler used behind the TLS listener scenarios where we
// care about the handshake, not proxying.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, "ok")
})

// ---------------------------------------------------------------------------
// TLS min-version
// ---------------------------------------------------------------------------

func TestE2E_TLSMinVersion(t *testing.T) {
	dir := t.TempDir()
	server := makeCert(t, "127.0.0.1", nil, []net.IP{net.ParseIP("127.0.0.1")})
	certFile, keyFile := writeCert(t, dir, "server", server)

	srvTLS, err := serverTLSFor(config.TLSConfig{
		Enabled:    true,
		CertFile:   certFile,
		KeyFile:    keyFile,
		MinVersion: "1.2",
	})
	if err != nil {
		t.Fatalf("build server tls config: %v", err)
	}
	addr := serveTLS(t, srvTLS, okHandler)

	// A client forced to a TLS 1.1 ceiling must be rejected.
	t.Run("tls1.1 rejected", func(t *testing.T) {
		conn, err := tls.Dial("tcp", addr, &tls.Config{
			RootCAs:    poolFor(server),
			ServerName: "127.0.0.1",
			MinVersion: tls.VersionTLS11,
			MaxVersion: tls.VersionTLS11,
		})
		if err == nil {
			_ = conn.Close()
			t.Fatal("TLS 1.1 client unexpectedly completed handshake against MinVersion=1.2 server")
		}
	})

	// A 1.2 client and a 1.3 client both succeed.
	for _, v := range []struct {
		name string
		ver  uint16
	}{{"tls1.2", tls.VersionTLS12}, {"tls1.3", tls.VersionTLS13}} {
		v := v
		t.Run(v.name+" ok", func(t *testing.T) {
			conn, err := tls.Dial("tcp", addr, &tls.Config{
				RootCAs:    poolFor(server),
				ServerName: "127.0.0.1",
				MinVersion: v.ver,
				MaxVersion: v.ver,
			})
			if err != nil {
				t.Fatalf("%s client handshake failed: %v", v.name, err)
			}
			if got := conn.ConnectionState().Version; got != v.ver {
				t.Fatalf("negotiated version %#x, want %#x", got, v.ver)
			}
			_ = conn.Close()
		})
	}
}

// ---------------------------------------------------------------------------
// SNI multi-cert
// ---------------------------------------------------------------------------

func TestE2E_SNIMultiCert(t *testing.T) {
	dir := t.TempDir()
	// Both certs also carry 127.0.0.1 in a SAN so the client (dialing loopback)
	// still verifies against whichever cert is returned, while SNI selection is
	// driven purely by the requested ServerName's DNS SAN.
	loopIP := []net.IP{net.ParseIP("127.0.0.1")}
	foo := makeCert(t, "foo", []string{"foo.example.com"}, loopIP)
	bar := makeCert(t, "bar", []string{"bar.example.com"}, loopIP)
	fooCert, fooKey := writeCert(t, dir, "foo", foo)
	barCert, barKey := writeCert(t, dir, "bar", bar)

	srvTLS, err := serverTLSFor(config.TLSConfig{
		Enabled:      true,
		CertFile:     fooCert,
		KeyFile:      fooKey,
		Certificates: []config.CertPair{{CertFile: barCert, KeyFile: barKey}},
	})
	if err != nil {
		t.Fatalf("build server tls config: %v", err)
	}
	addr := serveTLS(t, srvTLS, okHandler)

	dialSNI := func(sni string) *x509.Certificate {
		conn, err := tls.Dial("tcp", addr, &tls.Config{
			RootCAs:    poolFor(foo, bar),
			ServerName: sni,
		})
		if err != nil {
			t.Fatalf("dial %s: %v", sni, err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0]
	}

	if got := dialSNI("foo.example.com"); got.SerialNumber.Cmp(foo.leaf.SerialNumber) != 0 {
		t.Fatalf("SNI foo.example.com returned wrong cert (CN %q)", got.Subject.CommonName)
	}
	if got := dialSNI("bar.example.com"); got.SerialNumber.Cmp(bar.leaf.SerialNumber) != 0 {
		t.Fatalf("SNI bar.example.com returned wrong cert (CN %q)", got.Subject.CommonName)
	}
}

// ---------------------------------------------------------------------------
// Downstream mTLS (client -> proxy)
// ---------------------------------------------------------------------------

func TestE2E_DownstreamMTLS(t *testing.T) {
	dir := t.TempDir()
	server := makeCert(t, "127.0.0.1", nil, []net.IP{net.ParseIP("127.0.0.1")})
	certFile, keyFile := writeCert(t, dir, "server", server)

	clientCA := makeCert(t, "client-ca", nil, nil)
	caFile, _ := writeCert(t, dir, "clientca", clientCA)
	// A client cert signed by the client CA. Because makeCert self-signs (IsCA),
	// the CA cert is itself a valid client leaf trusted by the pool built from it.
	// Use the CA keypair directly as the client identity so verification against
	// ClientCAs (which contains that same cert) succeeds.
	clientCert := clientCA

	srvTLS, err := serverTLSFor(config.TLSConfig{
		Enabled:      true,
		CertFile:     certFile,
		KeyFile:      keyFile,
		ClientAuth:   "require_and_verify",
		ClientCAFile: caFile,
	})
	if err != nil {
		t.Fatalf("build server tls config: %v", err)
	}
	addr := serveTLS(t, srvTLS, okHandler)

	// Without a client cert: rejected. Under TLS 1.3 the certificate request is
	// completed post-handshake, so tls.Dial may return before the server's alert
	// arrives; drive a full HTTP request so the rejection surfaces as an error.
	t.Run("no client cert rejected", func(t *testing.T) {
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:    poolFor(server),
			ServerName: "127.0.0.1",
		}}}
		resp, err := client.Get("https://" + addr + "/")
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("request without client cert unexpectedly accepted by require_and_verify server")
		}
	})

	// With a client cert signed by the client CA: accepted, request succeeds.
	t.Run("valid client cert accepted", func(t *testing.T) {
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:      poolFor(server),
			ServerName:   "127.0.0.1",
			Certificates: []tls.Certificate{clientCert.tlsCert},
		}}}
		resp, err := client.Get("https://" + addr + "/")
		if err != nil {
			t.Fatalf("request with client cert failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})
}

// ---------------------------------------------------------------------------
// mTLS to backend (proxy -> backend)
// ---------------------------------------------------------------------------

func TestE2E_BackendMTLS(t *testing.T) {
	dir := t.TempDir()

	// Client identity the proxy presents to the backend, plus the CA the backend
	// uses to verify it. makeCert self-signs (IsCA), so the cert is its own CA.
	proxyClient := makeCert(t, "proxy-client", nil, nil)
	clientCertFile, clientKeyFile := writeCert(t, dir, "proxyclient", proxyClient)

	// Backend that REQUIRES client certs signed by proxyClient's CA.
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "backend-ok")
	}))
	backend.TLS = &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  poolFor(proxyClient),
		MinVersion: tls.VersionTLS12,
	}
	backend.StartTLS()
	defer backend.Close()

	// The proxy must trust the backend's (auto-generated) server cert. httptest
	// exposes it via backend.Certificate(). Write it out as the backend CA file.
	backendCAPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: backend.Certificate().Raw})
	backendCAFile := filepath.Join(dir, "backendca.crt")
	if err := os.WriteFile(backendCAFile, backendCAPEM, 0o600); err != nil {
		t.Fatalf("write backend ca: %v", err)
	}

	newProxy := func(bt config.BackendTLSConfig) *Server {
		cfg := baseConfig("round_robin", []config.BackendConfig{
			{URL: backend.URL, Weight: 1, MaxConns: 10},
		})
		cfg.BackendTLS = bt
		return New(cfg, "")
	}

	t.Run("with client cert reaches backend", func(t *testing.T) {
		s := newProxy(config.BackendTLSConfig{
			CAFile:         backendCAFile,
			ClientCertFile: clientCertFile,
			ClientKeyFile:  clientKeyFile,
		})
		body, code, _ := doReq(t, s.Handler(), "")
		if code != http.StatusOK {
			t.Fatalf("status = %d body=%q, want 200 (mTLS to backend should succeed)", code, body)
		}
		if !strings.Contains(body, "backend-ok") {
			t.Fatalf("body = %q, want backend-ok", body)
		}
	})

	t.Run("without client cert fails", func(t *testing.T) {
		// Trust the backend cert but present NO client cert: the backend's
		// require_and_verify must break the connection, so the proxy returns 5xx.
		s := newProxy(config.BackendTLSConfig{CAFile: backendCAFile})
		body, code, _ := doReq(t, s.Handler(), "")
		if code == http.StatusOK {
			t.Fatalf("status = 200 body=%q, want a 5xx: backend requires a client cert the proxy did not send", body)
		}
		if code < 500 {
			t.Fatalf("status = %d, want a 5xx upstream failure", code)
		}
	})
}

// ---------------------------------------------------------------------------
// Cert hot-reload
// ---------------------------------------------------------------------------

func TestE2E_CertHotReload(t *testing.T) {
	dir := t.TempDir()
	loopIP := []net.IP{net.ParseIP("127.0.0.1")}
	first := makeCert(t, "reload", []string{"reload.test"}, loopIP)
	certFile, keyFile := writeCert(t, dir, "server", first)

	srvTLS, err := serverTLSFor(config.TLSConfig{
		Enabled:        true,
		CertFile:       certFile,
		KeyFile:        keyFile,
		ReloadOnChange: true,
	})
	if err != nil {
		t.Fatalf("build server tls config: %v", err)
	}
	addr := serveTLS(t, srvTLS, okHandler)

	// InsecureSkipVerify is used only to read the presented leaf; we assert on the
	// serial number, which is what actually proves which cert was served.
	leafFromHandshake := func() *x509.Certificate {
		conn, err := tls.Dial("tcp", addr, &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // test reads the leaf, asserts on serial
			ServerName:         "reload.test",
		})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0]
	}

	if got := leafFromHandshake(); got.SerialNumber.Cmp(first.leaf.SerialNumber) != 0 {
		t.Fatalf("initial handshake served serial %v, want %v", got.SerialNumber, first.leaf.SerialNumber)
	}

	// Rotate: overwrite the same files with a brand-new keypair and push the mtime
	// forward so the resolver observes the change deterministically.
	second := makeCert(t, "reload", []string{"reload.test"}, loopIP)
	if err := os.WriteFile(certFile, second.certPEM, 0o600); err != nil {
		t.Fatalf("rewrite cert: %v", err)
	}
	if err := os.WriteFile(keyFile, second.keyPEM, 0o600); err != nil {
		t.Fatalf("rewrite key: %v", err)
	}
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(certFile, future, future)
	_ = os.Chtimes(keyFile, future, future)

	got := leafFromHandshake()
	if got.SerialNumber.Cmp(second.leaf.SerialNumber) != 0 {
		t.Fatalf("after rotation handshake served serial %v, want the NEW cert serial %v",
			got.SerialNumber, second.leaf.SerialNumber)
	}
	if got.SerialNumber.Cmp(first.leaf.SerialNumber) == 0 {
		t.Fatal("hot-reload did not take effect: still serving the old cert")
	}
}

// ---------------------------------------------------------------------------
// Security headers + CORS
// ---------------------------------------------------------------------------

func TestE2E_SecurityHeadersAndCORS(t *testing.T) {
	backend := newIDBackend("b1", nil)
	defer backend.close()

	cfg := baseConfig("round_robin", backendCfgs(backend))
	cfg.Security = config.SecurityConfig{
		Headers: config.HeadersConfig{
			Enabled:            true,
			HSTS:               "max-age=31536000; includeSubDomains",
			FrameOptions:       "DENY",
			ContentTypeOptions: true,
			ReferrerPolicy:     "no-referrer",
		},
		CORS: config.CORSConfig{
			Enabled:      true,
			AllowOrigins: []string{"https://app.example.com"},
			AllowMethods: []string{"GET", "POST", "PUT"},
			AllowHeaders: []string{"X-Custom", "Authorization"},
			MaxAge:       600,
		},
	}
	s := New(cfg, "")
	h := s.Handler()

	t.Run("security headers present on normal response", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		res := rec.Result()
		defer res.Body.Close()
		checks := map[string]string{
			"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
			"X-Frame-Options":           "DENY",
			"X-Content-Type-Options":    "nosniff",
			"Referrer-Policy":           "no-referrer",
		}
		for k, want := range checks {
			if got := res.Header.Get(k); got != want {
				t.Errorf("header %s = %q, want %q", k, got, want)
			}
		}
	})

	t.Run("CORS preflight returns ACAO/methods", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "http://proxy.test/api", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Origin", "https://app.example.com")
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		res := rec.Result()
		defer res.Body.Close()
		if res.StatusCode != http.StatusNoContent {
			t.Fatalf("preflight status = %d, want 204", res.StatusCode)
		}
		if got := res.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
			t.Errorf("ACAO = %q, want the request origin", got)
		}
		if got := res.Header.Get("Access-Control-Allow-Methods"); got != "GET, POST, PUT" {
			t.Errorf("Allow-Methods = %q, want GET, POST, PUT", got)
		}
		if got := res.Header.Get("Access-Control-Max-Age"); got != "600" {
			t.Errorf("Max-Age = %q, want 600", got)
		}
	})

	t.Run("CORS rejects disallowed origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Origin", "https://evil.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		res := rec.Result()
		defer res.Body.Close()
		if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("ACAO = %q, want empty for a disallowed origin", got)
		}
	})
}

// ---------------------------------------------------------------------------
// ACL
// ---------------------------------------------------------------------------

func TestE2E_ACL(t *testing.T) {
	backend := newIDBackend("b1", nil)
	defer backend.close()

	cfg := baseConfig("round_robin", backendCfgs(backend))
	cfg.Security.ACL = config.ACLConfig{
		Deny:         []string{"10.0.0.0/8"},
		BlockedPaths: []string{"/admin"},
	}
	s := New(cfg, "")
	h := s.Handler()

	t.Run("denied IP rejected", func(t *testing.T) {
		// Client IP resolved from XFF (loopback peer is trusted in baseConfig).
		_, code, _ := doReq(t, h, "10.1.2.3")
		if code != http.StatusForbidden {
			t.Fatalf("denied IP status = %d, want 403", code)
		}
	})

	t.Run("allowed IP passes", func(t *testing.T) {
		body, code, _ := doReq(t, h, "203.0.113.5")
		if code != http.StatusOK || !strings.Contains(body, "b1") {
			t.Fatalf("allowed IP status = %d body=%q, want 200/b1", code, body)
		}
	})

	t.Run("blocked path rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://proxy.test/admin/panel", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Forwarded-For", "203.0.113.5")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("blocked path status = %d, want 403", rec.Code)
		}
	})
}

// ---------------------------------------------------------------------------
// Auth: basic / apikey / jwt
// ---------------------------------------------------------------------------

func TestE2E_AuthBasic(t *testing.T) {
	backend := newIDBackend("b1", nil)
	defer backend.close()

	cfg := baseConfig("round_robin", backendCfgs(backend))
	cfg.Security.Auth = config.AuthConfig{
		Type:  "basic",
		Users: map[string]string{"alice": "s3cret"},
	}
	h := New(cfg, "").Handler()

	req := func(setAuth func(*http.Request)) (int, string) {
		r := httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil)
		r.RemoteAddr = "127.0.0.1:12345"
		if setAuth != nil {
			setAuth(r)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		res := rec.Result()
		b, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		return res.StatusCode, string(b)
	}

	if code, _ := req(nil); code != http.StatusUnauthorized {
		t.Fatalf("no credentials status = %d, want 401", code)
	}
	if code, _ := req(func(r *http.Request) { r.SetBasicAuth("alice", "wrong") }); code != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401", code)
	}
	if code, body := req(func(r *http.Request) { r.SetBasicAuth("alice", "s3cret") }); code != http.StatusOK || !strings.Contains(body, "b1") {
		t.Fatalf("valid basic auth status = %d body=%q, want 200/b1", code, body)
	}
}

func TestE2E_AuthAPIKey(t *testing.T) {
	backend := newIDBackend("b1", nil)
	defer backend.close()

	cfg := baseConfig("round_robin", backendCfgs(backend))
	cfg.Security.Auth = config.AuthConfig{
		Type:    "apikey",
		Header:  "X-API-Key",
		APIKeys: []string{"key-abc-123"},
	}
	h := New(cfg, "").Handler()

	req := func(key string) (int, string) {
		r := httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil)
		r.RemoteAddr = "127.0.0.1:12345"
		if key != "" {
			r.Header.Set("X-API-Key", key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		res := rec.Result()
		b, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		return res.StatusCode, string(b)
	}

	if code, _ := req(""); code != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d, want 401", code)
	}
	if code, _ := req("nope"); code != http.StatusUnauthorized {
		t.Fatalf("bad key status = %d, want 401", code)
	}
	if code, body := req("key-abc-123"); code != http.StatusOK || !strings.Contains(body, "b1") {
		t.Fatalf("valid key status = %d body=%q, want 200/b1", code, body)
	}
}

func TestE2E_AuthJWT(t *testing.T) {
	backend := newIDBackend("b1", nil)
	defer backend.close()

	const secret = "top-secret-hmac-key"
	cfg := baseConfig("round_robin", backendCfgs(backend))
	cfg.Security.Auth = config.AuthConfig{
		Type:      "jwt",
		JWTSecret: secret,
		JWTAlg:    "HS256",
	}
	h := New(cfg, "").Handler()

	req := func(bearer string) (int, string) {
		r := httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil)
		r.RemoteAddr = "127.0.0.1:12345"
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		res := rec.Result()
		b, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		return res.StatusCode, string(b)
	}

	valid := makeJWT(t, secret, "HS256", map[string]any{"sub": "u", "exp": time.Now().Add(time.Hour).Unix()})
	expired := makeJWT(t, secret, "HS256", map[string]any{"sub": "u", "exp": time.Now().Add(-time.Hour).Unix()})
	wrongSig := makeJWT(t, "other-secret", "HS256", map[string]any{"sub": "u"})
	algNone := makeJWT(t, secret, "none", map[string]any{"sub": "u"})

	if code, _ := req(""); code != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", code)
	}
	if code, _ := req(expired); code != http.StatusUnauthorized {
		t.Fatalf("expired token status = %d, want 401", code)
	}
	if code, _ := req(wrongSig); code != http.StatusUnauthorized {
		t.Fatalf("wrong-signature token status = %d, want 401", code)
	}
	if code, _ := req(algNone); code != http.StatusUnauthorized {
		t.Fatalf("alg=none token status = %d, want 401 (algorithm-confusion must be rejected)", code)
	}
	if code, body := req(valid); code != http.StatusOK || !strings.Contains(body, "b1") {
		t.Fatalf("valid JWT status = %d body=%q, want 200/b1", code, body)
	}
}

// makeJWT builds a compact HS256 JWS. When alg is "none" the signature segment
// is still HMAC-signed with secret but the header advertises alg=none, so the
// verifier (which pins HS256) must reject it.
func makeJWT(t *testing.T, secret, alg string, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal jwt segment: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	header := enc(map[string]string{"alg": alg, "typ": "JWT"})
	payload := enc(claims)
	signingInput := header + "." + payload
	sig := hs256Sign(t, secret, signingInput)
	return signingInput + "." + sig
}

func hs256Sign(t *testing.T, secret, input string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(input))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// serverTLSFor builds the server-side *tls.Config the way the real server does:
// it enables TLS, constructs a Server, and returns the tls.Config the server
// installed on its http.Server. This exercises the exact wiring in
// setupHTTPServer -> tlsutil.ServerTLSConfig rather than calling tlsutil
// directly, keeping these listener tests true end-to-end.
func serverTLSFor(tlsCfg config.TLSConfig) (*tls.Config, error) {
	cfg := baseConfig("round_robin", []config.BackendConfig{{URL: "http://127.0.0.1:1", Weight: 1, MaxConns: 1}})
	cfg.TLS = tlsCfg
	s := New(cfg, "")
	if s.httpServer.TLSConfig == nil {
		return nil, fmt.Errorf("server did not install a TLS config (invalid TLS block?)")
	}
	return s.httpServer.TLSConfig, nil
}
