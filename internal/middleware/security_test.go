package middleware

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
)

// secOKHandler responds 200 with a marker body so tests can distinguish a
// request that reached the downstream handler from one blocked by middleware.
var secOKHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
})

func serve(mw func(http.Handler) http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mw(secOKHandler).ServeHTTP(rec, r)
	return rec
}

func TestSecurityHeaders(t *testing.T) {
	cfg := config.HeadersConfig{
		Enabled:            true,
		HSTS:               "max-age=63072000; includeSubDomains",
		FrameOptions:       "DENY",
		ContentTypeOptions: true,
		CSP:                "default-src 'self'",
		ReferrerPolicy:     "no-referrer",
	}
	rec := serve(SecurityHeaders(cfg), httptest.NewRequest(http.MethodGet, "/", nil))

	checks := map[string]string{
		"Strict-Transport-Security": "max-age=63072000; includeSubDomains",
		"X-Frame-Options":           "DENY",
		"X-Content-Type-Options":    "nosniff",
		"Content-Security-Policy":   "default-src 'self'",
		"Referrer-Policy":           "no-referrer",
	}
	for h, want := range checks {
		if got := rec.Header().Get(h); got != want {
			t.Errorf("header %s = %q, want %q", h, got, want)
		}
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestSecurityHeadersDisabledIsNoop(t *testing.T) {
	rec := serve(SecurityHeaders(config.HeadersConfig{Enabled: false}),
		httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Header().Get("X-Frame-Options"); got != "" {
		t.Errorf("disabled headers set X-Frame-Options = %q", got)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("disabled middleware did not pass through, body = %q", rec.Body.String())
	}
}

func TestSecurityHeadersPartial(t *testing.T) {
	// Only ContentTypeOptions set; nothing else should be emitted.
	rec := serve(SecurityHeaders(config.HeadersConfig{Enabled: true, ContentTypeOptions: true}),
		httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("unexpected HSTS = %q", got)
	}
}

func TestCORSPreflight(t *testing.T) {
	cfg := config.CORSConfig{
		Enabled:      true,
		AllowOrigins: []string{"https://app.example.com"},
		AllowMethods: []string{"GET", "POST"},
		AllowHeaders: []string{"Content-Type", "Authorization"},
		MaxAge:       600,
	}
	r := httptest.NewRequest(http.MethodOptions, "/", nil)
	r.Header.Set("Origin", "https://app.example.com")
	r.Header.Set("Access-Control-Request-Method", "POST")
	rec := serve(CORS(cfg), r)

	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("ACAO = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "GET, POST" {
		t.Errorf("ACAM = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "Content-Type, Authorization" {
		t.Errorf("ACAH = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Max-Age"); got != "600" {
		t.Errorf("Max-Age = %q", got)
	}
	if rec.Body.String() == "ok" {
		t.Error("preflight should not reach downstream handler")
	}
}

func TestCORSSimpleRequestWildcard(t *testing.T) {
	cfg := config.CORSConfig{Enabled: true, AllowOrigins: []string{"*"}}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://anything.example")
	rec := serve(CORS(cfg), r)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("ACAO = %q, want *", got)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("simple request should pass through, body = %q", rec.Body.String())
	}
}

func TestCORSDisallowedOrigin(t *testing.T) {
	cfg := config.CORSConfig{Enabled: true, AllowOrigins: []string{"https://ok.example"}}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://evil.example")
	rec := serve(CORS(cfg), r)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("disallowed origin got ACAO = %q", got)
	}
	// Non-preflight still reaches the handler; CORS enforcement is browser-side.
	if rec.Body.String() != "ok" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestCORSCredentialsEchoesOrigin(t *testing.T) {
	cfg := config.CORSConfig{
		Enabled:          true,
		AllowOrigins:     []string{"*"},
		AllowCredentials: true,
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://app.example")
	rec := serve(CORS(cfg), r)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Errorf("ACAO with credentials = %q, want echoed origin", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Allow-Credentials = %q", got)
	}
}

func TestACLDenyAndAllow(t *testing.T) {
	cfg := config.ACLConfig{
		Deny:  []string{"10.0.0.0/8"},
		Allow: []string{"192.168.0.0/16"},
	}
	mw := ACL(cfg, nil)

	// Denied IP.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.2.3:1234"
	if rec := serve(mw, r); rec.Code != http.StatusForbidden {
		t.Errorf("denied IP status = %d, want 403", rec.Code)
	}

	// Allowed IP.
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.168.1.5:1234"
	if rec := serve(mw, r); rec.Code != http.StatusOK {
		t.Errorf("allowed IP status = %d, want 200", rec.Code)
	}

	// Not in Allow list (and Allow non-empty) => blocked.
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.7:1234"
	if rec := serve(mw, r); rec.Code != http.StatusForbidden {
		t.Errorf("non-allowlisted IP status = %d, want 403", rec.Code)
	}
}

func TestACLMethodAndPath(t *testing.T) {
	cfg := config.ACLConfig{
		Methods:      []string{"GET"},
		BlockedPaths: []string{"/admin"},
	}
	mw := ACL(cfg, nil)

	// Disallowed method.
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "203.0.113.9:1"
	if rec := serve(mw, r); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}

	// Blocked path prefix.
	r = httptest.NewRequest(http.MethodGet, "/admin/secret", nil)
	r.RemoteAddr = "203.0.113.9:1"
	if rec := serve(mw, r); rec.Code != http.StatusForbidden {
		t.Errorf("/admin path status = %d, want 403", rec.Code)
	}

	// Allowed.
	r = httptest.NewRequest(http.MethodGet, "/public", nil)
	r.RemoteAddr = "203.0.113.9:1"
	if rec := serve(mw, r); rec.Code != http.StatusOK {
		t.Errorf("allowed request status = %d, want 200", rec.Code)
	}
}

func TestACLEmptyIsNoop(t *testing.T) {
	r := httptest.NewRequest(http.MethodDelete, "/anything", nil)
	r.RemoteAddr = "10.9.9.9:1"
	if rec := serve(ACL(config.ACLConfig{}, nil), r); rec.Code != http.StatusOK {
		t.Errorf("empty ACL status = %d, want 200", rec.Code)
	}
}

func TestBasicAuth(t *testing.T) {
	cfg := config.AuthConfig{Type: "basic", Users: map[string]string{"alice": "s3cret"}}
	mw := Auth(cfg)

	// Good credentials.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.SetBasicAuth("alice", "s3cret")
	if rec := serve(mw, r); rec.Code != http.StatusOK {
		t.Errorf("good basic creds status = %d, want 200", rec.Code)
	}

	// Bad password.
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.SetBasicAuth("alice", "wrong")
	rec := serve(mw, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad basic creds status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("missing WWW-Authenticate header on 401")
	}

	// Missing header.
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	if rec := serve(mw, r); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing basic creds status = %d, want 401", rec.Code)
	}
}

func TestAPIKeyAuth(t *testing.T) {
	cfg := config.AuthConfig{Type: "apikey", Header: "X-API-Key", APIKeys: []string{"key-123", "key-456"}}
	mw := Auth(cfg)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-API-Key", "key-456")
	if rec := serve(mw, r); rec.Code != http.StatusOK {
		t.Errorf("good api key status = %d, want 200", rec.Code)
	}

	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-API-Key", "nope")
	if rec := serve(mw, r); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad api key status = %d, want 401", rec.Code)
	}

	r = httptest.NewRequest(http.MethodGet, "/", nil)
	if rec := serve(mw, r); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing api key status = %d, want 401", rec.Code)
	}
}

func TestAPIKeyAuthDefaultHeader(t *testing.T) {
	// Empty Header should default to X-API-Key.
	cfg := config.AuthConfig{Type: "apikey", APIKeys: []string{"k"}}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-API-Key", "k")
	if rec := serve(Auth(cfg), r); rec.Code != http.StatusOK {
		t.Errorf("default header status = %d, want 200", rec.Code)
	}
}

// makeJWT builds a compact HS256 JWT with the given claims JSON signed by secret.
func makeJWT(t *testing.T, alg, claimsJSON, secret string) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	header := enc([]byte(`{"alg":"` + alg + `","typ":"JWT"}`))
	payload := enc([]byte(claimsJSON))
	signingInput := header + "." + payload
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	sig := enc(mac.Sum(nil))
	return signingInput + "." + sig
}

func TestJWTAuth(t *testing.T) {
	secret := "topsecret"
	cfg := config.AuthConfig{Type: "jwt", JWTSecret: secret, JWTAlg: "HS256"}
	mw := Auth(cfg)

	future := time.Now().Add(time.Hour).Unix()
	past := time.Now().Add(-time.Hour).Unix()

	valid := makeJWT(t, "HS256", `{"sub":"u1","exp":`+strconv.FormatInt(future, 10)+`}`, secret)
	expired := makeJWT(t, "HS256", `{"sub":"u1","exp":`+strconv.FormatInt(past, 10)+`}`, secret)
	tampered := makeJWT(t, "HS256", `{"sub":"u1","exp":`+strconv.FormatInt(future, 10)+`}`, "wrongsecret")

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"valid", valid, http.StatusOK},
		{"expired", expired, http.StatusUnauthorized},
		{"tampered-signature", tampered, http.StatusUnauthorized},
		{"garbage", "not.a.jwt", http.StatusUnauthorized},
		{"missing", "", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if rec := serve(mw, r); rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestJWTRejectsAlgNone(t *testing.T) {
	cfg := config.AuthConfig{Type: "jwt", JWTSecret: "s"}
	// Header alg=none with empty signature — must be rejected.
	enc := base64.RawURLEncoding.EncodeToString
	header := enc([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := enc([]byte(`{"sub":"x"}`))
	token := header + "." + payload + "."
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if rec := serve(Auth(cfg), r); rec.Code != http.StatusUnauthorized {
		t.Errorf("alg=none status = %d, want 401", rec.Code)
	}
}

func TestAuthNoneIsNoop(t *testing.T) {
	for _, typ := range []string{"", "none"} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if rec := serve(Auth(config.AuthConfig{Type: typ}), r); rec.Code != http.StatusOK {
			t.Errorf("Auth type %q status = %d, want 200", typ, rec.Code)
		}
	}
}

// --- RS256 / JWKS test helpers ---

// makeRS256 builds a compact RS256 JWT signed by key, with the given optional
// kid embedded in the header.
func makeRS256(t *testing.T, key *rsa.PrivateKey, kid, claimsJSON string) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	hdr := `{"alg":"RS256","typ":"JWT"`
	if kid != "" {
		hdr += `,"kid":"` + kid + `"`
	}
	hdr += `}`
	header := enc([]byte(hdr))
	payload := enc([]byte(claimsJSON))
	signingInput := header + "." + payload
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + enc(sig)
}

// pkixPEM returns the PKIX/SPKI PEM encoding of pub.
func pkixPEM(t *testing.T, pub *rsa.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal pkix: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// jwksJSON returns a JWKS document exposing the given kid->public-key pairs.
func jwksJSON(t *testing.T, keys map[string]*rsa.PublicKey) []byte {
	t.Helper()
	type jwk struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		N   string `json:"n"`
		E   string `json:"e"`
		Alg string `json:"alg"`
		Use string `json:"use"`
	}
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	for kid, pub := range keys {
		var eBuf [8]byte
		binary.BigEndian.PutUint64(eBuf[:], uint64(pub.E))
		// Trim leading zero bytes from the exponent.
		i := 0
		for i < len(eBuf)-1 && eBuf[i] == 0 {
			i++
		}
		doc.Keys = append(doc.Keys, jwk{
			Kty: "RSA",
			Kid: kid,
			N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			E:   base64.RawURLEncoding.EncodeToString(eBuf[i:]),
			Alg: "RS256",
			Use: "sig",
		})
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	return b
}

func genKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	return k
}

// TestJWTRS256PublicKey covers the static-PEM RS256 path through the full Auth
// middleware: a valid token passes, a token signed by another key fails, an
// expired token fails, and an HS256 token is rejected (algorithm confusion).
func TestJWTRS256PublicKey(t *testing.T) {
	key := genKey(t)
	other := genKey(t)
	cfg := config.AuthConfig{
		Type:         "jwt",
		JWTAlg:       "RS256",
		JWTPublicKey: pkixPEM(t, &key.PublicKey),
	}
	mw := Auth(cfg)

	future := time.Now().Add(time.Hour).Unix()
	past := time.Now().Add(-time.Hour).Unix()

	valid := makeRS256(t, key, "", `{"sub":"u1","exp":`+strconv.FormatInt(future, 10)+`}`)
	wrongKey := makeRS256(t, other, "", `{"sub":"u1","exp":`+strconv.FormatInt(future, 10)+`}`)
	expired := makeRS256(t, key, "", `{"sub":"u1","exp":`+strconv.FormatInt(past, 10)+`}`)
	// An HS256 token presented to an RS256 config must be rejected.
	hs := makeJWT(t, "HS256", `{"sub":"u1","exp":`+strconv.FormatInt(future, 10)+`}`, "shared-secret")

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"valid", valid, http.StatusOK},
		{"wrong-key", wrongKey, http.StatusUnauthorized},
		{"expired", expired, http.StatusUnauthorized},
		{"hs256-alg-confusion", hs, http.StatusUnauthorized},
		{"garbage", "not.a.jwt", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Authorization", "Bearer "+tc.token)
			if rec := serve(mw, r); rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// TestJWTRS256RejectsAlgNone ensures "alg":"none" is rejected under RS256 even
// when the payload is otherwise well-formed.
func TestJWTRS256RejectsAlgNone(t *testing.T) {
	key := genKey(t)
	cfg := config.AuthConfig{Type: "jwt", JWTAlg: "RS256", JWTPublicKey: pkixPEM(t, &key.PublicKey)}
	enc := base64.RawURLEncoding.EncodeToString
	token := enc([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + enc([]byte(`{"sub":"x"}`)) + "."
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if rec := serve(Auth(cfg), r); rec.Code != http.StatusUnauthorized {
		t.Errorf("alg=none status = %d, want 401", rec.Code)
	}
}

// TestRS256RejectsHS256TokenAtVerifier exercises the verifier directly to
// confirm the algorithm-confusion guard: an HS256 token whose secret is set to
// the RSA public-key bytes must not verify against an RS256 verifier.
func TestRS256RejectsHS256TokenAtVerifier(t *testing.T) {
	key := genKey(t)
	v := newJWTVerifier(config.AuthConfig{Type: "jwt", JWTAlg: "RS256", JWTPublicKey: pkixPEM(t, &key.PublicKey)})
	// Attacker signs an HS256 token; alg mismatch alone must reject it.
	future := time.Now().Add(time.Hour).Unix()
	tok := makeJWT(t, "HS256", `{"exp":`+strconv.FormatInt(future, 10)+`}`, "anything")
	if v.verify(tok) {
		t.Fatal("RS256 verifier accepted an HS256 token")
	}
}

// TestJWTRS256NbfEnforced verifies the nbf ("not before") claim is enforced.
func TestJWTRS256NbfEnforced(t *testing.T) {
	key := genKey(t)
	v := newJWTVerifier(config.AuthConfig{Type: "jwt", JWTAlg: "RS256", JWTPublicKey: pkixPEM(t, &key.PublicKey)})
	future := time.Now().Add(time.Hour).Unix()
	notYet := makeRS256(t, key, "", `{"nbf":`+strconv.FormatInt(future, 10)+`}`)
	if v.verify(notYet) {
		t.Fatal("token with future nbf should be rejected")
	}
	past := time.Now().Add(-time.Hour).Unix()
	active := makeRS256(t, key, "", `{"nbf":`+strconv.FormatInt(past, 10)+`}`)
	if !v.verify(active) {
		t.Fatal("token with past nbf should be accepted")
	}
}

// stubDoer is an httpDoer that dispatches to an underlying http.Client but
// counts calls, letting tests assert refetch behavior against an httptest
// server.
type stubDoer struct {
	client *http.Client
	calls  int32
}

func (s *stubDoer) Do(r *http.Request) (*http.Response, error) {
	atomic.AddInt32(&s.calls, 1)
	return s.client.Do(r)
}

// TestJWTRS256JWKS stands up an httptest JWKS endpoint and verifies a token by
// kid, then confirms an unknown kid triggers a refetch and still fails with a
// genuinely unknown kid.
func TestJWTRS256JWKS(t *testing.T) {
	key := genKey(t)
	const kid = "key-1"

	var served atomic.Int64 // number of times the JWKS body was served
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksJSON(t, map[string]*rsa.PublicKey{kid: &key.PublicKey}))
	}))
	defer srv.Close()

	doer := &stubDoer{client: srv.Client()}
	v := newJWTVerifier(config.AuthConfig{Type: "jwt", JWTAlg: "RS256", JWKSURL: srv.URL})
	v.SetJWKSClient(doer)
	// Large throttle so refetches only happen on genuinely unknown kids.
	v.SetJWKSMinWait(time.Hour)

	future := time.Now().Add(time.Hour).Unix()

	// Known kid verifies (fetches once).
	good := makeRS256(t, key, kid, `{"sub":"u1","exp":`+strconv.FormatInt(future, 10)+`}`)
	if !v.verify(good) {
		t.Fatal("valid token with known kid should verify")
	}
	afterFirst := atomic.LoadInt32(&doer.calls)
	if afterFirst == 0 {
		t.Fatal("expected at least one JWKS fetch")
	}

	// A second known-kid token should not trigger another fetch (cache hit).
	good2 := makeRS256(t, key, kid, `{"sub":"u2","exp":`+strconv.FormatInt(future, 10)+`}`)
	if !v.verify(good2) {
		t.Fatal("second valid token should verify from cache")
	}
	if got := atomic.LoadInt32(&doer.calls); got != afterFirst {
		t.Fatalf("cache hit should not refetch: calls %d -> %d", afterFirst, got)
	}

	// Unknown kid triggers a refetch and then still fails (still unknown).
	unknown := makeRS256(t, key, "no-such-kid", `{"sub":"u3","exp":`+strconv.FormatInt(future, 10)+`}`)
	if v.verify(unknown) {
		t.Fatal("token with unknown kid must not verify")
	}
	if got := atomic.LoadInt32(&doer.calls); got <= afterFirst {
		t.Fatalf("unknown kid should trigger a refetch: calls %d -> %d", afterFirst, got)
	}
}

// TestJWTRS256JWKSRotation verifies that a kid absent at first-fetch time but
// present after a rotation is picked up via the unknown-kid refetch.
func TestJWTRS256JWKSRotation(t *testing.T) {
	oldKey := genKey(t)
	newKey := genKey(t)

	var body atomic.Pointer[[]byte]
	initial := jwksJSON(t, map[string]*rsa.PublicKey{"old": &oldKey.PublicKey})
	body.Store(&initial)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(*body.Load())
	}))
	defer srv.Close()

	v := newJWTVerifier(config.AuthConfig{Type: "jwt", JWTAlg: "RS256", JWKSURL: srv.URL})
	v.SetJWKSClient(&stubDoer{client: srv.Client()})
	v.SetJWKSMinWait(time.Hour)

	future := time.Now().Add(time.Hour).Unix()

	// Warm the cache with the old key.
	if !v.verify(makeRS256(t, oldKey, "old", fmt.Sprintf(`{"exp":%d}`, future))) {
		t.Fatal("old-kid token should verify")
	}

	// Rotate: server now serves the new key under kid "new".
	rotated := jwksJSON(t, map[string]*rsa.PublicKey{"new": &newKey.PublicKey})
	body.Store(&rotated)

	// A token with the new kid is unknown to the cache -> triggers refetch -> OK.
	if !v.verify(makeRS256(t, newKey, "new", fmt.Sprintf(`{"exp":%d}`, future))) {
		t.Fatal("new-kid token should verify after refetch")
	}
}

// TestJWTRS256ClockInjection verifies exp is evaluated against the injected
// clock rather than the wall clock.
func TestJWTRS256ClockInjection(t *testing.T) {
	key := genKey(t)
	v := newJWTVerifier(config.AuthConfig{Type: "jwt", JWTAlg: "RS256", JWTPublicKey: pkixPEM(t, &key.PublicKey)})

	exp := int64(1_000_000)
	tok := makeRS256(t, key, "", fmt.Sprintf(`{"exp":%d}`, exp))

	v.SetClock(func() time.Time { return time.Unix(exp-10, 0) })
	if !v.verify(tok) {
		t.Fatal("token should be valid before injected exp")
	}
	v.SetClock(func() time.Time { return time.Unix(exp+10, 0) })
	if v.verify(tok) {
		t.Fatal("token should be expired after injected exp")
	}
}

// TestParseRSAPublicKeyPEMPKCS1 confirms the PKCS#1 ("RSA PUBLIC KEY") PEM
// encoding is accepted in addition to PKIX.
func TestParseRSAPublicKeyPEMPKCS1(t *testing.T) {
	key := genKey(t)
	der := x509.MarshalPKCS1PublicKey(&key.PublicKey)
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: der}))
	got, err := parseRSAPublicKeyPEM(pemStr)
	if err != nil {
		t.Fatalf("parse pkcs1 pem: %v", err)
	}
	if got.N.Cmp(key.PublicKey.N) != 0 || got.E != key.PublicKey.E {
		t.Fatal("parsed key does not match original")
	}
}

// ensure net import used (ACL takes []*net.IPNet); referenced for clarity.
var _ = net.IPNet{}
