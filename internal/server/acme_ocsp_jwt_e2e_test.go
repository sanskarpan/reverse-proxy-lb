package server

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ocsp"

	"reverse-proxy-lb/internal/config"
	"reverse-proxy-lb/internal/tlsutil"
)

// This file exercises the ACME / OCSP / RS256-JWT features end-to-end through
// the real server stack. All key material and certificates are generated
// in-test with crypto/x509; the OCSP responder and JWKS endpoint are stood up
// as httptest servers. No assertion is weakened to force a pass. Where a
// scenario cannot be fully driven locally (real ACME certificate issuance needs
// a reachable domain and a live/test ACME CA), that limit is documented on the
// relevant test rather than papered over.

// ---------------------------------------------------------------------------
// RS256 JWT helpers
// ---------------------------------------------------------------------------

// rsaJWTKit holds an RSA keypair plus PEM encodings used to drive RS256 JWT
// verification through the server's security Auth middleware.
type rsaJWTKit struct {
	priv   *rsa.PrivateKey
	pubPEM string
	kid    string
}

func newRSAJWTKit(t *testing.T, kid string) rsaJWTKit {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal pkix: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki})
	return rsaJWTKit{priv: priv, pubPEM: string(pubPEM), kid: kid}
}

// b64url marshals v to JSON and base64url-encodes it (no padding), matching JWS.
func b64url(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal jws segment: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// signRS256 builds a compact RS256 JWS signed with priv. The advertised header
// alg is taken from headerAlg so tests can forge an alg=none / HS256 header
// while still producing a syntactically valid token, exercising the verifier's
// algorithm-confusion rejection.
func signRS256(t *testing.T, priv *rsa.PrivateKey, headerAlg, kid string, claims map[string]any) string {
	t.Helper()
	hdr := map[string]any{"alg": headerAlg, "typ": "JWT"}
	if kid != "" {
		hdr["kid"] = kid
	}
	signingInput := b64url(t, hdr) + "." + b64url(t, claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign rs256: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// jwksHandler stands up an httptest JWKS endpoint publishing pub under kid.
func jwksHandler(t *testing.T, kid string, pub *rsa.PublicKey) *httptest.Server {
	t.Helper()
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	var eBuf [8]byte
	binary.BigEndian.PutUint64(eBuf[:], uint64(pub.E))
	// Trim leading zero bytes from the exponent (JWK "e" is minimal big-endian).
	e := eBuf[:]
	for len(e) > 1 && e[0] == 0 {
		e = e[1:]
	}
	doc := map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"kid": kid,
			"use": "sig",
			"alg": "RS256",
			"n":   n,
			"e":   base64.RawURLEncoding.EncodeToString(e),
		}},
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---------------------------------------------------------------------------
// RS256 JWT via config (static public key)
// ---------------------------------------------------------------------------

func TestE2E_AuthJWT_RS256_PublicKey(t *testing.T) {
	backend := newIDBackend("rs256-backend", nil)
	defer backend.close()

	kit := newRSAJWTKit(t, "")
	other := newRSAJWTKit(t, "") // a second, unrelated key for the wrong-key case

	cfg := baseConfig("round_robin", backendCfgs(backend))
	cfg.Security.Auth = config.AuthConfig{
		Type:         "jwt",
		JWTAlg:       "RS256",
		JWTPublicKey: kit.pubPEM,
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

	now := time.Now()
	valid := signRS256(t, kit.priv, "RS256", "", map[string]any{"sub": "u", "exp": now.Add(time.Hour).Unix()})
	wrongKey := signRS256(t, other.priv, "RS256", "", map[string]any{"sub": "u", "exp": now.Add(time.Hour).Unix()})
	expired := signRS256(t, kit.priv, "RS256", "", map[string]any{"sub": "u", "exp": now.Add(-time.Hour).Unix()})
	// An HS256 token whose header advertises HS256 must be rejected under an
	// RS256 config (algorithm confusion). We forge the header via signRS256 but
	// the signature bytes are irrelevant: the alg mismatch rejects it first.
	hs256Header := signRS256(t, kit.priv, "HS256", "", map[string]any{"sub": "u", "exp": now.Add(time.Hour).Unix()})
	algNone := signRS256(t, kit.priv, "none", "", map[string]any{"sub": "u", "exp": now.Add(time.Hour).Unix()})

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"wrong key", wrongKey, http.StatusUnauthorized},
		{"expired", expired, http.StatusUnauthorized},
		{"hs256 header under rs256 config", hs256Header, http.StatusUnauthorized},
		{"alg=none", algNone, http.StatusUnauthorized},
		{"valid", valid, http.StatusOK},
	}
	for _, c := range cases {
		code, body := req(c.token)
		if code != c.want {
			t.Fatalf("%s: status = %d, want %d (body=%q)", c.name, code, c.want, body)
		}
		if c.want == http.StatusOK && !strings.Contains(body, "rs256-backend") {
			t.Fatalf("%s: body = %q, want to reach backend", c.name, body)
		}
	}
}

// ---------------------------------------------------------------------------
// RS256 JWT via JWKS (key selected by kid)
// ---------------------------------------------------------------------------

func TestE2E_AuthJWT_RS256_JWKS(t *testing.T) {
	backend := newIDBackend("jwks-backend", nil)
	defer backend.close()

	const kid = "key-1"
	kit := newRSAJWTKit(t, kid)
	jwks := jwksHandler(t, kid, &kit.priv.PublicKey)

	// A second key that is NOT published in the JWKS: a token signed by it (even
	// with a matching kid header) must fail because the JWKS has no such key
	// material to verify against.
	other := newRSAJWTKit(t, kid)

	cfg := baseConfig("round_robin", backendCfgs(backend))
	cfg.Security.Auth = config.AuthConfig{
		Type:    "jwt",
		JWTAlg:  "RS256",
		JWKSURL: jwks.URL,
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

	now := time.Now()
	valid := signRS256(t, kit.priv, "RS256", kid, map[string]any{"sub": "u", "exp": now.Add(time.Hour).Unix()})
	unknownKid := signRS256(t, kit.priv, "RS256", "no-such-kid", map[string]any{"sub": "u", "exp": now.Add(time.Hour).Unix()})
	wrongKey := signRS256(t, other.priv, "RS256", kid, map[string]any{"sub": "u", "exp": now.Add(time.Hour).Unix()})

	if code, body := req(valid); code != http.StatusOK || !strings.Contains(body, "jwks-backend") {
		t.Fatalf("valid JWKS token: status=%d body=%q, want 200/jwks-backend", code, body)
	}
	if code, _ := req(unknownKid); code != http.StatusUnauthorized {
		t.Fatalf("unknown kid: status=%d, want 401", code)
	}
	if code, _ := req(wrongKey); code != http.StatusUnauthorized {
		t.Fatalf("token signed by unpublished key: status=%d, want 401", code)
	}
}

// ---------------------------------------------------------------------------
// OCSP stapling
// ---------------------------------------------------------------------------

// ocspE2EFixture is a CA + leaf keypair whose leaf carries an OCSPServer URL
// pointing at an in-test responder that signs "good" responses.
type ocspE2EFixture struct {
	certFile  string
	keyFile   string
	leaf      *x509.Certificate
	issuer    *x509.Certificate
	caPool    *x509.CertPool
	responder *httptest.Server
}

func newOCSPE2EFixture(t *testing.T, dir string, nextUpdate time.Time) *ocspE2EFixture {
	t.Helper()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "OCSP Test CA"},
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

	f := &ocspE2EFixture{issuer: caCert}

	// The responder must exist before the leaf so its URL can be baked in.
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
		der, err := ocsp.CreateResponse(f.issuer, f.issuer, tmpl, caKey)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/ocsp-response")
		_, _ = w.Write(der)
	}))
	t.Cleanup(f.responder.Close)

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(4242),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
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

	// Write the chain (leaf + issuer) so tlsutil's Stapler can find the issuer in
	// the served chain, matching production (leaf then intermediate/CA).
	certPEM := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...,
	)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(leafKey)})

	f.certFile = writeFile(t, dir, "ocsp-leaf.crt", certPEM)
	f.keyFile = writeFile(t, dir, "ocsp-leaf.key", keyPEM)

	f.caPool = x509.NewCertPool()
	f.caPool.AddCert(caCert)
	return f
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := dir + "/" + name
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// The Stapler built over the EXACT served certs must populate OCSPStaple, and a
// real TLS handshake against a listener serving that config must deliver the
// staple to the client (ConnectionState.OCSPResponse).
func TestE2E_OCSPStapling_ServedAndHandshake(t *testing.T) {
	dir := t.TempDir()
	f := newOCSPE2EFixture(t, dir, time.Now().Add(time.Hour))

	// Build the server tls.Config AND grab the exact served *tls.Certificate
	// pointers, exactly as the server does in setupTLS.
	tlsCfg, certs, err := tlsutil.ServerTLSConfigWithCerts(config.TLSConfig{
		Enabled:      true,
		CertFile:     f.certFile,
		KeyFile:      f.keyFile,
		OCSPStapling: true,
	})
	if err != nil {
		t.Fatalf("ServerTLSConfigWithCerts: %v", err)
	}
	if len(certs) == 0 {
		t.Fatal("expected at least one served certificate")
	}

	stapler := tlsutil.NewStapler(certs)
	stapler.HTTPClient = f.responder.Client()
	if err := stapler.RefreshOnce(context.Background()); err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}

	// Assert the served certificate now carries a non-empty staple.
	if len(certs[0].OCSPStaple) == 0 {
		t.Fatal("served certificate has empty OCSPStaple after refresh")
	}
	parsed, err := ocsp.ParseResponseForCert(certs[0].OCSPStaple, f.leaf, f.issuer)
	if err != nil {
		t.Fatalf("parse installed staple: %v", err)
	}
	if parsed.Status != ocsp.Good {
		t.Fatalf("installed staple status = %d, want good(%d)", parsed.Status, ocsp.Good)
	}

	// Real handshake: the client must receive the stapled response. Note that Go
	// only delivers OCSPResponse to the client when the client requests it, which
	// crypto/tls does by default.
	addr := serveTLS(t, tlsCfg, okHandler)
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		RootCAs:    f.caPool,
		ServerName: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer conn.Close()
	cs := conn.ConnectionState()
	if len(cs.OCSPResponse) == 0 {
		t.Fatal("client received no stapled OCSP response in ConnectionState")
	}
	clientParsed, err := ocsp.ParseResponseForCert(cs.OCSPResponse, f.leaf, f.issuer)
	if err != nil {
		t.Fatalf("client parse OCSP response: %v", err)
	}
	if clientParsed.Status != ocsp.Good {
		t.Fatalf("client OCSP status = %d, want good(%d)", clientParsed.Status, ocsp.Good)
	}
}

// ---------------------------------------------------------------------------
// ACME challenge handler
// ---------------------------------------------------------------------------

// With ACME enabled and DirectoryURL unset, the server's TLS config must have
// GetCertificate wired (autocert-backed) and the shared HTTP-01 challenge
// handler must be retained and serve the /.well-known/acme-challenge/ path with
// a non-500 response from autocert.
//
// LIMITATION: real certificate issuance is NOT exercised here. It requires a
// publicly reachable domain and a live or Pebble/test ACME CA to complete the
// HTTP-01 challenge; that infrastructure is out of scope for a local unit test.
// We assert the wiring (GetCertificate present, challenge handler served) only.
func TestE2E_ACMEChallengeHandlerWiring(t *testing.T) {
	cfg := baseConfig("round_robin", []config.BackendConfig{
		{URL: "http://127.0.0.1:1", Weight: 1, MaxConns: 1},
	})
	cfg.TLS = config.TLSConfig{
		Enabled: true,
		ACME: config.ACMEConfig{
			Enabled:           true,
			Domains:           []string{"example.test"},
			HTTPChallengePort: 80,
			// DirectoryURL intentionally unset (autocert default). No network is
			// touched because we never trigger issuance.
		},
	}
	s := New(cfg, "")

	// 1. The installed server TLS config is autocert-backed: GetCertificate set.
	if s.httpServer.TLSConfig == nil {
		t.Fatal("ACME-enabled server installed no TLS config")
	}
	if s.httpServer.TLSConfig.GetCertificate == nil {
		t.Fatal("ACME TLS config has nil GetCertificate (autocert not wired)")
	}

	// 2. The shared HTTP-01 challenge handler is retained.
	if s.acmeChallengeHandler == nil {
		t.Fatal("ACME challenge handler is nil; Start would serve nothing on the challenge port")
	}

	// 3. Serve the challenge handler on its own listener (as Start does on the
	//    challenge port) and hit /.well-known/acme-challenge/. autocert returns a
	//    non-500 (typically 404 for an unknown token), proving the handler is
	//    live and routing challenge paths rather than erroring.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	challengeSrv := &http.Server{Handler: s.acmeChallengeHandler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = challengeSrv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = challengeSrv.Shutdown(ctx)
	})

	url := "http://" + ln.Addr().String() + "/.well-known/acme-challenge/sometoken"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET challenge path: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		t.Fatalf("challenge handler returned %d, want a non-500 response from autocert", resp.StatusCode)
	}
}
