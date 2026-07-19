package tlsutil

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ocsp"

	"reverse-proxy-lb/internal/config"
)

// genSelfSigned creates a self-signed leaf certificate for the given DNS names
// and writes the cert and key PEM files into dir. It returns the file paths and
// the parsed certificate (useful for building a client CA pool).
func genSelfSigned(t *testing.T, dir, name string, dnsNames []string) (certFile, keyFile string, leaf *x509.Certificate) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dnsNames,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}

	certFile = filepath.Join(dir, name+".crt")
	keyFile = filepath.Join(dir, name+".key")

	writePEM(t, certFile, "CERTIFICATE", der)

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	writePEM(t, keyFile, "EC PRIVATE KEY", keyDER)

	return certFile, keyFile, parsed
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	buf := pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestServerTLSConfigMinVersion(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := genSelfSigned(t, dir, "example.com", []string{"example.com"})

	t.Run("default is 1.2", func(t *testing.T) {
		tc, err := ServerTLSConfig(config.TLSConfig{CertFile: certFile, KeyFile: keyFile})
		if err != nil {
			t.Fatal(err)
		}
		if tc.MinVersion != tls.VersionTLS12 {
			t.Fatalf("MinVersion = %#x, want TLS 1.2 (%#x)", tc.MinVersion, tls.VersionTLS12)
		}
	})

	t.Run("explicit 1.3", func(t *testing.T) {
		tc, err := ServerTLSConfig(config.TLSConfig{CertFile: certFile, KeyFile: keyFile, MinVersion: "1.3"})
		if err != nil {
			t.Fatal(err)
		}
		if tc.MinVersion != tls.VersionTLS13 {
			t.Fatalf("MinVersion = %#x, want TLS 1.3 (%#x)", tc.MinVersion, tls.VersionTLS13)
		}
	})
}

func TestServerTLSConfigCipherSuites(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := genSelfSigned(t, dir, "example.com", []string{"example.com"})

	tc, err := ServerTLSConfig(config.TLSConfig{
		CertFile:     certFile,
		KeyFile:      keyFile,
		CipherSuites: []string{"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tc.CipherSuites) != 1 || tc.CipherSuites[0] != tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256 {
		t.Fatalf("CipherSuites = %v, want the one requested suite", tc.CipherSuites)
	}

	if _, err := ServerTLSConfig(config.TLSConfig{
		CertFile:     certFile,
		KeyFile:      keyFile,
		CipherSuites: []string{"TLS_NOT_A_REAL_SUITE"},
	}); err == nil {
		t.Fatal("expected error for unknown cipher suite, got nil")
	}
}

func TestServerTLSConfigSNISelectsCert(t *testing.T) {
	dir := t.TempDir()
	fooCert, fooKey, fooLeaf := genSelfSigned(t, dir, "foo", []string{"foo.example.com"})
	barCert, barKey, barLeaf := genSelfSigned(t, dir, "bar", []string{"bar.example.com"})

	tc, err := ServerTLSConfig(config.TLSConfig{
		CertFile:     fooCert,
		KeyFile:      fooKey,
		Certificates: []config.CertPair{{CertFile: barCert, KeyFile: barKey}},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := tc.GetCertificate(&tls.ClientHelloInfo{ServerName: "bar.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if leaf := parseLeaf(t, got); leaf.SerialNumber.Cmp(barLeaf.SerialNumber) != 0 {
		t.Fatalf("SNI bar.example.com selected the wrong certificate")
	}

	got, err = tc.GetCertificate(&tls.ClientHelloInfo{ServerName: "foo.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if leaf := parseLeaf(t, got); leaf.SerialNumber.Cmp(fooLeaf.SerialNumber) != 0 {
		t.Fatalf("SNI foo.example.com selected the wrong certificate")
	}

	// Unknown SNI falls back to the primary (foo) certificate.
	got, err = tc.GetCertificate(&tls.ClientHelloInfo{ServerName: "unknown.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if leaf := parseLeaf(t, got); leaf.SerialNumber.Cmp(fooLeaf.SerialNumber) != 0 {
		t.Fatalf("unknown SNI did not fall back to primary certificate")
	}
}

func TestServerTLSConfigClientAuthRequireAndVerify(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := genSelfSigned(t, dir, "example.com", []string{"example.com"})
	caFile, _, caLeaf := genSelfSigned(t, dir, "clientca", []string{"clientca"})

	tc, err := ServerTLSConfig(config.TLSConfig{
		CertFile:     certFile,
		KeyFile:      keyFile,
		ClientAuth:   "require_and_verify",
		ClientCAFile: caFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tc.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v, want RequireAndVerifyClientCert", tc.ClientAuth)
	}
	if tc.ClientCAs == nil {
		t.Fatal("ClientCAs is nil, want populated pool")
	}
	// The configured CA must be a trusted subject in the pool.
	subjects := tc.ClientCAs.Subjects() //nolint:staticcheck // acceptable in test for verifying pool contents
	found := false
	for _, s := range subjects {
		if string(s) == string(caLeaf.RawSubject) {
			found = true
		}
	}
	if !found {
		t.Fatal("ClientCAs does not contain the configured CA subject")
	}

	// require_and_verify without a CA file is an error.
	if _, err := ServerTLSConfig(config.TLSConfig{
		CertFile:   certFile,
		KeyFile:    keyFile,
		ClientAuth: "require_and_verify",
	}); err == nil {
		t.Fatal("expected error for require_and_verify without client_ca_file")
	}

	// "request" maps to RequestClientCert.
	reqTC, err := ServerTLSConfig(config.TLSConfig{
		CertFile:   certFile,
		KeyFile:    keyFile,
		ClientAuth: "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reqTC.ClientAuth != tls.RequestClientCert {
		t.Fatalf("ClientAuth = %v, want RequestClientCert", reqTC.ClientAuth)
	}
}

func TestServerTLSConfigHotReload(t *testing.T) {
	dir := t.TempDir()
	// Initial cert for example.com.
	certFile, keyFile, firstLeaf := genSelfSigned(t, dir, "example.com", []string{"example.com"})

	tc, err := ServerTLSConfig(config.TLSConfig{
		CertFile:       certFile,
		KeyFile:        keyFile,
		ReloadOnChange: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := tc.GetCertificate(&tls.ClientHelloInfo{ServerName: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if parseLeaf(t, got).SerialNumber.Cmp(firstLeaf.SerialNumber) != 0 {
		t.Fatal("initial GetCertificate returned unexpected cert")
	}

	// Rewrite the same paths with a brand-new keypair (rotation) and bump the
	// mtime forward so the resolver observes the change deterministically.
	newCert, newKey, secondLeaf := genSelfSigned(t, t.TempDir(), "example.com", []string{"example.com"})
	copyFile(t, newCert, certFile)
	copyFile(t, newKey, keyFile)
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(certFile, future, future); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(keyFile, future, future); err != nil {
		t.Fatal(err)
	}

	got, err = tc.GetCertificate(&tls.ClientHelloInfo{ServerName: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if parseLeaf(t, got).SerialNumber.Cmp(secondLeaf.SerialNumber) != 0 {
		t.Fatal("hot-reload did not return the rotated certificate")
	}

	// Without reload, the same rotation must NOT take effect.
	certFile2, keyFile2, first2 := genSelfSigned(t, t.TempDir(), "example.com", []string{"example.com"})
	staticTC, err := ServerTLSConfig(config.TLSConfig{CertFile: certFile2, KeyFile: keyFile2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := staticTC.GetCertificate(&tls.ClientHelloInfo{ServerName: "example.com"}); err != nil {
		t.Fatal(err)
	}
	rot, rotKey, _ := genSelfSigned(t, t.TempDir(), "example.com", []string{"example.com"})
	copyFile(t, rot, certFile2)
	copyFile(t, rotKey, keyFile2)
	if err := os.Chtimes(certFile2, future, future); err != nil {
		t.Fatal(err)
	}
	got, err = staticTC.GetCertificate(&tls.ClientHelloInfo{ServerName: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if parseLeaf(t, got).SerialNumber.Cmp(first2.SerialNumber) != 0 {
		t.Fatal("static config unexpectedly reloaded the certificate")
	}
}

func parseLeaf(t *testing.T, cert *tls.Certificate) *x509.Certificate {
	t.Helper()
	if cert.Leaf != nil {
		return cert.Leaf
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return leaf
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

// ---------------------------------------------------------------------------
// ACME wiring tests (6.2)
// ---------------------------------------------------------------------------

// NewACMEManager must wire an autocert-backed *tls.Config (GetCertificate set)
// and a non-nil HTTP-01 challenge handler that share the same manager.
func TestNewACMEManagerWiring(t *testing.T) {
	cfg := config.TLSConfig{
		ACME: config.ACMEConfig{
			Enabled: true,
			Domains: []string{"example.com"},
			Email:   "ops@example.com",
		},
	}

	tc, handler, err := NewACMEManager(cfg)
	if err != nil {
		t.Fatalf("NewACMEManager: %v", err)
	}
	if tc == nil {
		t.Fatal("expected non-nil *tls.Config")
	}
	if tc.GetCertificate == nil {
		t.Fatal("expected GetCertificate to be set by autocert manager")
	}
	if handler == nil {
		t.Fatal("expected non-nil HTTP-01 challenge handler")
	}
	if tc.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %d, want TLS1.2 default", tc.MinVersion)
	}
}

// ServerTLSConfig must route to the ACME path (and not require cert_file) when
// ACME is enabled, honoring MinVersion.
func TestServerTLSConfigUsesACMEWhenEnabled(t *testing.T) {
	cfg := config.TLSConfig{
		MinVersion: "1.3",
		ACME: config.ACMEConfig{
			Enabled: true,
			Domains: []string{"example.com"},
		},
	}
	tc, err := ServerTLSConfig(cfg)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	if tc.GetCertificate == nil {
		t.Fatal("expected autocert GetCertificate")
	}
	if tc.MinVersion != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %d, want TLS1.3", tc.MinVersion)
	}
}

// ACMEHTTPHandler returns nil when ACME is disabled and a handler when enabled.
func TestACMEHTTPHandler(t *testing.T) {
	if h := ACMEHTTPHandler(config.TLSConfig{}); h != nil {
		t.Fatal("expected nil handler when ACME disabled")
	}
	h := ACMEHTTPHandler(config.TLSConfig{
		ACME: config.ACMEConfig{Enabled: true, Domains: []string{"example.com"}},
	})
	if h == nil {
		t.Fatal("expected non-nil handler when ACME enabled")
	}
}

// NewACMEManager must error when enabled with no domains.
func TestACMERequiresDomains(t *testing.T) {
	_, _, err := NewACMEManager(config.TLSConfig{
		ACME: config.ACMEConfig{Enabled: true},
	})
	if err == nil {
		t.Fatal("expected error when ACME enabled with no domains")
	}
}

// The manager's HostPolicy must politely reject a non-whitelisted host without
// any network access, and accept a whitelisted host. GetCertificate for a
// non-whitelisted SNI must error rather than attempt issuance for it.
func TestACMEHostWhitelistRejectsUnlistedSNI(t *testing.T) {
	cfg := config.TLSConfig{
		ACME: config.ACMEConfig{
			Enabled:      true,
			Domains:      []string{"allowed.example.com"},
			DirectoryURL: "https://127.0.0.1:1/directory",
		},
	}
	am, err := newACMEManager(cfg)
	if err != nil {
		t.Fatalf("newACMEManager: %v", err)
	}

	ctx := context.Background()
	if err := am.Manager.HostPolicy(ctx, "evil.example.com"); err == nil {
		t.Fatal("expected HostPolicy to reject non-whitelisted host")
	}
	if err := am.Manager.HostPolicy(ctx, "allowed.example.com"); err != nil {
		t.Fatalf("HostPolicy rejected whitelisted host: %v", err)
	}

	if _, err := am.Manager.GetCertificate(&tls.ClientHelloInfo{ServerName: "evil.example.com"}); err == nil {
		t.Fatal("expected GetCertificate to reject non-whitelisted SNI")
	}
}

// An empty CacheDir must select the in-memory cache, which must round-trip via
// the autocert.Cache interface.
func TestACMEInMemoryCache(t *testing.T) {
	am, err := newACMEManager(config.TLSConfig{
		ACME: config.ACMEConfig{Enabled: true, Domains: []string{"example.com"}},
	})
	if err != nil {
		t.Fatalf("newACMEManager: %v", err)
	}
	mc, ok := am.Manager.Cache.(*memCache)
	if !ok {
		t.Fatalf("expected *memCache, got %T", am.Manager.Cache)
	}
	ctx := context.Background()
	if _, err := mc.Get(ctx, "missing"); err == nil {
		t.Fatal("expected ErrCacheMiss for missing key")
	}
	if err := mc.Put(ctx, "k", []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := mc.Get(ctx, "k")
	if err != nil || string(got) != "v" {
		t.Fatalf("Get = %q, %v; want v", got, err)
	}
	if err := mc.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := mc.Get(ctx, "k"); err == nil {
		t.Fatal("expected miss after Delete")
	}
}

// ---------------------------------------------------------------------------
// OCSP stapling tests (6.11)
// ---------------------------------------------------------------------------

// ocspFixture is a CA + issued leaf, with the responder URL baked into the
// leaf, used to drive the Stapler.
type ocspFixture struct {
	issuer    *x509.Certificate
	issuerKey *rsa.PrivateKey
	leaf      *x509.Certificate
	tlsCert   *tls.Certificate
	responder *httptest.Server
}

// newOCSPFixture builds a self-signed CA and a leaf signed by it. The leaf's
// OCSPServer points at an httptest responder that returns a freshly signed
// "good" OCSP response with the given nextUpdate.
func newOCSPFixture(t *testing.T, nextUpdate time.Time) *ocspFixture {
	t.Helper()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}

	f := &ocspFixture{issuer: caCert, issuerKey: caKey}

	// Responder must exist before the leaf so we can bake its URL in.
	f.responder = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		tmpl := ocsp.Response{
			Status:       ocsp.Good,
			SerialNumber: f.leaf.SerialNumber,
			ThisUpdate:   time.Now().Add(-time.Minute),
			NextUpdate:   nextUpdate,
		}
		der, err := ocsp.CreateResponse(f.issuer, f.issuer, tmpl, f.issuerKey)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/ocsp-response")
		w.Write(der)
	}))
	t.Cleanup(f.responder.Close)

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: "leaf.example.com"},
		DNSNames:     []string{"leaf.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(12 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		OCSPServer:   []string{f.responder.URL},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	f.leaf = leaf
	f.tlsCert = &tls.Certificate{
		Certificate: [][]byte{leafDER, caDER},
		PrivateKey:  leafKey,
		Leaf:        leaf,
	}
	return f
}

// RefreshOnce must fetch a good OCSP response and populate OCSPStaple.
func TestStaplerPopulatesStaple(t *testing.T) {
	f := newOCSPFixture(t, time.Now().Add(time.Hour))

	s := NewStapler([]*tls.Certificate{f.tlsCert})
	s.HTTPClient = f.responder.Client()

	if err := s.RefreshOnce(context.Background()); err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}
	if len(f.tlsCert.OCSPStaple) == 0 {
		t.Fatal("expected OCSPStaple to be populated")
	}
	parsed, err := ocsp.ParseResponseForCert(f.tlsCert.OCSPStaple, f.leaf, f.issuer)
	if err != nil {
		t.Fatalf("parse staple: %v", err)
	}
	if parsed.Status != ocsp.Good {
		t.Fatalf("staple status = %d, want good", parsed.Status)
	}
}

// Start must populate the staple synchronously and the background loop must run
// without panicking; Stop must return promptly and be idempotent.
func TestStaplerStartStop(t *testing.T) {
	f := newOCSPFixture(t, time.Now().Add(time.Hour))

	s := NewStapler([]*tls.Certificate{f.tlsCert})
	s.HTTPClient = f.responder.Client()
	s.MinRefresh = 10 * time.Millisecond
	fixed := time.Now()
	s.Now = func() time.Time { return fixed }

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(f.tlsCert.OCSPStaple) == 0 {
		t.Fatal("expected staple populated after Start")
	}

	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return promptly")
	}
	s.Stop() // idempotent
}

// A certificate without an OCSP responder URL must be skipped, so the Stapler
// has no targets and Start is a no-op.
func TestStaplerSkipsCertsWithoutResponder(t *testing.T) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(7),
		Subject:               pkix.Name{CommonName: "no-ocsp"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	cert := &tls.Certificate{Certificate: [][]byte{der}}

	s := NewStapler([]*tls.Certificate{cert})
	if len(s.targets) != 0 {
		t.Fatalf("expected 0 targets, got %d", len(s.targets))
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start with no targets: %v", err)
	}
	s.Stop()
}

// nextRefresh must aim for roughly half the remaining time and honor the floor.
func TestStaplerNextRefresh(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	s := &Stapler{Now: func() time.Time { return now }, MinRefresh: time.Minute}

	r := &ocsp.Response{NextUpdate: now.Add(2 * time.Hour)}
	if d := s.nextRefresh(r); d != time.Hour {
		t.Fatalf("nextRefresh = %v, want 1h", d)
	}
	r2 := &ocsp.Response{NextUpdate: now.Add(30 * time.Second)}
	if d := s.nextRefresh(r2); d != time.Minute {
		t.Fatalf("nextRefresh = %v, want floor 1m", d)
	}
	r3 := &ocsp.Response{NextUpdate: now.Add(-time.Hour)}
	if d := s.nextRefresh(r3); d != time.Minute {
		t.Fatalf("nextRefresh = %v, want floor 1m", d)
	}
	if d := s.nextRefresh(&ocsp.Response{}); d != time.Minute {
		t.Fatalf("nextRefresh = %v, want floor 1m", d)
	}
}

// The Stapler must not staple a non-good (revoked) response and must report an
// error from refresh.
func TestStaplerDoesNotStapleRevoked(t *testing.T) {
	f := newOCSPFixture(t, time.Now().Add(time.Hour))
	// Swap in a responder that returns revoked.
	f.responder.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tmpl := ocsp.Response{
			Status:           ocsp.Revoked,
			SerialNumber:     f.leaf.SerialNumber,
			ThisUpdate:       time.Now().Add(-time.Minute),
			NextUpdate:       time.Now().Add(time.Hour),
			RevokedAt:        time.Now().Add(-time.Minute),
			RevocationReason: ocsp.Unspecified,
		}
		der, err := ocsp.CreateResponse(f.issuer, f.issuer, tmpl, f.issuerKey)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/ocsp-response")
		w.Write(der)
	})

	s := NewStapler([]*tls.Certificate{f.tlsCert})
	s.HTTPClient = f.responder.Client()

	if err := s.RefreshOnce(context.Background()); err == nil {
		t.Fatal("expected error for revoked OCSP status")
	}
	if len(f.tlsCert.OCSPStaple) != 0 {
		t.Fatal("must not staple a revoked response")
	}
}

// The staple write path must be safe under concurrent RefreshOnce calls (run
// with -race for full coverage).
func TestStaplerConcurrentRefresh(t *testing.T) {
	f := newOCSPFixture(t, time.Now().Add(time.Hour))
	s := NewStapler([]*tls.Certificate{f.tlsCert})
	s.HTTPClient = f.responder.Client()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.RefreshOnce(context.Background())
		}()
	}
	wg.Wait()
	if len(f.tlsCert.OCSPStaple) == 0 {
		t.Fatal("expected staple populated after concurrent refresh")
	}
}
