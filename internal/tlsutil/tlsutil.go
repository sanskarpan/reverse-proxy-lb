// Package tlsutil builds *tls.Config values for the reverse proxy's TLS
// listener from a config.TLSConfig. It supports a configurable minimum TLS
// version, an explicit cipher-suite allowlist, SNI-based selection across
// multiple certificates, downstream client authentication (mTLS), optional
// hot-reload of certificate keypairs when the underlying files change on disk,
// automatic certificate management via ACME (autocert), and OCSP stapling.
//
// Hot-reload is implemented without any filesystem-watching dependency: the
// GetCertificate callback consults an mtime-keyed cache and transparently
// re-reads a keypair whenever its cert or key file mtime advances. Rotating the
// files therefore takes effect on the next TLS handshake, with no restart.
//
// ACME (opt-in via TLSConfig.ACME.Enabled) obtains and renews certificates for
// the configured Domains from an ACME CA, using an in-process autocert.Manager.
// The same Manager backs both the TLS GetCertificate callback and the HTTP-01
// challenge handler (see ACMEHTTPHandler / NewACMEManager), so they share
// account and challenge state. When ACME is enabled it takes precedence over
// the static cert_file/key_file path.
//
// OCSP stapling (opt-in via TLSConfig.OCSPStapling) fetches an OCSP response
// for each loaded leaf certificate that advertises a responder URL and staples
// it into the served tls.Certificate. A Stapler refreshes staples periodically
// ahead of each response's NextUpdate.
package tlsutil

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/crypto/ocsp"

	"reverse-proxy-lb/internal/config"
)

// goCipherSuites maps Go cipher-suite names to their IDs. It covers both the
// secure suites reported by tls.CipherSuites and the insecure/legacy ones from
// tls.InsecureCipherSuites, so validation and lookup agree on the same set of
// names the Go standard library recognizes.
var goCipherSuites = func() map[string]uint16 {
	m := make(map[string]uint16)
	for _, cs := range tls.CipherSuites() {
		m[cs.Name] = cs.ID
	}
	for _, cs := range tls.InsecureCipherSuites() {
		m[cs.Name] = cs.ID
	}
	return m
}()

// minVersionFor maps the config MinVersion string to a tls.Version* constant.
// The empty string and any unrecognized value default to TLS 1.2, matching the
// config Load() default.
func minVersionFor(v string) uint16 {
	switch v {
	case "1.3":
		return tls.VersionTLS13
	case "1.2", "":
		return tls.VersionTLS12
	default:
		return tls.VersionTLS12
	}
}

// cipherSuiteIDs resolves a list of Go cipher-suite names to their IDs. An
// unknown name is an error (config.validate() is expected to reject these too,
// but tlsutil does not assume it ran).
func cipherSuiteIDs(names []string) ([]uint16, error) {
	if len(names) == 0 {
		return nil, nil
	}
	ids := make([]uint16, 0, len(names))
	for _, n := range names {
		id, ok := goCipherSuites[n]
		if !ok {
			return nil, fmt.Errorf("tlsutil: unknown cipher suite %q", n)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// certEntry describes one keypair that participates in SNI selection.
type certEntry struct {
	certFile string
	keyFile  string
}

// loadedCert is a cached, parsed keypair together with the file mtimes it was
// loaded from, used to detect rotation for hot-reload.
type loadedCert struct {
	cert      *tls.Certificate
	certMtime int64
	keyMtime  int64
}

// certResolver loads and caches the configured keypairs and implements the
// GetCertificate callback for SNI selection. When reload is true it re-reads a
// keypair whenever its cert or key file mtime advances.
type certResolver struct {
	entries []certEntry
	reload  bool

	mu    sync.Mutex
	cache map[string]*loadedCert // keyed by certFile
}

func newCertResolver(entries []certEntry, reload bool) *certResolver {
	return &certResolver{
		entries: entries,
		reload:  reload,
		cache:   make(map[string]*loadedCert),
	}
}

// mtime returns the modification time of path in nanoseconds, or 0 if the file
// cannot be stat'd (which forces a reload attempt so a genuine load error
// surfaces during the handshake rather than being silently cached).
func mtime(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.ModTime().UnixNano()
}

// get returns the cached certificate for entry e, loading (or reloading) it
// from disk as needed. Safe for concurrent use.
func (r *certResolver) get(e certEntry) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cached := r.cache[e.certFile]
	if cached != nil && !r.reload {
		return cached.cert, nil
	}

	if cached != nil && r.reload {
		cm := mtime(e.certFile)
		km := mtime(e.keyFile)
		if cm == cached.certMtime && km == cached.keyMtime && cm != 0 {
			return cached.cert, nil
		}
	}

	cert, err := tls.LoadX509KeyPair(e.certFile, e.keyFile)
	if err != nil {
		if cached != nil {
			// Fall back to the last good certificate if a reload fails
			// mid-rotation (e.g. cert written but key not yet updated).
			return cached.cert, nil
		}
		return nil, err
	}
	r.cache[e.certFile] = &loadedCert{
		cert:      &cert,
		certMtime: mtime(e.certFile),
		keyMtime:  mtime(e.keyFile),
	}
	return &cert, nil
}

// leafDNSNames returns the DNS SANs (and, as a fallback, the CN) advertised by
// a certificate, so SNI matching can be performed without TLS having parsed the
// leaf yet.
func leafDNSNames(cert *tls.Certificate) []string {
	if cert.Leaf == nil && len(cert.Certificate) > 0 {
		if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
			cert.Leaf = leaf
		}
	}
	if cert.Leaf == nil {
		return nil
	}
	names := append([]string(nil), cert.Leaf.DNSNames...)
	if len(names) == 0 && cert.Leaf.Subject.CommonName != "" {
		names = append(names, cert.Leaf.Subject.CommonName)
	}
	return names
}

// hostMatches reports whether serverName matches candidate, honoring a single
// leading wildcard label (e.g. "*.example.com").
func hostMatches(serverName, candidate string) bool {
	serverName = strings.ToLower(strings.TrimSuffix(serverName, "."))
	candidate = strings.ToLower(strings.TrimSuffix(candidate, "."))
	if candidate == serverName {
		return true
	}
	if strings.HasPrefix(candidate, "*.") {
		suffix := candidate[1:] // ".example.com"
		if strings.HasSuffix(serverName, suffix) {
			// Wildcard matches exactly one leading label.
			host := serverName[:len(serverName)-len(suffix)]
			if host != "" && !strings.Contains(host, ".") {
				return true
			}
		}
	}
	return false
}

// getCertificate is the tls.Config.GetCertificate callback. It selects the
// keypair whose SANs match hello.ServerName, falling back to the first
// configured entry when nothing matches (or when SNI is absent).
func (r *certResolver) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if len(r.entries) == 0 {
		return nil, fmt.Errorf("tlsutil: no certificates configured")
	}

	if hello != nil && hello.ServerName != "" {
		for _, e := range r.entries {
			cert, err := r.get(e)
			if err != nil {
				continue
			}
			for _, name := range leafDNSNames(cert) {
				if hostMatches(hello.ServerName, name) {
					return cert, nil
				}
			}
		}
	}

	// Fall back to the first (primary) certificate.
	return r.get(r.entries[0])
}

// ServerTLSConfig builds a *tls.Config for the reverse proxy's TLS listener
// from cfg. The returned config sets MinVersion, an optional CipherSuites
// allowlist, a GetCertificate callback for SNI selection across the primary
// keypair plus any additional Certificates, and downstream client
// authentication per ClientAuth/ClientCAFile. When cfg.ReloadOnChange is set,
// GetCertificate hot-reloads a keypair when its files change on disk.
func ServerTLSConfig(cfg config.TLSConfig) (*tls.Config, error) {
	// ACME takes precedence over the static cert_file/key_file path when
	// enabled: the returned config's GetCertificate is backed by an
	// autocert.Manager that obtains and renews certificates automatically.
	// ServerTLSConfigWithCerts also handles the ACME branch; we discard the
	// served-cert slice here since callers of this function do not need it.
	tc, _, err := ServerTLSConfigWithCerts(cfg)
	return tc, err
}

// ServerTLSConfigWithCerts builds the static-cert *tls.Config exactly like
// ServerTLSConfig, but also returns the parsed *tls.Certificate pointers the
// listener serves so a caller can build an OCSP Stapler over the EXACT certs
// presented during the handshake. A Stapler that installs OCSPStaple on these
// pointers therefore takes effect on the next handshake.
//
// The returned certs slice is nil when ACME is enabled (autocert manages its
// own certificates and stapling) — in that case the returned *tls.Config is the
// ACME-backed config, identical to ServerTLSConfig. It is intended for the
// OCSP-stapling wiring; callers that do not need the served certs should keep
// using ServerTLSConfig.
func ServerTLSConfigWithCerts(cfg config.TLSConfig) (*tls.Config, []*tls.Certificate, error) {
	if cfg.ACME.Enabled {
		tc, _, err := NewACMEManager(cfg)
		return tc, nil, err
	}

	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, nil, fmt.Errorf("tlsutil: cert_file and key_file are required")
	}

	entries := make([]certEntry, 0, 1+len(cfg.Certificates))
	entries = append(entries, certEntry{certFile: cfg.CertFile, keyFile: cfg.KeyFile})
	for _, c := range cfg.Certificates {
		if c.CertFile == "" || c.KeyFile == "" {
			return nil, nil, fmt.Errorf("tlsutil: additional certificate missing cert_file or key_file")
		}
		entries = append(entries, certEntry{certFile: c.CertFile, keyFile: c.KeyFile})
	}

	resolver := newCertResolver(entries, cfg.ReloadOnChange)

	// Eagerly load every keypair once so misconfiguration fails at startup and
	// so we can hand the served pointers back for OCSP stapling.
	certs := make([]*tls.Certificate, 0, len(entries))
	for _, e := range entries {
		cert, err := resolver.get(e)
		if err != nil {
			return nil, nil, fmt.Errorf("tlsutil: loading keypair %s/%s: %w", e.certFile, e.keyFile, err)
		}
		certs = append(certs, cert)
	}

	suites, err := cipherSuiteIDs(cfg.CipherSuites)
	if err != nil {
		return nil, nil, err
	}

	tc := &tls.Config{
		MinVersion:     minVersionFor(cfg.MinVersion),
		CipherSuites:   suites,
		GetCertificate: resolver.getCertificate,
	}

	switch cfg.ClientAuth {
	case "require_and_verify":
		tc.ClientAuth = tls.RequireAndVerifyClientCert
	case "request":
		tc.ClientAuth = tls.RequestClientCert
	case "none", "":
		tc.ClientAuth = tls.NoClientCert
	default:
		return nil, nil, fmt.Errorf("tlsutil: unknown client_auth %q", cfg.ClientAuth)
	}

	if tc.ClientAuth == tls.RequireAndVerifyClientCert || tc.ClientAuth == tls.RequestClientCert {
		if cfg.ClientCAFile != "" {
			pool, err := loadCAPool(cfg.ClientCAFile)
			if err != nil {
				return nil, nil, err
			}
			tc.ClientCAs = pool
		} else if tc.ClientAuth == tls.RequireAndVerifyClientCert {
			return nil, nil, fmt.Errorf("tlsutil: client_auth=require_and_verify needs client_ca_file")
		}
	}

	return tc, certs, nil
}

// loadCAPool reads a PEM CA bundle into an x509.CertPool.
func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tlsutil: reading client_ca_file %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("tlsutil: no valid certificates in client_ca_file %s", path)
	}
	return pool, nil
}

// ---------------------------------------------------------------------------
// ACME (automatic certificate management)
// ---------------------------------------------------------------------------

// memCache is an in-memory autocert.Cache used when ACMEConfig.CacheDir is
// empty. It is safe for concurrent use. Certificates cached here do not survive
// a restart, so a persistent DirCache is preferred for production.
type memCache struct {
	mu sync.RWMutex
	m  map[string][]byte
}

func newMemCache() *memCache { return &memCache{m: make(map[string][]byte)} }

func (c *memCache) Get(_ context.Context, key string) ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[key]
	if !ok {
		return nil, autocert.ErrCacheMiss
	}
	// Return a copy so callers cannot mutate cached bytes.
	out := make([]byte, len(v))
	copy(out, v)
	return out, nil
}

func (c *memCache) Put(_ context.Context, key string, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := make([]byte, len(data))
	copy(v, data)
	c.m[key] = v
	return nil
}

func (c *memCache) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
	return nil
}

// ACMEManager wraps an autocert.Manager so the TLS GetCertificate callback and
// the HTTP-01 challenge handler share the same account, cache, and challenge
// state. Construct one with NewACMEManager (or the convenience wrappers
// ServerTLSConfig / ACMEHTTPHandler).
type ACMEManager struct {
	Manager *autocert.Manager
}

// newACMEManager builds an *ACMEManager from cfg. It requires cfg.ACME.Enabled
// and a non-empty Domains list.
func newACMEManager(cfg config.TLSConfig) (*ACMEManager, error) {
	ac := cfg.ACME
	if !ac.Enabled {
		return nil, fmt.Errorf("tlsutil: ACME is not enabled")
	}
	if len(ac.Domains) == 0 {
		return nil, fmt.Errorf("tlsutil: ACME requires at least one domain")
	}

	var cache autocert.Cache
	if ac.CacheDir != "" {
		cache = autocert.DirCache(ac.CacheDir)
	} else {
		cache = newMemCache()
	}

	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(ac.Domains...),
		Cache:      cache,
		Email:      ac.Email,
	}
	if ac.DirectoryURL != "" {
		// Point at a non-prod / test ACME CA. Mutating Client after the first
		// GetCertificate call has no effect, so we set it here at construction.
		m.Client = &acme.Client{DirectoryURL: ac.DirectoryURL}
	}

	return &ACMEManager{Manager: m}, nil
}

// TLSConfig returns a *tls.Config whose GetCertificate is served by the
// underlying autocert.Manager. It also advertises the ACME TLS-ALPN-01
// protocol so that challenge type is available in addition to HTTP-01.
func (a *ACMEManager) TLSConfig() *tls.Config {
	return a.Manager.TLSConfig()
}

// HTTPHandler returns the HTTP-01 challenge handler for the underlying manager.
// Serve it on the ACME HTTP challenge port (ACMEConfig.HTTPChallengePort,
// default 80). The fallback is nil, matching autocert's default behavior of
// redirecting non-challenge requests to HTTPS.
func (a *ACMEManager) HTTPHandler() http.Handler {
	return a.Manager.HTTPHandler(nil)
}

// NewACMEManager builds the shared ACME state for cfg and returns both the
// server *tls.Config (GetCertificate backed by the manager) and the HTTP-01
// challenge http.Handler that must be served on the challenge port. Both share
// the same autocert.Manager. It errors if ACME is not enabled or misconfigured.
func NewACMEManager(cfg config.TLSConfig) (*tls.Config, http.Handler, error) {
	am, err := newACMEManager(cfg)
	if err != nil {
		return nil, nil, err
	}
	tc := am.TLSConfig()
	tc.MinVersion = minVersionFor(cfg.MinVersion)
	return tc, am.HTTPHandler(), nil
}

// ACMEHTTPHandler returns the HTTP-01 challenge handler for cfg, or nil when
// ACME is disabled (or misconfigured). The server serves the returned handler
// on the ACME challenge port. Note: this constructs a fresh autocert.Manager;
// to share challenge state with the TLS listener, prefer NewACMEManager and
// reuse the returned handler alongside the returned *tls.Config.
func ACMEHTTPHandler(cfg config.TLSConfig) http.Handler {
	if !cfg.ACME.Enabled {
		return nil
	}
	am, err := newACMEManager(cfg)
	if err != nil {
		return nil
	}
	return am.HTTPHandler()
}

// ---------------------------------------------------------------------------
// OCSP stapling
// ---------------------------------------------------------------------------

// stapleTarget is a single leaf certificate whose OCSP staple is refreshed by a
// Stapler. issuer is the certificate that signed the leaf (needed to build the
// OCSP request and verify the response); responderURL is the leaf's OCSP
// responder endpoint.
type stapleTarget struct {
	cert         *tls.Certificate
	leaf         *x509.Certificate
	issuer       *x509.Certificate
	responderURL string
}

// Stapler periodically fetches OCSP responses for a set of server certificates
// and installs them into each tls.Certificate.OCSPStaple, so Go staples the
// response during the handshake. Fetches happen at startup and are refreshed
// ahead of each response's NextUpdate. The HTTP client and clock are injectable
// for tests. A Stapler is safe for concurrent use.
type Stapler struct {
	targets []*stapleTarget

	// HTTPClient performs OCSP POST requests. If nil, http.DefaultClient is used.
	HTTPClient *http.Client
	// Now returns the current time. If nil, time.Now is used. Injectable so
	// tests can control refresh scheduling deterministically.
	Now func() time.Time
	// MinRefresh bounds how frequently the background loop re-fetches, so a
	// short-lived test response (or a responder that omits NextUpdate) does not
	// spin. Defaults to a small floor when zero.
	MinRefresh time.Duration

	mu     sync.Mutex // guards writes to each target's cert.OCSPStaple
	stopMu sync.Mutex // guards stop channel lifecycle
	stop   chan struct{}
	wg     sync.WaitGroup
}

// NewStapler builds a Stapler for every certificate in certs that advertises an
// OCSP responder URL and carries an issuer (its second entry, or a caller in
// this package parsing the chain). Certificates without a responder URL or
// issuer are skipped (nothing to staple). The returned Stapler has not fetched
// yet; call Start (or RefreshOnce) to populate staples.
func NewStapler(certs []*tls.Certificate) *Stapler {
	s := &Stapler{}
	for _, c := range certs {
		t := newStapleTarget(c)
		if t != nil {
			s.targets = append(s.targets, t)
		}
	}
	return s
}

// newStapleTarget derives a stapleTarget from a tls.Certificate, or nil if it
// lacks an OCSP responder URL or an issuer certificate in its chain.
func newStapleTarget(c *tls.Certificate) *stapleTarget {
	if c == nil || len(c.Certificate) == 0 {
		return nil
	}
	leaf := c.Leaf
	if leaf == nil {
		var err error
		leaf, err = x509.ParseCertificate(c.Certificate[0])
		if err != nil {
			return nil
		}
		c.Leaf = leaf
	}
	if len(leaf.OCSPServer) == 0 {
		return nil
	}
	// The issuer is the next certificate in the presented chain.
	if len(c.Certificate) < 2 {
		return nil
	}
	issuer, err := x509.ParseCertificate(c.Certificate[1])
	if err != nil {
		return nil
	}
	return &stapleTarget{
		cert:         c,
		leaf:         leaf,
		issuer:       issuer,
		responderURL: leaf.OCSPServer[0],
	}
}

func (s *Stapler) httpClient() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return http.DefaultClient
}

func (s *Stapler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// fetch retrieves and verifies a fresh OCSP response for target t and returns
// the raw DER response together with its parsed form.
func (s *Stapler) fetch(ctx context.Context, t *stapleTarget) ([]byte, *ocsp.Response, error) {
	reqDER, err := ocsp.CreateRequest(t.leaf, t.issuer, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("tlsutil: building OCSP request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.responderURL, bytes.NewReader(reqDER))
	if err != nil {
		return nil, nil, fmt.Errorf("tlsutil: OCSP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/ocsp-request")
	httpReq.Header.Set("Accept", "application/ocsp-response")

	resp, err := s.httpClient().Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("tlsutil: OCSP responder %s: %w", t.responderURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("tlsutil: OCSP responder %s: status %d", t.responderURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("tlsutil: reading OCSP response: %w", err)
	}
	parsed, err := ocsp.ParseResponseForCert(body, t.leaf, t.issuer)
	if err != nil {
		return nil, nil, fmt.Errorf("tlsutil: parsing OCSP response: %w", err)
	}
	return body, parsed, nil
}

// refreshTarget fetches a staple for t and installs it. It returns the parsed
// response (for scheduling the next refresh) or an error.
func (s *Stapler) refreshTarget(ctx context.Context, t *stapleTarget) (*ocsp.Response, error) {
	der, parsed, err := s.fetch(ctx, t)
	if err != nil {
		return nil, err
	}
	// Only staple a "good" response; a revoked/unknown status must not be
	// stapled (it would tell clients the cert is bad).
	if parsed.Status != ocsp.Good {
		return parsed, fmt.Errorf("tlsutil: OCSP status not good (%d) for %s", parsed.Status, t.responderURL)
	}
	s.mu.Lock()
	t.cert.OCSPStaple = der
	s.mu.Unlock()
	return parsed, nil
}

// RefreshOnce fetches and installs a staple for every target once, synchronously.
// It returns the first error encountered but still attempts every target, so a
// single failing responder does not block the others. Callers typically invoke
// this once at startup, then Start for periodic refresh.
func (s *Stapler) RefreshOnce(ctx context.Context) error {
	var firstErr error
	for _, t := range s.targets {
		if _, err := s.refreshTarget(ctx, t); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// nextRefresh computes when to next refresh a staple given its parsed response.
// It aims for roughly halfway between now and NextUpdate, clamped to MinRefresh.
func (s *Stapler) nextRefresh(parsed *ocsp.Response) time.Duration {
	floor := s.MinRefresh
	if floor <= 0 {
		floor = time.Minute
	}
	if parsed == nil || parsed.NextUpdate.IsZero() {
		return floor
	}
	remaining := parsed.NextUpdate.Sub(s.now())
	if remaining <= 0 {
		return floor
	}
	d := remaining / 2
	if d < floor {
		d = floor
	}
	return d
}

// Start performs an initial synchronous refresh of all staples, then launches a
// background goroutine that periodically re-fetches each staple ahead of its
// NextUpdate. It is a no-op (returns nil) when there are no OCSP targets. Call
// Stop to shut the goroutine down. Start must not be called twice concurrently.
func (s *Stapler) Start(ctx context.Context) error {
	if len(s.targets) == 0 {
		return nil
	}
	// Initial population; report the first error but keep running so transient
	// responder outages self-heal on the next tick.
	err := s.RefreshOnce(ctx)

	s.stopMu.Lock()
	if s.stop != nil {
		s.stopMu.Unlock()
		return err
	}
	s.stop = make(chan struct{})
	stop := s.stop
	s.wg.Add(len(s.targets))
	for _, t := range s.targets {
		go s.refreshLoop(stop, t)
	}
	s.stopMu.Unlock()

	return err
}

// refreshLoop refreshes a single target ahead of each response's NextUpdate
// until stop is closed. Each target runs in its own goroutine so a slow
// responder for one certificate does not delay the others.
func (s *Stapler) refreshLoop(stop chan struct{}, t *stapleTarget) {
	defer s.wg.Done()

	delay := s.initialDelay(t)
	timer := time.NewTimer(delay)
	defer timer.Stop()

	for {
		select {
		case <-stop:
			return
		case <-timer.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			parsed, _ := s.refreshTarget(ctx, t)
			cancel()
			timer.Reset(s.nextRefresh(parsed))
		}
	}
}

// initialDelay picks the first refresh delay for a target based on the staple
// already installed (if any). Without prior state it uses MinRefresh.
func (s *Stapler) initialDelay(t *stapleTarget) time.Duration {
	s.mu.Lock()
	der := t.cert.OCSPStaple
	s.mu.Unlock()
	if len(der) > 0 {
		if parsed, err := ocsp.ParseResponseForCert(der, t.leaf, t.issuer); err == nil {
			return s.nextRefresh(parsed)
		}
	}
	if s.MinRefresh > 0 {
		return s.MinRefresh
	}
	return time.Minute
}

// Stop signals the background refresh goroutines to exit and waits for them. It
// is safe to call multiple times and safe to call when Start was never called.
func (s *Stapler) Stop() {
	s.stopMu.Lock()
	stop := s.stop
	s.stop = nil
	s.stopMu.Unlock()

	if stop == nil {
		return
	}
	close(stop)
	s.wg.Wait()
}
