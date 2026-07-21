package middleware

import (
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"reverse-proxy-lb/internal/config"
	"reverse-proxy-lb/internal/netutil"
)

// errJWKS is a sentinel returned by the JWKS/PEM parsers when key material is
// malformed or of an unsupported type.
var errJWKS = errors.New("invalid RSA key material")

// SecurityHeaders returns middleware that sets common security response headers
// according to cfg. When cfg.Enabled is false it is a no-op passthrough. Each
// individual header is only set when its corresponding config value is
// non-empty (or, for X-Content-Type-Options, when ContentTypeOptions is true),
// so operators can opt into headers piecemeal.
//
// Headers are written before the response is served so that they are present on
// every response regardless of the downstream handler's behavior.
func SecurityHeaders(cfg config.HeadersConfig) func(http.Handler) http.Handler {
	if !cfg.Enabled {
		return passthrough
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			if cfg.HSTS != "" {
				h.Set("Strict-Transport-Security", cfg.HSTS)
			}
			if cfg.FrameOptions != "" {
				h.Set("X-Frame-Options", cfg.FrameOptions)
			}
			if cfg.ContentTypeOptions {
				h.Set("X-Content-Type-Options", "nosniff")
			}
			if cfg.CSP != "" {
				h.Set("Content-Security-Policy", cfg.CSP)
			}
			if cfg.ReferrerPolicy != "" {
				h.Set("Referrer-Policy", cfg.ReferrerPolicy)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CORS returns middleware implementing Cross-Origin Resource Sharing per cfg.
// When cfg.Enabled is false it is a no-op passthrough.
//
// For actual (non-preflight) requests carrying an Origin header, it sets the
// Access-Control-Allow-Origin (and, when configured, Allow-Credentials) headers
// when the origin is permitted. Preflight OPTIONS requests carrying
// Access-Control-Request-Method are answered directly with 204 No Content and
// the negotiated Allow-Methods/Allow-Headers/Max-Age headers, without invoking
// the downstream handler.
//
// AllowOrigins may contain "*" to allow any origin, or exact origin strings. If
// AllowCredentials is set the concrete request origin is echoed back (a bare "*"
// is not valid with credentials).
func CORS(cfg config.CORSConfig) func(http.Handler) http.Handler {
	if !cfg.Enabled {
		return passthrough
	}
	allowMethods := strings.Join(cfg.AllowMethods, ", ")
	allowHeaders := strings.Join(cfg.AllowHeaders, ", ")
	maxAge := ""
	if cfg.MaxAge > 0 {
		maxAge = strconv.Itoa(cfg.MaxAge)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				// Not a CORS request.
				next.ServeHTTP(w, r)
				return
			}

			allowed, ok := originMatch(cfg, origin)
			if ok {
				w.Header().Set("Access-Control-Allow-Origin", allowed)
				if allowed != "*" {
					// Vary on Origin whenever the response depends on it.
					w.Header().Add("Vary", "Origin")
				}
				if cfg.AllowCredentials {
					w.Header().Set("Access-Control-Allow-Credentials", "true")
				}
			}

			// Preflight.
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				if ok {
					if allowMethods != "" {
						w.Header().Set("Access-Control-Allow-Methods", allowMethods)
					}
					if allowHeaders != "" {
						w.Header().Set("Access-Control-Allow-Headers", allowHeaders)
					} else if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
						w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
					}
					if maxAge != "" {
						w.Header().Set("Access-Control-Max-Age", maxAge)
					}
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// originMatch reports the value to use for Access-Control-Allow-Origin and
// whether the origin is permitted. With AllowCredentials, "*" echoes the
// concrete origin (since "*" is invalid alongside credentials).
func originMatch(cfg config.CORSConfig, origin string) (string, bool) {
	for _, o := range cfg.AllowOrigins {
		if o == "*" {
			if cfg.AllowCredentials {
				return origin, true
			}
			return "*", true
		}
		if o == origin {
			return origin, true
		}
	}
	return "", false
}

// ACL returns middleware enforcing network- and request-level access control
// per cfg. trusted is the set of trusted proxy networks used to resolve the
// real client IP via netutil.ClientIP.
//
// Enforcement order for each request:
//   - Deny: if the client IP matches any Deny CIDR, respond 403.
//   - Allow: if Allow is non-empty and the client IP matches none of it, 403.
//   - Methods: if Methods is non-empty and the request method is not listed,
//     respond 405 Method Not Allowed.
//   - BlockedPaths: if the request path has any BlockedPaths entry as a prefix,
//     respond 403.
//
// When every rule set is empty the middleware is a no-op passthrough.
func ACL(cfg config.ACLConfig, trusted []*net.IPNet) func(http.Handler) http.Handler {
	if len(cfg.Allow) == 0 && len(cfg.Deny) == 0 && len(cfg.Methods) == 0 && len(cfg.BlockedPaths) == 0 {
		return passthrough
	}
	allow := netutil.ParseCIDRs(cfg.Allow)
	deny := netutil.ParseCIDRs(cfg.Deny)
	methods := make(map[string]struct{}, len(cfg.Methods))
	for _, m := range cfg.Methods {
		methods[strings.ToUpper(strings.TrimSpace(m))] = struct{}{}
	}
	blocked := make([]string, 0, len(cfg.BlockedPaths))
	for _, p := range cfg.BlockedPaths {
		if p = strings.TrimSpace(p); p != "" {
			blocked = append(blocked, p)
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := net.ParseIP(netutil.ClientIP(r, trusted))

			if len(deny) > 0 && ipMatch(ip, deny) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if len(allow) > 0 && !ipMatch(ip, allow) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if len(methods) > 0 {
				if _, ok := methods[strings.ToUpper(r.Method)]; !ok {
					http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
					return
				}
			}
			for _, p := range blocked {
				if strings.HasPrefix(r.URL.Path, p) {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func ipMatch(ip net.IP, nets []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Auth returns middleware enforcing request authentication per cfg.Type:
//
//   - "none" (or empty): no-op passthrough.
//   - "basic": require an Authorization: Basic header whose user/password match
//     an entry in cfg.Users (password compared in constant time). On failure a
//     401 with WWW-Authenticate: Basic is returned.
//   - "apikey": require a key present in cfg.APIKeys, supplied in the header
//     named by cfg.Header (default "X-API-Key"). 401 on failure.
//   - "jwt": require an Authorization: Bearer <token> whose signature verifies
//     and whose exp claim (and nbf, if present) is satisfied. When cfg.JWTAlg is
//     "HS256" (default) the HMAC signature is checked against cfg.JWTSecret; when
//     it is "RS256" the RSA signature is checked against a public key sourced
//     from cfg.JWTPublicKey (PEM) or fetched from cfg.JWKSURL (selected by the
//     token's "kid"). The token header's alg must equal the configured alg, so
//     an HS256 token is rejected under an RS256 config and vice versa; "none" is
//     always rejected. 401 on failure.
//
// All string comparisons that gate access use constant-time comparison to avoid
// leaking secrets through timing.
func Auth(cfg config.AuthConfig) func(http.Handler) http.Handler {
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case "basic":
		return basicAuth(cfg)
	case "apikey":
		return apiKeyAuth(cfg)
	case "jwt":
		return jwtAuth(cfg)
	default:
		return passthrough
	}
}

func basicAuth(cfg config.AuthConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := r.BasicAuth()
			if ok {
				if want, found := cfg.Users[user]; found &&
					subtle.ConstantTimeCompare([]byte(pass), []byte(want)) == 1 {
					next.ServeHTTP(w, r)
					return
				}
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="restricted"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		})
	}
}

func apiKeyAuth(cfg config.AuthConfig) func(http.Handler) http.Handler {
	header := cfg.Header
	if header == "" {
		header = "X-API-Key"
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get(header)
			if got != "" && keyMatch(got, cfg.APIKeys) {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		})
	}
}

// keyMatch reports whether got equals any key in keys, comparing every entry in
// constant time to avoid early-exit timing leaks.
func keyMatch(got string, keys []string) bool {
	match := false
	for _, k := range keys {
		if subtle.ConstantTimeCompare([]byte(got), []byte(k)) == 1 {
			match = true
		}
	}
	return match
}

func jwtAuth(cfg config.AuthConfig) func(http.Handler) http.Handler {
	v := newJWTVerifier(cfg)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if ok && v.verify(token) {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		})
	}
}

// httpDoer is the subset of *http.Client used to fetch JWKS documents; it is an
// interface so tests can inject a stub transport.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// jwtVerifier verifies compact JWS tokens for a single AuthConfig. It supports
// HS256 (HMAC) and RS256 (RSA), sourcing RS256 keys either from a static PEM
// public key or from a JWKS endpoint (keyed by "kid" and cached with a bounded
// refresh). The HTTP client and clock are injectable to make JWKS and expiry
// behavior testable.
type jwtVerifier struct {
	alg    string // upper-cased configured alg: "HS256" or "RS256"
	secret []byte // HS256 HMAC secret

	// pubKey is the static RS256 public key parsed from JWTPublicKey (may be nil
	// when a JWKS URL is configured instead).
	pubKey *rsa.PublicKey

	// JWKS configuration/state (guarded by mu).
	jwksURL     string
	httpClient  httpDoer
	now         func() time.Time
	jwksMinWait time.Duration // minimum interval between JWKS refetches

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey // kid -> key
	lastFetch   time.Time
	jwksFetched bool // whether an initial fetch has completed
}

// newJWTVerifier builds a jwtVerifier from cfg, using production defaults for
// the HTTP client, clock, and refresh throttle. Tests may override those via the
// exported setters before first use.
func newJWTVerifier(cfg config.AuthConfig) *jwtVerifier {
	alg := strings.ToUpper(strings.TrimSpace(cfg.JWTAlg))
	if alg == "" {
		alg = "HS256"
	}
	v := &jwtVerifier{
		alg:         alg,
		secret:      []byte(cfg.JWTSecret),
		jwksURL:     strings.TrimSpace(cfg.JWKSURL),
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		now:         time.Now,
		jwksMinWait: time.Minute,
		keys:        map[string]*rsa.PublicKey{},
	}
	if alg == "RS256" {
		if pem := strings.TrimSpace(cfg.JWTPublicKey); pem != "" {
			if k, err := parseRSAPublicKeyPEM(pem); err == nil {
				v.pubKey = k
			}
		}
	}
	return v
}

// SetJWKSClient overrides the HTTP client used to fetch JWKS documents. It is
// intended for tests; call before the verifier handles any request.
func (v *jwtVerifier) SetJWKSClient(c httpDoer) { v.httpClient = c }

// SetClock overrides the clock used for exp/nbf checks and JWKS refresh
// throttling. It is intended for tests.
func (v *jwtVerifier) SetClock(now func() time.Time) { v.now = now }

// SetJWKSMinWait overrides the minimum interval between JWKS refetches. It is
// intended for tests.
func (v *jwtVerifier) SetJWKSMinWait(d time.Duration) { v.jwksMinWait = d }

// verify reports whether token is a valid JWS under the verifier's configured
// algorithm and key material, and whether its temporal claims are satisfied.
func (v *jwtVerifier) verify(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &hdr); err != nil {
		return false
	}
	// Reject algorithm confusion: the header alg must match the configured alg
	// exactly (this also rejects "none").
	if !strings.EqualFold(hdr.Alg, v.alg) {
		return false
	}

	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}

	switch v.alg {
	case "HS256":
		mac := hmac.New(sha256.New, v.secret)
		mac.Write([]byte(signingInput))
		if !hmac.Equal(sig, mac.Sum(nil)) {
			return false
		}
	case "RS256":
		if !v.verifyRS256(signingInput, sig, hdr.Kid) {
			return false
		}
	default:
		return false
	}

	return v.claimsValid(parts[1])
}

// verifyRS256 checks an RSA-SHA256 signature over signingInput against the
// verifier's key material, selecting a JWKS key by kid when no static key is set.
func (v *jwtVerifier) verifyRS256(signingInput string, sig []byte, kid string) bool {
	digest := sha256.Sum256([]byte(signingInput))
	check := func(k *rsa.PublicKey) bool {
		return k != nil && rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig) == nil
	}
	if v.pubKey != nil {
		return check(v.pubKey)
	}
	if v.jwksURL == "" {
		return false
	}
	if k := v.jwksKey(kid, false); check(k) {
		return true
	}
	// Unknown kid (or stale key): refetch once, then retry.
	if k := v.jwksKey(kid, true); check(k) {
		return true
	}
	return false
}

// jwksKey returns the cached RSA key for kid, fetching/refreshing the JWKS
// document when the key is absent or forceRefresh is set. When kid is empty and
// exactly one key is cached, that key is returned. Refetches are throttled by
// jwksMinWait unless forceRefresh is set for a genuinely unknown kid.
func (v *jwtVerifier) jwksKey(kid string, forceRefresh bool) *rsa.PublicKey {
	v.mu.Lock()
	defer v.mu.Unlock()

	if !forceRefresh {
		if k := v.lookupLocked(kid); k != nil {
			return k
		}
	}

	// Decide whether to (re)fetch: always on the first use, on forceRefresh, or
	// once the throttle window has elapsed.
	needFetch := forceRefresh || !v.jwksFetched
	if !needFetch {
		if v.now().Sub(v.lastFetch) >= v.jwksMinWait {
			needFetch = true
		}
	}
	if needFetch {
		v.fetchLocked()
	}
	return v.lookupLocked(kid)
}

// lookupLocked returns the cached key for kid (or the sole key when kid is
// empty). Callers must hold v.mu.
func (v *jwtVerifier) lookupLocked(kid string) *rsa.PublicKey {
	if kid != "" {
		return v.keys[kid]
	}
	if len(v.keys) == 1 {
		for _, k := range v.keys {
			return k
		}
	}
	return nil
}

// fetchLocked fetches and parses the JWKS document, replacing the cache on
// success. Failures leave the previous cache intact. Callers must hold v.mu.
func (v *jwtVerifier) fetchLocked() {
	v.lastFetch = v.now()
	v.jwksFetched = true

	req, err := http.NewRequest(http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return
	}
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
			Alg string `json:"alg"`
			Use string `json:"use"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return
	}
	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if !strings.EqualFold(k.Kty, "RSA") {
			continue
		}
		pub, err := jwkToRSA(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) > 0 {
		v.keys = keys
	}
}

// claimsValid decodes the base64url payload and enforces exp (must be in the
// future) and nbf (must not be in the future) when present.
func (v *jwtVerifier) claimsValid(payloadSeg string) bool {
	payloadJSON, err := base64.RawURLEncoding.DecodeString(payloadSeg)
	if err != nil {
		return false
	}
	var claims struct {
		Exp *int64 `json:"exp"`
		Nbf *int64 `json:"nbf"`
	}
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return false
	}
	now := v.now().Unix()
	if claims.Exp != nil && now >= *claims.Exp {
		return false
	}
	if claims.Nbf != nil && now < *claims.Nbf {
		return false
	}
	return true
}

// parseRSAPublicKeyPEM parses a PEM-encoded RSA public key, accepting both
// PKIX/SPKI ("PUBLIC KEY") and PKCS#1 ("RSA PUBLIC KEY") encodings.
func parseRSAPublicKeyPEM(pemStr string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errJWKS
	}
	if key, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PublicKey); ok {
			return rsaKey, nil
		}
		return nil, errJWKS
	}
	if key, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, errJWKS
}

// jwkToRSA builds an RSA public key from the base64url modulus (n) and exponent
// (e) fields of a JWK.
func jwkToRSA(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(nB64, "="))
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(eB64, "="))
	if err != nil {
		return nil, err
	}
	if len(nBytes) == 0 || len(eBytes) == 0 {
		return nil, errJWKS
	}
	// Big-endian exponent, left-padded to 8 bytes for binary.BigEndian.
	var eBuf [8]byte
	copy(eBuf[8-len(eBytes):], eBytes)
	e := binary.BigEndian.Uint64(eBuf[:])
	if e == 0 {
		return nil, errJWKS
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(e), // #nosec G115 -- RSA public exponent always fits in int (typically 65537)
	}, nil
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

// passthrough is the identity middleware used when a security feature is
// disabled or has no configuration.
func passthrough(next http.Handler) http.Handler { return next }
