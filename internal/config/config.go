package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// TracingConfig configures OpenTelemetry distributed tracing.
// When Enabled is false, a noop TracerProvider is installed so the binary
// compiles and runs without any telemetry overhead or network dials.
type TracingConfig struct {
	// Enabled activates distributed tracing; default false.
	Enabled bool `yaml:"enabled"`
	// Exporter selects the trace exporter; currently only "otlp" is supported.
	// Default "otlp".
	Exporter string `yaml:"exporter"`
	// Endpoint is the OTLP gRPC collector address (host:port).
	// Default "localhost:4317".
	Endpoint string `yaml:"endpoint"`
	// SampleRate is the fraction (0.0-1.0) of traces to sample; 1.0 means
	// sample everything. Default 1.0.
	SampleRate float64 `yaml:"sample_rate"`
	// ServiceName is the service.name resource attribute visible in the tracing
	// backend. Default "rplb".
	ServiceName string `yaml:"service_name"`
}

// Config is the top-level configuration loaded from YAML, containing all subsystem configs.
type Config struct {
	Server         ServerConfig         `yaml:"server"`
	TLS            TLSConfig            `yaml:"tls"`
	BackendTLS     BackendTLSConfig     `yaml:"backend_tls"`
	Backends       []BackendConfig      `yaml:"backends"`
	LoadBalancer   LoadBalancerConfig   `yaml:"load_balancer"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
	RateLimiter    RateLimiterConfig    `yaml:"rate_limiter"`
	Retry          RetryConfig          `yaml:"retry"`
	Logging        LoggingConfig        `yaml:"logging"`
	Metrics        MetricsConfig        `yaml:"metrics"`
	Compression    CompressionConfig    `yaml:"compression"`
	// Tracing configures optional OpenTelemetry distributed tracing. Disabled
	// by default; its per-field defaults (exporter, endpoint, sample_rate,
	// service_name) are applied by Load().
	Tracing TracingConfig `yaml:"tracing"`
	// Security configures optional edge security middleware (headers, CORS, IP/
	// method ACLs, and client authentication). Every block is opt-in and defaults
	// to disabled, preserving current behavior.
	Security SecurityConfig `yaml:"security"`
	// Routes configures optional L7 request routing. When empty, the proxy uses
	// the single default balancer (Backends + LoadBalancer) as before. Requests
	// are matched to routes first-match-wins; unmatched requests use the default.
	Routes []RouteConfig `yaml:"routes"`
	// Canary configures optional canary traffic splitting: a WeightPercent share
	// of requests is routed to a separate canary backend pool. Disabled by default.
	Canary CanaryConfig `yaml:"canary"`
	// Mirror configures optional fire-and-forget request mirroring (shadow
	// traffic) to a secondary URL. Disabled by default.
	Mirror MirrorConfig `yaml:"mirror"`
	// Rewrite configures optional request/response header and path rewriting.
	// Disabled/empty by default (no-op).
	Rewrite RewriteConfig `yaml:"rewrite"`
	// FaultInjection configures optional latency/abort fault injection for a
	// percentage of requests (chaos testing). Disabled by default.
	FaultInjection FaultConfig `yaml:"fault_injection"`
	// Cache configures an optional in-memory HTTP response cache. Disabled by
	// default; its per-field defaults are applied by Load().
	Cache CacheConfig `yaml:"cache"`
	// Discovery configures optional dynamic backend discovery (currently DNS-based)
	// that periodically resolves targets into the default backend group. Empty by
	// default (no discovery); per-target defaults are applied by Load().
	Discovery DiscoveryConfig `yaml:"discovery"`
}

// CacheConfig configures an optional in-memory HTTP response cache. When Enabled,
// cacheable responses to requests using one of Methods are stored for up to
// DefaultTTL, bounded by MaxEntries and MaxBodyBytes. Disabled by default; its
// per-field defaults (DefaultTTL 60s, MaxEntries 1000, MaxBodyBytes 1MiB, Methods
// [GET,HEAD]) are applied by Load().
type CacheConfig struct {
	Enabled bool `yaml:"enabled"`
	// DefaultTTL is how long a cached response is served before revalidation;
	// default 60s.
	DefaultTTL time.Duration `yaml:"default_ttl"`
	// MaxEntries caps the number of cached responses; default 1000.
	MaxEntries int `yaml:"max_entries"`
	// MaxBodyBytes caps the size of a cacheable response body; default 1MiB.
	MaxBodyBytes int `yaml:"max_body_bytes"`
	// Methods lists the request methods eligible for caching; default [GET,HEAD].
	Methods []string `yaml:"methods"`
}

// DiscoveryConfig configures optional dynamic backend discovery. Currently only
// DNS-based discovery is supported; each DNSTarget is resolved periodically into
// the default backend group. Empty by default.
type DiscoveryConfig struct {
	DNS []DNSTarget `yaml:"dns"`
}

// DNSTarget describes a single DNS name resolved periodically into backends in
// the default group. Type "a" resolves A/AAAA records (using Port); type "srv"
// resolves SRV records (host+port from the record). Per-field defaults (Type "a",
// Scheme "http", Interval 30s, Weight 1, MaxConns 100) are applied by Load().
type DNSTarget struct {
	// Name is the DNS name to resolve; required.
	Name string `yaml:"name"`
	// Type selects the record kind: "a" (default) or "srv".
	Type string `yaml:"type"`
	// Scheme is the backend URL scheme for resolved addresses: "http" (default) or
	// "https".
	Scheme string `yaml:"scheme"`
	// Port is the backend port used for "a"-type targets (ignored for "srv", whose
	// port comes from the record).
	Port int `yaml:"port"`
	// Interval is how often the target is re-resolved; default 30s.
	Interval time.Duration `yaml:"interval"`
	// Weight is the load-balancing weight assigned to resolved backends; default 1.
	Weight int `yaml:"weight"`
	// MaxConns caps connections per resolved backend; default 100.
	MaxConns int `yaml:"max_conns"`
}

// CanaryConfig configures canary traffic splitting. When Enabled, WeightPercent
// (0..100) of traffic is sent to the canary Backends pool using Algorithm (and
// ConsistentHash when Algorithm is "consistent_hash"); the remainder continues
// to the primary pool. Its algorithm/consistent-hash/backend defaults are applied
// by Load().
type CanaryConfig struct {
	Enabled bool `yaml:"enabled"`
	// WeightPercent is the 0..100 share of traffic routed to the canary pool.
	WeightPercent int `yaml:"weight_percent"`
	// Algorithm is the canary pool's load-balancing algorithm; default "round_robin".
	Algorithm string `yaml:"algorithm"`
	// ConsistentHash tunes the canary consistent-hash ring (when Algorithm is
	// "consistent_hash"). Its replicas/load_factor are defaulted by Load().
	ConsistentHash ConsistentHashConfig `yaml:"consistent_hash"`
	// Backends are the canary upstreams; at least one is required when Enabled.
	Backends []BackendConfig `yaml:"backends"`
}

// MirrorConfig configures fire-and-forget request mirroring. When Enabled, a
// SamplePercent (0..100) share of requests is shadow-copied to URL with the
// given Timeout; mirror responses are discarded and never affect the client.
type MirrorConfig struct {
	Enabled bool `yaml:"enabled"`
	// URL is the shadow target base URL (http/https); required when Enabled.
	URL string `yaml:"url"`
	// SamplePercent is the 0..100 share of requests mirrored.
	SamplePercent int `yaml:"sample_percent"`
	// Timeout bounds each fire-and-forget mirror request.
	Timeout time.Duration `yaml:"timeout"`
}

// RewriteConfig configures request/response header rewriting, path-prefix
// stripping, and HTTP->HTTPS redirects. Empty fields are no-ops.
type RewriteConfig struct {
	// RequestHeadersSet sets (overwrites) the named request headers before proxying.
	RequestHeadersSet map[string]string `yaml:"request_headers_set"`
	// RequestHeadersRemove removes the named request headers before proxying.
	RequestHeadersRemove []string `yaml:"request_headers_remove"`
	// ResponseHeadersSet sets (overwrites) the named response headers before replying.
	ResponseHeadersSet map[string]string `yaml:"response_headers_set"`
	// ResponseHeadersRemove removes the named response headers before replying.
	ResponseHeadersRemove []string `yaml:"response_headers_remove"`
	// StripPathPrefix, when set, is stripped from the request path before proxying.
	StripPathPrefix string `yaml:"strip_path_prefix"`
	// HTTPSRedirect, when true, redirects plain HTTP requests to HTTPS.
	HTTPSRedirect bool `yaml:"https_redirect"`
}

// FaultConfig configures fault injection for chaos testing. When Enabled, a
// DelayPercent (0..100) share of requests is delayed by Delay, and an
// AbortPercent (0..100) share is aborted with AbortStatus. Disabled by default.
type FaultConfig struct {
	Enabled bool `yaml:"enabled"`
	// DelayPercent is the 0..100 share of requests delayed by Delay.
	DelayPercent int `yaml:"delay_percent"`
	// Delay is the injected latency for delayed requests.
	Delay time.Duration `yaml:"delay"`
	// AbortPercent is the 0..100 share of requests aborted with AbortStatus.
	AbortPercent int `yaml:"abort_percent"`
	// AbortStatus is the HTTP status returned for aborted requests; default 503.
	AbortStatus int `yaml:"abort_status"`
}

// RouteConfig defines a single L7 route: a set of optional match criteria and
// the per-route backend group (algorithm + backends + consistent-hash options)
// used for requests that match. Advanced wrappers (priority/zone/slow-start/
// outlier) remain on the default group only; per-route wrappers are a follow-up.
type RouteConfig struct {
	// Name optionally labels the route. When set, names must be unique.
	Name string `yaml:"name"`
	// Host, when set, matches the request Host exactly (case-insensitive).
	Host string `yaml:"host"`
	// PathPrefix, when set, matches when the request URL path has this prefix.
	PathPrefix string `yaml:"path_prefix"`
	// Methods, when set, matches when the request method is any-of (case-insensitive).
	Methods []string `yaml:"methods"`
	// Headers, when set, matches when all named headers equal the given values exactly.
	Headers map[string]string `yaml:"headers"`
	// Algorithm is the per-route load-balancing algorithm; default "round_robin".
	Algorithm string `yaml:"algorithm"`
	// ConsistentHash tunes the per-route consistent-hash ring (when Algorithm is
	// "consistent_hash"). Its replicas/load_factor are defaulted by Load().
	ConsistentHash ConsistentHashConfig `yaml:"consistent_hash"`
	// Backends are the per-route upstreams; at least one is required.
	Backends []BackendConfig `yaml:"backends"`
}

// CompressionConfig controls gzip compression eligibility by content type and minimum body size.
type CompressionConfig struct {
	Enabled bool `yaml:"enabled"`
	// MinSize only compresses response bodies of at least this many bytes; the
	// default 0 compresses all bodies (current behavior).
	MinSize int `yaml:"min_size"`
	// ContentTypes is an allowlist of Content-Type prefixes eligible for
	// compression; empty compresses all content types (current behavior).
	ContentTypes []string `yaml:"content_types"`
}

// ServerConfig holds the HTTP server bind address, timeouts, TLS, and optional subsystem configs.
type ServerConfig struct {
	Host         string        `yaml:"host"`
	Port         int           `yaml:"port"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"`
	// ShutdownTimeout bounds how long graceful shutdown waits for in-flight
	// requests to drain before forcing close. Defaults to 30s.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	TrustedProxies  []string      `yaml:"trusted_proxies"`
	// Zone is the availability zone/region this proxy instance runs in. When set
	// alongside LoadBalancer.PreferSameZone, backends sharing this zone are
	// preferred during selection.
	Zone string `yaml:"zone"`
	// Upstream tunes the HTTP transport used to reach backends. Its per-field
	// defaults are applied by Load().
	Upstream UpstreamConfig `yaml:"upstream"`
	// L4 configures an optional raw TCP (layer-4) proxy. Disabled by default.
	L4 L4Config `yaml:"l4"`
	// WebSocket tunes WebSocket connection handling. Zero values mean unlimited.
	WebSocket WebSocketConfig `yaml:"websocket"`
	// WatchConfig, when true, polls the config file on disk and auto-reloads on
	// change. Disabled by default; existing SIGHUP / POST /reload behavior is
	// unchanged when off.
	WatchConfig bool `yaml:"watch_config"`
	// WatchInterval is the poll interval used when WatchConfig is enabled;
	// defaults to 5s when WatchConfig is set and this is <= 0.
	WatchInterval time.Duration `yaml:"watch_interval"`
	// HTTP3 configures optional HTTP/3 (QUIC) downstream support. Disabled by
	// default; requires TLS to be configured (QUIC mandates TLS 1.3).
	HTTP3 HTTP3Config `yaml:"http3"`
}

// HTTP3Config configures optional HTTP/3 (QUIC) downstream support. When
// Enabled, an additional UDP listener is started on Port (default: same as the
// main server port) and the Alt-Svc header is injected on every response so
// clients can upgrade. TLS must be configured; enabling HTTP/3 without TLS is
// a validation error.
type HTTP3Config struct {
	// Enabled activates HTTP/3 support; requires tls.enabled to be true.
	Enabled bool `yaml:"enabled"`
	// Port is the UDP port the QUIC listener binds to. Defaults to the main
	// server port when 0. Must be >= 1 when Enabled.
	Port int `yaml:"port"`
}

// UpstreamConfig tunes the HTTP transport used to connect to backends. All
// timeouts default via Load(); an explicit smaller value is respected. HTTP2
// is opt-in and defaults to false to preserve current behavior.
type UpstreamConfig struct {
	// DialTimeout bounds establishing a TCP connection to a backend; default 5s.
	DialTimeout time.Duration `yaml:"dial_timeout"`
	// TLSHandshakeTimeout bounds the TLS handshake to an https backend; default 5s.
	TLSHandshakeTimeout time.Duration `yaml:"tls_handshake_timeout"`
	// ResponseHeaderTimeout bounds waiting for a backend's response headers;
	// default 30s.
	ResponseHeaderTimeout time.Duration `yaml:"response_header_timeout"`
	// ExpectContinueTimeout bounds waiting for a 100-continue response; default 1s.
	ExpectContinueTimeout time.Duration `yaml:"expect_continue_timeout"`
	// IdleConnTimeout bounds how long an idle keep-alive connection is retained;
	// default 90s.
	IdleConnTimeout time.Duration `yaml:"idle_conn_timeout"`
	// MaxIdleConns caps total idle keep-alive connections across all hosts;
	// default 100.
	MaxIdleConns int `yaml:"max_idle_conns"`
	// MaxIdleConnsPerHost caps idle keep-alive connections per host; default 100.
	MaxIdleConnsPerHost int `yaml:"max_idle_conns_per_host"`
	// MaxConnsPerHost caps total connections per host; default 0 (unlimited).
	MaxConnsPerHost int `yaml:"max_conns_per_host"`
	// HTTP2 enables HTTP/2 to backends; default false (current behavior).
	HTTP2 bool `yaml:"http2"`
}

// L4Config configures an optional raw TCP (layer-4) proxy. Disabled by default.
type L4Config struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
	// DialTimeout bounds establishing the upstream TCP connection; default 5s.
	DialTimeout time.Duration `yaml:"dial_timeout"`
}

// WebSocketConfig tunes WebSocket connection handling. A zero value for either
// field means unlimited (the default).
type WebSocketConfig struct {
	// IdleTimeout closes an idle WebSocket connection after this duration; 0 =
	// unlimited (default).
	IdleTimeout time.Duration `yaml:"idle_timeout"`
	// MaxMessageBytes caps a single WebSocket message size; 0 = unlimited (default).
	MaxMessageBytes int64 `yaml:"max_message_bytes"`
}

// applyUpstreamDefaults fills in the documented upstream transport defaults.
// Only zero-values are filled so an explicitly configured smaller value is
// respected. HTTP2 and MaxConnsPerHost keep their zero-values (false / 0).
func applyUpstreamDefaults(u *UpstreamConfig) {
	if u.DialTimeout == 0 {
		u.DialTimeout = 5 * time.Second
	}
	if u.TLSHandshakeTimeout == 0 {
		u.TLSHandshakeTimeout = 5 * time.Second
	}
	if u.ResponseHeaderTimeout == 0 {
		u.ResponseHeaderTimeout = 30 * time.Second
	}
	if u.ExpectContinueTimeout == 0 {
		u.ExpectContinueTimeout = 1 * time.Second
	}
	if u.IdleConnTimeout == 0 {
		u.IdleConnTimeout = 90 * time.Second
	}
	if u.MaxIdleConns == 0 {
		u.MaxIdleConns = 100
	}
	if u.MaxIdleConnsPerHost == 0 {
		u.MaxIdleConnsPerHost = 100
	}
}

// applyL4Defaults fills in the documented L4 defaults. The proxy stays disabled
// unless explicitly enabled; only the dial timeout is defaulted.
func applyL4Defaults(l *L4Config) {
	if l.DialTimeout == 0 {
		l.DialTimeout = 5 * time.Second
	}
}

// applyCanaryDefaults fills in the documented canary defaults: a round_robin
// algorithm, the consistent-hash ring defaults, and per-backend weight/max_conns
// (and any per-backend health-check defaults). The canary stays disabled unless
// explicitly enabled.
func applyCanaryDefaults(c *CanaryConfig) {
	if c.Algorithm == "" {
		c.Algorithm = "round_robin"
	}
	if c.ConsistentHash.Replicas == 0 {
		c.ConsistentHash.Replicas = 100
	}
	if c.ConsistentHash.LoadFactor == 0 {
		c.ConsistentHash.LoadFactor = 1.25
	}
	for i := range c.Backends {
		if c.Backends[i].Weight == 0 {
			c.Backends[i].Weight = 1
		}
		if c.Backends[i].MaxConns == 0 {
			c.Backends[i].MaxConns = 100
		}
		if c.Backends[i].HealthCheck != nil {
			applyHealthCheckDefaults(c.Backends[i].HealthCheck)
		}
	}
}

// applyCacheDefaults fills in the documented cache defaults: DefaultTTL 60s,
// MaxEntries 1000, MaxBodyBytes 1MiB, and Methods [GET,HEAD]. Defaults are always
// applied (not just when enabled) so downstream consumers see sane values, but the
// cache stays disabled unless explicitly enabled.
func applyCacheDefaults(c *CacheConfig) {
	if c.DefaultTTL <= 0 {
		c.DefaultTTL = 60 * time.Second
	}
	if c.MaxEntries <= 0 {
		c.MaxEntries = 1000
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 1 << 20 // 1 MiB
	}
	if len(c.Methods) == 0 {
		c.Methods = []string{"GET", "HEAD"}
	}
}

// applyDiscoveryDefaults fills in the documented per-DNSTarget defaults: Type "a",
// Scheme "http", Interval 30s, Weight 1, and MaxConns 100.
func applyDiscoveryDefaults(d *DiscoveryConfig) {
	for i := range d.DNS {
		t := &d.DNS[i]
		if t.Type == "" {
			t.Type = "a"
		}
		if t.Scheme == "" {
			t.Scheme = "http"
		}
		if t.Interval <= 0 {
			t.Interval = 30 * time.Second
		}
		if t.Weight == 0 {
			t.Weight = 1
		}
		if t.MaxConns == 0 {
			t.MaxConns = 100
		}
	}
}

// applyTracingDefaults fills in the documented tracing defaults. The tracing
// block stays disabled (Enabled=false) unless explicitly enabled; the remaining
// fields are always defaulted so downstream consumers see sane values.
func applyTracingDefaults(t *TracingConfig) {
	if t.Exporter == "" {
		t.Exporter = "otlp"
	}
	if t.Endpoint == "" {
		t.Endpoint = "localhost:4317"
	}
	if t.SampleRate == 0 {
		t.SampleRate = 1.0
	}
	if t.ServiceName == "" {
		t.ServiceName = "rplb"
	}
}

// applyFaultDefaults fills in the documented fault-injection defaults. When
// enabled, an unset/invalid AbortStatus defaults to 503.
func applyFaultDefaults(f *FaultConfig) {
	if f.Enabled && f.AbortStatus <= 0 {
		f.AbortStatus = 503
	}
}

// validAlgorithms is the set of accepted load-balancing algorithm names.
var validAlgorithms = map[string]bool{
	"round_robin":         true,
	"least_conn":          true,
	"weighted":            true,
	"ip_hash":             true,
	"swrr":                true,
	"p2c":                 true,
	"weighted_least_conn": true,
	"weighted_random":     true,
	"consistent_hash":     true,
	"ewma":                true,
}

// TLSConfig configures TLS for the downstream (client->proxy) listener.
type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// MinVersion is the minimum negotiated TLS version: "1.2" (default) or "1.3".
	MinVersion string `yaml:"min_version"`
	// CipherSuites optionally restricts the offered cipher suites to the named Go
	// cipher-suite names (e.g. "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"). Empty =>
	// Go defaults. Ignored for TLS 1.3 (whose suites are not configurable in Go).
	CipherSuites []string `yaml:"cipher_suites"`
	// Certificates lists ADDITIONAL certificate/key pairs served via SNI, besides
	// the primary CertFile/KeyFile pair.
	Certificates []CertPair `yaml:"certificates"`
	// ClientAuth configures downstream (client -> proxy) mutual TLS: "none"
	// (default), "request", or "require_and_verify".
	ClientAuth string `yaml:"client_auth"`
	// ClientCAFile is a PEM bundle of CAs used to verify client certificates.
	// Required when ClientAuth is "require_and_verify".
	ClientCAFile string `yaml:"client_ca_file"`
	// ReloadOnChange enables hot-reloading of the server certificate files when
	// they change on disk, without a restart.
	ReloadOnChange bool `yaml:"reload_on_change"`
	// ACME configures automatic certificate provisioning/renewal via an ACME CA
	// (e.g. Let's Encrypt). Disabled by default. When enabled, ACME takes
	// precedence over the static CertFile/KeyFile (which are then ignored).
	ACME ACMEConfig `yaml:"acme"`
	// OCSPStapling, when true, fetches and staples OCSP responses for the served
	// certificates so clients can verify revocation without a separate round-trip.
	OCSPStapling bool `yaml:"ocsp_stapling"`
}

// ACMEConfig configures automatic certificate provisioning and renewal via an
// ACME CA (e.g. Let's Encrypt) using the HTTP-01 challenge. Disabled by default.
// When Enabled, certificates are obtained and renewed automatically for Domains,
// and ACME takes precedence over any static TLS CertFile/KeyFile.
type ACMEConfig struct {
	Enabled bool `yaml:"enabled"`
	// Domains is the list of hostnames certificates are provisioned for; at least
	// one is required when Enabled.
	Domains []string `yaml:"domains"`
	// Email is the optional ACME account contact address.
	Email string `yaml:"email"`
	// CacheDir is the directory where obtained certificates and account keys are
	// cached across restarts.
	CacheDir string `yaml:"cache_dir"`
	// DirectoryURL optionally points at a non-production ACME CA directory (for
	// tests/staging). Empty uses the autocert default (Let's Encrypt production).
	DirectoryURL string `yaml:"directory_url"`
	// HTTPChallengePort is the port serving the HTTP-01 challenge; default 80.
	HTTPChallengePort int `yaml:"http_challenge_port"`
}

// CertPair is a single certificate/key file pair used for SNI multi-cert serving.
type CertPair struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// BackendTLSConfig controls how the proxy connects to https:// backends.
type BackendTLSConfig struct {
	// InsecureSkipVerify disables verification of backend certificates. Use only for
	// testing; it exposes the proxy to MITM on the backend leg.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
	// CAFile is a PEM bundle of additional roots trusted for backend certificates.
	CAFile string `yaml:"ca_file"`
	// ClientCertFile/ClientKeyFile, when both set, enable mutual TLS TO backends:
	// the keypair is presented to backends that request a client certificate.
	ClientCertFile string `yaml:"client_cert_file"`
	ClientKeyFile  string `yaml:"client_key_file"`
}

// SecurityConfig groups the optional edge security middleware. Each sub-block is
// opt-in and defaults to disabled/empty (no-op), preserving current behavior.
type SecurityConfig struct {
	Headers HeadersConfig `yaml:"headers"`
	CORS    CORSConfig    `yaml:"cors"`
	ACL     ACLConfig     `yaml:"acl"`
	Auth    AuthConfig    `yaml:"auth"`
}

// HeadersConfig configures common security response headers. Only non-empty
// fields are emitted; ContentTypeOptions emits "X-Content-Type-Options: nosniff".
type HeadersConfig struct {
	Enabled bool `yaml:"enabled"`
	// HSTS sets the Strict-Transport-Security header value (e.g. "max-age=31536000").
	HSTS string `yaml:"hsts"`
	// FrameOptions sets the X-Frame-Options header value (e.g. "DENY").
	FrameOptions string `yaml:"frame_options"`
	// ContentTypeOptions, when true, sets "X-Content-Type-Options: nosniff".
	ContentTypeOptions bool `yaml:"content_type_options"`
	// CSP sets the Content-Security-Policy header value.
	CSP string `yaml:"csp"`
	// ReferrerPolicy sets the Referrer-Policy header value.
	ReferrerPolicy string `yaml:"referrer_policy"`
}

// CORSConfig configures Cross-Origin Resource Sharing responses.
type CORSConfig struct {
	Enabled          bool     `yaml:"enabled"`
	AllowOrigins     []string `yaml:"allow_origins"`
	AllowMethods     []string `yaml:"allow_methods"`
	AllowHeaders     []string `yaml:"allow_headers"`
	AllowCredentials bool     `yaml:"allow_credentials"`
	MaxAge           int      `yaml:"max_age"`
}

// ACLConfig configures IP allow/deny lists (CIDRs), a method allowlist, and
// blocked path prefixes. Allow/Deny entries are CIDRs or bare IPs; Methods is an
// allowlist (empty => all methods); BlockedPaths are blocked path prefixes.
type ACLConfig struct {
	Allow        []string `yaml:"allow"`
	Deny         []string `yaml:"deny"`
	Methods      []string `yaml:"methods"`
	BlockedPaths []string `yaml:"blocked_paths"`
}

// AuthConfig configures optional client authentication. Type selects the scheme:
// "none" (default), "basic", "apikey", or "jwt".
type AuthConfig struct {
	// Type is the auth scheme: "none" (default) | "basic" | "apikey" | "jwt".
	Type string `yaml:"type"`
	// Users maps username -> password for Basic auth.
	Users map[string]string `yaml:"users"`
	// APIKeys is the set of accepted API keys for apikey auth.
	APIKeys []string `yaml:"api_keys"`
	// Header is the request header carrying the API key; default "X-API-Key".
	Header string `yaml:"header"`
	// JWTSecret is the HMAC secret used to verify JWTs (required for HS256).
	JWTSecret string `yaml:"jwt_secret"`
	// JWTAlg is the accepted JWT signing algorithm: "HS256" (default) or "RS256".
	JWTAlg string `yaml:"jwt_alg"`
	// JWTPublicKey is a PEM-encoded RSA public key used to verify RS256 JWTs.
	// For RS256, exactly one of JWTPublicKey or JWKSURL must be set.
	JWTPublicKey string `yaml:"jwt_public_key"`
	// JWKSURL is a JWKS endpoint from which RSA public keys are fetched to verify
	// RS256 JWTs. For RS256, exactly one of JWTPublicKey or JWKSURL must be set.
	JWKSURL string `yaml:"jwks_url"`
}

// validClientAuth is the set of accepted downstream ClientAuth modes.
var validClientAuth = map[string]bool{
	"none":               true,
	"request":            true,
	"require_and_verify": true,
}

// validAuthTypes is the set of accepted Security.Auth.Type values.
var validAuthTypes = map[string]bool{
	"none":   true,
	"basic":  true,
	"apikey": true,
	"jwt":    true,
}

// BackendConfig describes one upstream backend: URL, weight, connection cap, zone, tier, and optional per-backend health check.
type BackendConfig struct {
	URL      string `yaml:"url"`
	Weight   int    `yaml:"weight"`
	MaxConns int    `yaml:"max_conns"`
	// Zone is the availability zone/region this backend lives in, matched
	// against ServerConfig.Zone when LoadBalancer.PreferSameZone is enabled.
	Zone string `yaml:"zone"`
	// Tier orders backends into failover groups: 0 is primary, higher values are
	// backups that are only used when lower tiers are unavailable.
	Tier int `yaml:"tier"`
	// HealthCheck, when non-nil, overrides the global load_balancer.health_check
	// for this backend. Its per-field defaults are applied by Load().
	HealthCheck *HealthCheckConfig `yaml:"health_check"`
}

// LoadBalancerConfig selects the load-balancing algorithm and tunes health checks, session affinity, and advanced wrappers.
type LoadBalancerConfig struct {
	Algorithm   string            `yaml:"algorithm"`
	HealthCheck HealthCheckConfig `yaml:"health_check"`
	// GRPCHealth configures an optional dedicated gRPC Health Checking Protocol
	// server.  Disabled by default; its Port defaults to 9091 and Reflection to
	// true when enabled.
	GRPCHealth       GRPCHealthConfig       `yaml:"grpc_health"`
	ConsistentHash   ConsistentHashConfig   `yaml:"consistent_hash"`
	Sticky           StickyConfig           `yaml:"sticky"`
	SlowStart        time.Duration          `yaml:"slow_start"`
	OutlierDetection OutlierDetectionConfig `yaml:"outlier_detection"`
	PreferSameZone   bool                   `yaml:"prefer_same_zone"`
}

// ConsistentHashConfig tunes the consistent-hash / bounded-load ring.
type ConsistentHashConfig struct {
	// Replicas is the number of virtual nodes placed on the ring per backend.
	Replicas int `yaml:"replicas"`
	// LoadFactor caps how overloaded a backend may be relative to the mean
	// before keys spill to the next backend (bounded-load). Must be >= 1.0.
	LoadFactor float64 `yaml:"load_factor"`
}

// StickyConfig configures cookie-based session affinity.
type StickyConfig struct {
	Enabled bool          `yaml:"enabled"`
	Cookie  string        `yaml:"cookie"`
	TTL     time.Duration `yaml:"ttl"`
}

// OutlierDetectionConfig configures passive ejection of misbehaving backends.
type OutlierDetectionConfig struct {
	Enabled            bool          `yaml:"enabled"`
	ErrorRateThreshold float64       `yaml:"error_rate_threshold"`
	MinRequests        int           `yaml:"min_requests"`
	BaseEjection       time.Duration `yaml:"base_ejection"`
	MaxEjectionPercent int           `yaml:"max_ejection_percent"`
}

// GRPCHealthConfig configures the optional gRPC Health Checking Protocol endpoint.
// When Enabled, a dedicated gRPC server is started on Port (default 9091) exposing
// the grpc.health.v1.Health service.  Reflection enables gRPC Server Reflection so
// tools like grpcurl can introspect the server without a compiled proto.
type GRPCHealthConfig struct {
	Enabled    bool `yaml:"enabled"`    // expose gRPC Health Protocol endpoint
	Port       int  `yaml:"port"`       // default 9091
	Reflection bool `yaml:"reflection"` // enable gRPC server reflection
}

type HealthCheckConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
	Path     string        `yaml:"path"`
	// Type selects the probe kind: "http" (default) or "tcp".
	Type string `yaml:"type"`
	// Method is the HTTP method used for http-type probes; default "GET".
	Method string `yaml:"method"`
	// ExpectedStatuses lists acceptable HTTP status codes. Empty => any 2xx (200-299).
	ExpectedStatuses []int `yaml:"expected_statuses"`
	// ExpectedBody, when set, is a substring the response body must contain.
	ExpectedBody string `yaml:"expected_body"`
	// Host optionally overrides the Host header sent on http probes.
	Host string `yaml:"host"`
	// Headers are optional additional request headers for http probes.
	Headers map[string]string `yaml:"headers"`
	// HealthyThreshold is the number of consecutive successes required to mark a
	// backend healthy; default 2.
	HealthyThreshold int `yaml:"healthy_threshold"`
	// UnhealthyThreshold is the number of consecutive failures required to mark a
	// backend unhealthy; default 3.
	UnhealthyThreshold int `yaml:"unhealthy_threshold"`
	// Jitter is the fraction (0..1) of the interval randomized between probes;
	// default 0.1. Note an explicit 0 is treated as unset and becomes 0.1.
	Jitter float64 `yaml:"jitter"`
	// StartupGracePeriod delays counting the first check result after startup;
	// default 0.
	StartupGracePeriod time.Duration `yaml:"startup_grace_period"`
}

// applyGRPCHealthDefaults fills in the documented defaults for GRPCHealthConfig:
// Port defaults to 9091 and Reflection to true (always applied; the server stays
// disabled unless GRPCHealth.Enabled is explicitly set to true).
func applyGRPCHealthDefaults(g *GRPCHealthConfig) {
	if g.Port == 0 {
		g.Port = 9091
	}
	// Default Reflection to true so grpcurl / grpc_cli work out of the box.
	// Note: yaml.v3 cannot distinguish an explicit "reflection: false" from an
	// omitted field when the block is otherwise empty; in practice operators who
	// want to disable reflection should also set enabled: true.
	if !g.Enabled {
		// Leave reflection at its zero value (false) when the whole block is off;
		// it will be set to true below only when we are applying defaults for a
		// config that at least partially enables gRPC health.
		return
	}
	// When enabled, default Reflection to true.
	// We cannot detect "explicitly false" vs "omitted" (yaml.v3 zero-value
	// problem), so we unconditionally default to true here.  Operators who need
	// to disable reflection while keeping the health server enabled should set
	// reflection: false explicitly in the same block as enabled: true.
	g.Reflection = true
}

// applyHealthCheckDefaults fills in the documented per-field defaults for a
// HealthCheckConfig. It is used for both the global and per-backend configs.
// Note: an explicit Jitter of 0 becomes 0.1 (documented, acceptable).
func applyHealthCheckDefaults(hc *HealthCheckConfig) {
	if hc.Type == "" {
		hc.Type = "http"
	}
	if hc.Method == "" {
		hc.Method = "GET"
	}
	if hc.HealthyThreshold <= 0 {
		hc.HealthyThreshold = 2
	}
	if hc.UnhealthyThreshold <= 0 {
		hc.UnhealthyThreshold = 3
	}
	if hc.Jitter == 0 {
		hc.Jitter = 0.1
	}
}

// validFailureClasses is the set of failure classes accepted in trip_on.
var validFailureClasses = map[string]bool{
	"connect": true,
	"timeout": true,
	"5xx":     true,
}

// validRetryClasses is the set of failure classes accepted in retry_on. Only
// pre-response classes (connect/timeout) may be retried; 5xx responses have
// already been written to the client.
var validRetryClasses = map[string]bool{
	"connect": true,
	"timeout": true,
}

// applyCircuitBreakerDefaults fills in the documented circuit-breaker defaults.
// Mode defaults to "consecutive" (current behavior). The rolling-mode knobs are
// only meaningful in rolling mode but are always defaulted for predictability.
func applyCircuitBreakerDefaults(cb *CircuitBreakerConfig) {
	if cb.Mode == "" {
		cb.Mode = "consecutive"
	}
	if cb.RollingWindow <= 0 {
		cb.RollingWindow = 10 * time.Second
	}
	if cb.ErrorRateThreshold <= 0 {
		cb.ErrorRateThreshold = 0.5
	}
	if cb.MinRequests <= 0 {
		cb.MinRequests = 20
	}
	if len(cb.TripOn) == 0 {
		cb.TripOn = []string{"connect", "timeout"}
	}
}

// applyRetryDefaults fills in the documented retry defaults.
//
// HonorRetryAfter caveat: yaml.v3 cannot tell a bare "honor_retry_after: false"
// apart from an omitted field. We treat HonorRetryAfter as defaulting to true
// only when the whole retry block is zero-value; if any other retry field is
// set, the unmarshaled HonorRetryAfter (including an explicit false) is left
// untouched.
func applyRetryDefaults(r *RetryConfig) {
	if retryBlockIsZero(r) {
		r.HonorRetryAfter = true
	}
	if len(r.RetryOn) == 0 {
		r.RetryOn = []string{"connect", "timeout"}
	}
	if r.Hedge.Enabled && r.Hedge.MaxExtra <= 0 {
		r.Hedge.MaxExtra = 1
	}
}

// retryBlockIsZero reports whether the retry block was left entirely
// zero-valued (ignoring HonorRetryAfter, which is what we are deciding). This
// lets us default HonorRetryAfter to true only for a fully-omitted retry block.
func retryBlockIsZero(r *RetryConfig) bool {
	return r.MaxAttempts == 0 &&
		r.Backoff == "" &&
		r.MaxBackoff == 0 &&
		r.Budget == 0 &&
		r.PerTryTimeout == 0 &&
		len(r.RetryOn) == 0 &&
		r.Hedge == (HedgeConfig{})
}

// validateHealthCheck enforces the health-check field bounds. label is used to
// prefix errors (e.g. the global config or a specific backend).
func validateHealthCheck(hc HealthCheckConfig, label string) error {
	if hc.Jitter < 0 || hc.Jitter > 1 {
		return fmt.Errorf("config: %s.jitter %.3f out of range (0-1)", label, hc.Jitter)
	}
	if hc.Type != "http" && hc.Type != "tcp" {
		return fmt.Errorf("config: %s.type %q must be \"http\" or \"tcp\"", label, hc.Type)
	}
	if hc.HealthyThreshold < 1 {
		return fmt.Errorf("config: %s.healthy_threshold must be >= 1", label)
	}
	if hc.UnhealthyThreshold < 1 {
		return fmt.Errorf("config: %s.unhealthy_threshold must be >= 1", label)
	}
	for _, s := range hc.ExpectedStatuses {
		if s < 100 || s > 599 {
			return fmt.Errorf("config: %s.expected_statuses entry %d out of range (100-599)", label, s)
		}
	}
	return nil
}

// CircuitBreakerConfig tunes the per-backend circuit breaker (consecutive or rolling mode).
type CircuitBreakerConfig struct {
	Enabled          bool          `yaml:"enabled"`
	FailureThreshold int           `yaml:"failure_threshold"`
	SuccessThreshold int           `yaml:"success_threshold"`
	Timeout          time.Duration `yaml:"timeout"`
	// Mode selects the tripping strategy: "consecutive" (default, current
	// behavior) trips after FailureThreshold consecutive failures; "rolling"
	// trips on the error rate within RollingWindow.
	Mode string `yaml:"mode"`
	// RollingWindow is the time window used by rolling mode; default 10s.
	RollingWindow time.Duration `yaml:"rolling_window"`
	// ErrorRateThreshold is the failure fraction (0-1) that trips rolling mode;
	// default 0.5.
	ErrorRateThreshold float64 `yaml:"error_rate_threshold"`
	// MinRequests is the minimum number of requests in the window before rolling
	// mode may trip; default 20.
	MinRequests int `yaml:"min_requests"`
	// TripOn lists the failure classes counted as failures for circuit
	// accounting: a subset of {"connect","timeout","5xx"}; default
	// {"connect","timeout"}.
	TripOn []string `yaml:"trip_on"`
}

// RateLimiterConfig configures the rate limiter: algorithm, key, limits, and optional shared store.
type RateLimiterConfig struct {
	Enabled           bool `yaml:"enabled"`
	RequestsPerSecond int  `yaml:"requests_per_second"`
	Burst             int  `yaml:"burst"`
	// Algorithm selects the limiting strategy: "token_bucket" (default, current
	// behavior) or "gcra".
	Algorithm string `yaml:"algorithm"`
	// GlobalRPS/GlobalBurst configure the process-wide limiter independently of
	// the per-key defaults. When <= 0 they fall back to RequestsPerSecond/Burst.
	GlobalRPS   int `yaml:"global_rps"`
	GlobalBurst int `yaml:"global_burst"`
	// Key selects the limiting key: "ip" (default) or "header:<Name>" to key by
	// the named request header (an empty header value falls back to the IP).
	Key string `yaml:"key"`
	// RetryAfterSeconds is the Retry-After value sent on a 429; default 1.
	RetryAfterSeconds int `yaml:"retry_after_seconds"`
	// Message is the 429 response body; default "Rate limit exceeded".
	Message string `yaml:"message"`
	// Allowlist lists IPs/CIDRs exempt from limiting entirely.
	Allowlist []string `yaml:"allowlist"`
	// Rules are per-route overrides evaluated in order; the first match wins.
	Rules []RateLimitRule `yaml:"rules"`
	// SharedStore configures an optional distributed rate-limit Store (ENHANCEMENTS 4.4).
	// When enabled, all proxy instances sharing the same store enforce a combined limit.
	SharedStore SharedStoreConfig `yaml:"shared_store"`
}

// RateLimitRule overrides the per-key rps/burst for requests matching a route.
// An empty PathPrefix matches any path; an empty Method matches any method.
type RateLimitRule struct {
	PathPrefix string `yaml:"path_prefix"`
	Method     string `yaml:"method"`
	RPS        int    `yaml:"rps"`
	Burst      int    `yaml:"burst"`
}

// SharedStoreConfig configures the optional distributed rate-limit Store that
// enforces a combined limit across all proxy instances sharing a backend.
type SharedStoreConfig struct {
	Enabled bool `yaml:"enabled"`
	// Backend selects the store implementation: "memory" (default) or "redis".
	Backend string           `yaml:"backend"`
	Redis   RedisStoreConfig `yaml:"redis"`
	// Key is the shared namespace key used by the store; default "__global__".
	Key string `yaml:"key"`
}

// RedisStoreConfig tunes the Redis connection used by a redis-backed SharedStore.
type RedisStoreConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
	// Prefix is prepended to every rate-limit key stored in Redis; default "rplb:rl".
	Prefix string `yaml:"prefix"`
}

// validRateLimitAlgorithms is the set of accepted rate-limiter algorithm names.
var validRateLimitAlgorithms = map[string]bool{
	"token_bucket": true,
	"gcra":         true,
}

// applyRateLimiterDefaults fills in the documented rate-limiter defaults. The
// defaults keep current behavior: token_bucket, IP keying, and the global
// limiter sharing the per-key rps/burst.
func applyRateLimiterDefaults(rl *RateLimiterConfig) {
	if rl.Algorithm == "" {
		rl.Algorithm = "token_bucket"
	}
	if rl.Key == "" {
		rl.Key = "ip"
	}
	if rl.RetryAfterSeconds <= 0 {
		rl.RetryAfterSeconds = 1
	}
	if rl.Message == "" {
		rl.Message = "Rate limit exceeded"
	}
	if rl.GlobalRPS <= 0 {
		rl.GlobalRPS = rl.RequestsPerSecond
	}
	if rl.GlobalBurst <= 0 {
		rl.GlobalBurst = rl.Burst
	}
	if rl.SharedStore.Backend == "" {
		rl.SharedStore.Backend = "memory"
	}
	if rl.SharedStore.Key == "" {
		rl.SharedStore.Key = "__global__"
	}
	if rl.SharedStore.Redis.Prefix == "" {
		rl.SharedStore.Redis.Prefix = "rplb:rl"
	}
}

// RetryConfig controls how many times and on what errors the proxy retries a request.
type RetryConfig struct {
	MaxAttempts int           `yaml:"max_attempts"`
	Backoff     string        `yaml:"backoff"`
	MaxBackoff  time.Duration `yaml:"max_backoff"`
	// Budget caps retries as a fraction of requests; 0 = unlimited (current
	// behavior).
	Budget float64 `yaml:"budget"`
	// PerTryTimeout bounds each individual attempt; 0 = none.
	PerTryTimeout time.Duration `yaml:"per_try_timeout"`
	// HonorRetryAfter controls whether a Retry-After response header is honored;
	// default true.
	//
	// CAVEAT: yaml.v3 cannot distinguish an explicit "honor_retry_after: false"
	// from an omitted field (both unmarshal to the zero value false). We default
	// to true only when the ENTIRE retry block is zero-value; if any other retry
	// field is set, a bare false is respected. Therefore, to disable Retry-After
	// while leaving all other retry fields at their defaults, another retry field
	// must also be set explicitly (or accept that the default of true applies).
	HonorRetryAfter bool `yaml:"honor_retry_after"`
	// RetryOn lists failure classes retried before a response is produced: a
	// subset of {"connect","timeout"}; default {"connect","timeout"}.
	RetryOn []string `yaml:"retry_on"`
	// Hedge configures optional hedged (parallel speculative) requests.
	Hedge HedgeConfig `yaml:"hedge"`
}

// HedgeConfig configures hedged requests: after Delay, up to MaxExtra parallel
// speculative attempts are launched and the first response wins. Disabled by
// default.
type HedgeConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Delay    time.Duration `yaml:"delay"`
	MaxExtra int           `yaml:"max_extra"`
}

// LoggingConfig selects the log level and format.
type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// MetricsConfig enables the Prometheus/admin listener and tunes its bind address and auth.
type MetricsConfig struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
	// Host is the interface the metrics/admin endpoint binds to. Defaults to
	// 127.0.0.1 so admin surfaces are loopback-only unless explicitly opened.
	Host string `yaml:"host"`
	// AuthToken, when set, is required as a bearer token on admin/metrics
	// requests. Consumed by the server package; not enforced here.
	AuthToken string `yaml:"auth_token"`
}

// Load reads and validates the YAML config at path, applying defaults and environment overrides.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}

	if cfg.Server.ShutdownTimeout <= 0 {
		cfg.Server.ShutdownTimeout = 30 * time.Second
	}

	if cfg.Server.WatchConfig && cfg.Server.WatchInterval <= 0 {
		cfg.Server.WatchInterval = 5 * time.Second
	}

	if cfg.LoadBalancer.Algorithm == "" {
		cfg.LoadBalancer.Algorithm = "round_robin"
	}

	if cfg.LoadBalancer.ConsistentHash.Replicas == 0 {
		cfg.LoadBalancer.ConsistentHash.Replicas = 100
	}
	if cfg.LoadBalancer.ConsistentHash.LoadFactor == 0 {
		cfg.LoadBalancer.ConsistentHash.LoadFactor = 1.25
	}
	if cfg.LoadBalancer.Sticky.Cookie == "" {
		cfg.LoadBalancer.Sticky.Cookie = "rplb_affinity"
	}

	applyHealthCheckDefaults(&cfg.LoadBalancer.HealthCheck)
	applyGRPCHealthDefaults(&cfg.LoadBalancer.GRPCHealth)

	for i := range cfg.Backends {
		if cfg.Backends[i].Weight == 0 {
			cfg.Backends[i].Weight = 1
		}
		if cfg.Backends[i].MaxConns == 0 {
			cfg.Backends[i].MaxConns = 100
		}
		if cfg.Backends[i].HealthCheck != nil {
			applyHealthCheckDefaults(cfg.Backends[i].HealthCheck)
		}
	}

	for i := range cfg.Routes {
		r := &cfg.Routes[i]
		if r.Algorithm == "" {
			r.Algorithm = "round_robin"
		}
		if r.ConsistentHash.Replicas == 0 {
			r.ConsistentHash.Replicas = 100
		}
		if r.ConsistentHash.LoadFactor == 0 {
			r.ConsistentHash.LoadFactor = 1.25
		}
		for j := range r.Backends {
			if r.Backends[j].Weight == 0 {
				r.Backends[j].Weight = 1
			}
			if r.Backends[j].MaxConns == 0 {
				r.Backends[j].MaxConns = 100
			}
			if r.Backends[j].HealthCheck != nil {
				applyHealthCheckDefaults(r.Backends[j].HealthCheck)
			}
		}
	}

	applyCanaryDefaults(&cfg.Canary)
	applyFaultDefaults(&cfg.FaultInjection)
	applyCacheDefaults(&cfg.Cache)
	applyDiscoveryDefaults(&cfg.Discovery)

	if cfg.Metrics.Host == "" {
		cfg.Metrics.Host = "127.0.0.1"
	}

	if cfg.TLS.MinVersion == "" {
		cfg.TLS.MinVersion = "1.2"
	}
	if cfg.TLS.ClientAuth == "" {
		cfg.TLS.ClientAuth = "none"
	}
	if cfg.TLS.ACME.HTTPChallengePort == 0 {
		cfg.TLS.ACME.HTTPChallengePort = 80
	}
	if cfg.Security.Auth.Header == "" {
		cfg.Security.Auth.Header = "X-API-Key"
	}
	if cfg.Security.Auth.JWTAlg == "" {
		cfg.Security.Auth.JWTAlg = "HS256"
	}

	applyUpstreamDefaults(&cfg.Server.Upstream)
	applyL4Defaults(&cfg.Server.L4)

	// HTTP3: default the QUIC port to the main server port when not specified.
	if cfg.Server.HTTP3.Port == 0 {
		cfg.Server.HTTP3.Port = cfg.Server.Port
	}

	applyCircuitBreakerDefaults(&cfg.CircuitBreaker)
	applyRetryDefaults(&cfg.Retry)
	applyTracingDefaults(&cfg.Tracing)

	// Apply environment overrides after defaults but before validation so
	// overridden values are subject to the same validation as file values.
	applyEnvOverrides(&cfg)

	// Rate-limiter defaults are applied after env overrides so the global
	// rps/burst fallback observes any env-overridden RequestsPerSecond/Burst.
	applyRateLimiterDefaults(&cfg.RateLimiter)

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// applyEnvOverrides layers a documented subset of environment variables over the
// loaded configuration. Blank/unset variables are ignored; parse failures leave
// the existing (defaulted) value untouched. Supported variables:
//
//	RPLB_SERVER_HOST         -> Server.Host
//	RPLB_SERVER_PORT         -> Server.Port (int)
//	RPLB_LOG_LEVEL           -> Logging.Level
//	RPLB_METRICS_ENABLED     -> Metrics.Enabled (true/false)
//	RPLB_METRICS_PORT        -> Metrics.Port (int)
//	RPLB_RATE_LIMIT_ENABLED  -> RateLimiter.Enabled (true/false)
//	RPLB_RATE_LIMIT_RPS      -> RateLimiter.RequestsPerSecond (int)
//	RPLB_RATE_LIMIT_BURST    -> RateLimiter.Burst (int)
//	RPLB_BACKENDS            -> replaces Backends with a comma-separated URL list,
//	                            each backend defaulted (Weight 1, MaxConns 100)
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("RPLB_SERVER_HOST"); v != "" {
		cfg.Server.Host = v
	}
	if v := os.Getenv("RPLB_SERVER_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Server.Port = n
		}
	}
	if v := os.Getenv("RPLB_LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
	if v := os.Getenv("RPLB_METRICS_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Metrics.Enabled = b
		}
	}
	if v := os.Getenv("RPLB_METRICS_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Metrics.Port = n
		}
	}
	if v := os.Getenv("RPLB_RATE_LIMIT_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.RateLimiter.Enabled = b
		}
	}
	if v := os.Getenv("RPLB_RATE_LIMIT_RPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.RateLimiter.RequestsPerSecond = n
		}
	}
	if v := os.Getenv("RPLB_RATE_LIMIT_BURST"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.RateLimiter.Burst = n
		}
	}
	if v := os.Getenv("RPLB_BACKENDS"); v != "" {
		var backends []BackendConfig
		for _, raw := range strings.Split(v, ",") {
			u := strings.TrimSpace(raw)
			if u == "" {
				continue
			}
			backends = append(backends, BackendConfig{
				URL:      u,
				Weight:   1,
				MaxConns: 100,
			})
		}
		if len(backends) > 0 {
			cfg.Backends = backends
		}
	}
}

// validateUpstream enforces the upstream transport bounds: all timeouts and
// connection caps must be non-negative.
func validateUpstream(u UpstreamConfig) error {
	if u.DialTimeout < 0 {
		return fmt.Errorf("config: server.upstream.dial_timeout must be >= 0")
	}
	if u.TLSHandshakeTimeout < 0 {
		return fmt.Errorf("config: server.upstream.tls_handshake_timeout must be >= 0")
	}
	if u.ResponseHeaderTimeout < 0 {
		return fmt.Errorf("config: server.upstream.response_header_timeout must be >= 0")
	}
	if u.ExpectContinueTimeout < 0 {
		return fmt.Errorf("config: server.upstream.expect_continue_timeout must be >= 0")
	}
	if u.IdleConnTimeout < 0 {
		return fmt.Errorf("config: server.upstream.idle_conn_timeout must be >= 0")
	}
	if u.MaxIdleConns < 0 {
		return fmt.Errorf("config: server.upstream.max_idle_conns must be >= 0")
	}
	if u.MaxIdleConnsPerHost < 0 {
		return fmt.Errorf("config: server.upstream.max_idle_conns_per_host must be >= 0")
	}
	if u.MaxConnsPerHost < 0 {
		return fmt.Errorf("config: server.upstream.max_conns_per_host must be >= 0")
	}
	return nil
}

// validateL4 enforces the L4 proxy bounds: a non-negative dial timeout and, when
// enabled, a port in the valid range.
func validateL4(l L4Config) error {
	if l.DialTimeout < 0 {
		return fmt.Errorf("config: server.l4.dial_timeout must be >= 0")
	}
	if l.Enabled && (l.Port < 1 || l.Port > 65535) {
		return fmt.Errorf("config: server.l4.port %d out of range (1-65535)", l.Port)
	}
	return nil
}

// validateWebSocket enforces that the WebSocket knobs are non-negative.
func validateWebSocket(w WebSocketConfig) error {
	if w.IdleTimeout < 0 {
		return fmt.Errorf("config: server.websocket.idle_timeout must be >= 0")
	}
	if w.MaxMessageBytes < 0 {
		return fmt.Errorf("config: server.websocket.max_message_bytes must be >= 0")
	}
	return nil
}

// validate rejects configurations that would silently misbehave at runtime.
func (c *Config) validate() error {
	if len(c.Backends) == 0 {
		return fmt.Errorf("config: at least one backend is required")
	}

	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("config: server.port %d out of range (1-65535)", c.Server.Port)
	}

	if c.Server.WatchInterval < 0 {
		return fmt.Errorf("config: server.watch_interval must be >= 0")
	}

	if err := validateUpstream(c.Server.Upstream); err != nil {
		return err
	}
	if err := validateL4(c.Server.L4); err != nil {
		return err
	}
	if err := validateWebSocket(c.Server.WebSocket); err != nil {
		return err
	}

	if !validAlgorithms[c.LoadBalancer.Algorithm] {
		return fmt.Errorf("config: unknown load_balancer.algorithm %q", c.LoadBalancer.Algorithm)
	}

	for i, b := range c.Backends {
		u, err := url.Parse(b.URL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("config: backend[%d] has invalid url %q", i, b.URL)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("config: backend[%d] url %q must be http or https", i, b.URL)
		}
		if b.Weight < 0 {
			return fmt.Errorf("config: backend[%d] weight must be >= 0", i)
		}
		if b.MaxConns < 0 {
			return fmt.Errorf("config: backend[%d] max_conns must be >= 0", i)
		}
	}

	if c.RateLimiter.Enabled && c.RateLimiter.RequestsPerSecond <= 0 {
		return fmt.Errorf("config: rate_limiter.requests_per_second must be > 0 when enabled")
	}

	if !validRateLimitAlgorithms[c.RateLimiter.Algorithm] {
		return fmt.Errorf("config: rate_limiter.algorithm %q must be \"token_bucket\" or \"gcra\"",
			c.RateLimiter.Algorithm)
	}
	if k := c.RateLimiter.Key; k != "ip" {
		if !strings.HasPrefix(k, "header:") || strings.TrimPrefix(k, "header:") == "" {
			return fmt.Errorf("config: rate_limiter.key %q must be \"ip\" or \"header:<name>\"", k)
		}
	}
	if c.RateLimiter.RetryAfterSeconds < 0 {
		return fmt.Errorf("config: rate_limiter.retry_after_seconds must be >= 0")
	}
	for i, entry := range c.RateLimiter.Allowlist {
		if _, _, err := net.ParseCIDR(entry); err != nil {
			if net.ParseIP(entry) == nil {
				return fmt.Errorf("config: rate_limiter.allowlist[%d] %q is not a valid IP or CIDR", i, entry)
			}
		}
	}
	if c.RateLimiter.Enabled {
		for i, rule := range c.RateLimiter.Rules {
			if rule.RPS <= 0 {
				return fmt.Errorf("config: rate_limiter.rules[%d].rps must be > 0 when enabled", i)
			}
			if rule.Burst <= 0 {
				return fmt.Errorf("config: rate_limiter.rules[%d].burst must be > 0 when enabled", i)
			}
		}
	}
	if ss := c.RateLimiter.SharedStore; ss.Enabled {
		if ss.Backend != "memory" && ss.Backend != "redis" {
			return fmt.Errorf("config: rate_limiter.shared_store.backend %q must be \"memory\" or \"redis\"", ss.Backend)
		}
		if ss.Backend == "redis" && ss.Redis.Addr == "" {
			return fmt.Errorf("config: rate_limiter.shared_store.redis.addr is required when backend is \"redis\"")
		}
	}

	if c.Metrics.Enabled && (c.Metrics.Port < 1 || c.Metrics.Port > 65535) {
		return fmt.Errorf("config: metrics.port %d out of range (1-65535)", c.Metrics.Port)
	}

	if c.LoadBalancer.GRPCHealth.Enabled && (c.LoadBalancer.GRPCHealth.Port < 1 || c.LoadBalancer.GRPCHealth.Port > 65535) {
		return fmt.Errorf("config: load_balancer.grpc_health.port %d out of range (1-65535)", c.LoadBalancer.GRPCHealth.Port)
	}

	if c.LoadBalancer.ConsistentHash.LoadFactor != 0 && c.LoadBalancer.ConsistentHash.LoadFactor < 1.0 {
		return fmt.Errorf("config: load_balancer.consistent_hash.load_factor %.3f must be >= 1.0",
			c.LoadBalancer.ConsistentHash.LoadFactor)
	}
	if c.LoadBalancer.ConsistentHash.Replicas < 0 {
		return fmt.Errorf("config: load_balancer.consistent_hash.replicas must be >= 0")
	}

	if c.LoadBalancer.Sticky.TTL < 0 {
		return fmt.Errorf("config: load_balancer.sticky.ttl must be >= 0")
	}

	if c.LoadBalancer.SlowStart < 0 {
		return fmt.Errorf("config: load_balancer.slow_start must be >= 0")
	}

	if c.LoadBalancer.OutlierDetection.Enabled {
		od := c.LoadBalancer.OutlierDetection
		if od.MaxEjectionPercent < 0 || od.MaxEjectionPercent > 100 {
			return fmt.Errorf("config: load_balancer.outlier_detection.max_ejection_percent %d out of range (0-100)",
				od.MaxEjectionPercent)
		}
		if od.ErrorRateThreshold < 0 || od.ErrorRateThreshold > 1 {
			return fmt.Errorf("config: load_balancer.outlier_detection.error_rate_threshold %.3f out of range (0-1)",
				od.ErrorRateThreshold)
		}
		if od.MinRequests < 0 {
			return fmt.Errorf("config: load_balancer.outlier_detection.min_requests must be >= 0")
		}
	}

	for i, b := range c.Backends {
		if b.Tier < 0 {
			return fmt.Errorf("config: backend[%d] tier must be >= 0", i)
		}
		if b.HealthCheck != nil {
			label := fmt.Sprintf("backend[%d].health_check", i)
			if err := validateHealthCheck(*b.HealthCheck, label); err != nil {
				return err
			}
		}
	}

	if err := validateHealthCheck(c.LoadBalancer.HealthCheck, "load_balancer.health_check"); err != nil {
		return err
	}

	if c.CircuitBreaker.Mode != "consecutive" && c.CircuitBreaker.Mode != "rolling" {
		return fmt.Errorf("config: circuit_breaker.mode %q must be \"consecutive\" or \"rolling\"",
			c.CircuitBreaker.Mode)
	}
	if c.CircuitBreaker.ErrorRateThreshold < 0 || c.CircuitBreaker.ErrorRateThreshold > 1 {
		return fmt.Errorf("config: circuit_breaker.error_rate_threshold %.3f out of range (0-1)",
			c.CircuitBreaker.ErrorRateThreshold)
	}
	for _, class := range c.CircuitBreaker.TripOn {
		if !validFailureClasses[class] {
			return fmt.Errorf("config: circuit_breaker.trip_on entry %q must be one of connect,timeout,5xx", class)
		}
	}

	if c.Retry.Budget < 0 {
		return fmt.Errorf("config: retry.budget must be >= 0")
	}
	for _, class := range c.Retry.RetryOn {
		if !validRetryClasses[class] {
			return fmt.Errorf("config: retry.retry_on entry %q must be one of connect,timeout", class)
		}
	}
	if c.Retry.Hedge.Delay < 0 {
		return fmt.Errorf("config: retry.hedge.delay must be >= 0")
	}

	if err := c.validateTLS(); err != nil {
		return err
	}
	if err := c.validateHTTP3(); err != nil {
		return err
	}
	if err := c.validateSecurity(); err != nil {
		return err
	}

	if err := c.validateRoutes(); err != nil {
		return err
	}

	if err := c.validateCanary(); err != nil {
		return err
	}
	if err := c.validateMirror(); err != nil {
		return err
	}
	if err := c.validateFaultInjection(); err != nil {
		return err
	}
	if err := c.validateCache(); err != nil {
		return err
	}
	if err := c.validateDiscovery(); err != nil {
		return err
	}

	if err := c.validateTracing(); err != nil {
		return err
	}

	return nil
}

// validateCache enforces the cache contract: when Enabled, DefaultTTL, MaxEntries,
// and MaxBodyBytes must all be > 0 (they are defaulted, so this guards against an
// explicit non-positive override) and Methods must be non-empty.
func (c *Config) validateCache() error {
	if !c.Cache.Enabled {
		return nil
	}
	if c.Cache.DefaultTTL <= 0 {
		return fmt.Errorf("config: cache.default_ttl must be > 0 when enabled")
	}
	if c.Cache.MaxEntries <= 0 {
		return fmt.Errorf("config: cache.max_entries must be > 0 when enabled")
	}
	if c.Cache.MaxBodyBytes <= 0 {
		return fmt.Errorf("config: cache.max_body_bytes must be > 0 when enabled")
	}
	if len(c.Cache.Methods) == 0 {
		return fmt.Errorf("config: cache.methods must be non-empty when enabled")
	}
	return nil
}

// validateDiscovery enforces the DNS-discovery contract: each target needs a
// non-empty Name, a Type in {a,srv}, a Scheme in {http,https}, and an Interval > 0
// (defaulted to 30s). A-type targets require a valid Port.
func (c *Config) validateDiscovery() error {
	for i, t := range c.Discovery.DNS {
		if t.Name == "" {
			return fmt.Errorf("config: discovery.dns[%d].name is required", i)
		}
		if t.Type != "a" && t.Type != "srv" {
			return fmt.Errorf("config: discovery.dns[%d].type %q must be \"a\" or \"srv\"", i, t.Type)
		}
		if t.Scheme != "http" && t.Scheme != "https" {
			return fmt.Errorf("config: discovery.dns[%d].scheme %q must be \"http\" or \"https\"", i, t.Scheme)
		}
		if t.Interval <= 0 {
			return fmt.Errorf("config: discovery.dns[%d].interval must be > 0", i)
		}
		if t.Type == "a" && (t.Port < 1 || t.Port > 65535) {
			return fmt.Errorf("config: discovery.dns[%d].port %d out of range (1-65535) for type \"a\"", i, t.Port)
		}
		if t.Weight < 0 {
			return fmt.Errorf("config: discovery.dns[%d].weight must be >= 0", i)
		}
		if t.MaxConns < 0 {
			return fmt.Errorf("config: discovery.dns[%d].max_conns must be >= 0", i)
		}
	}
	return nil
}

// validatePercent enforces that a percentage value is within 0..100.
func validatePercent(v int, label string) error {
	if v < 0 || v > 100 {
		return fmt.Errorf("config: %s %d out of range (0-100)", label, v)
	}
	return nil
}

// validateHTTPURL enforces that a URL is a valid absolute http(s) URL.
func validateHTTPURL(raw, label string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("config: %s has invalid url %q", label, raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("config: %s url %q must be http or https", label, raw)
	}
	return nil
}

// validateCanary enforces the canary contract: WeightPercent in 0..100, a known
// algorithm, and (when enabled) at least one backend with a valid http(s) URL and
// non-negative weight/max_conns/tier, plus consistent-hash bounds.
func (c *Config) validateCanary() error {
	if err := validatePercent(c.Canary.WeightPercent, "canary.weight_percent"); err != nil {
		return err
	}
	if !validAlgorithms[c.Canary.Algorithm] {
		return fmt.Errorf("config: unknown canary.algorithm %q", c.Canary.Algorithm)
	}
	if c.Canary.ConsistentHash.LoadFactor != 0 && c.Canary.ConsistentHash.LoadFactor < 1.0 {
		return fmt.Errorf("config: canary.consistent_hash.load_factor %.3f must be >= 1.0",
			c.Canary.ConsistentHash.LoadFactor)
	}
	if c.Canary.ConsistentHash.Replicas < 0 {
		return fmt.Errorf("config: canary.consistent_hash.replicas must be >= 0")
	}
	if c.Canary.Enabled && len(c.Canary.Backends) == 0 {
		return fmt.Errorf("config: canary.backends must have at least one backend when enabled")
	}
	for i, b := range c.Canary.Backends {
		if err := validateHTTPURL(b.URL, fmt.Sprintf("canary.backend[%d]", i)); err != nil {
			return err
		}
		if b.Weight < 0 {
			return fmt.Errorf("config: canary.backend[%d] weight must be >= 0", i)
		}
		if b.MaxConns < 0 {
			return fmt.Errorf("config: canary.backend[%d] max_conns must be >= 0", i)
		}
		if b.Tier < 0 {
			return fmt.Errorf("config: canary.backend[%d] tier must be >= 0", i)
		}
		if b.HealthCheck != nil {
			if err := validateHealthCheck(*b.HealthCheck, fmt.Sprintf("canary.backend[%d].health_check", i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateMirror enforces the mirror contract: SamplePercent in 0..100, a
// non-negative timeout, and (when enabled) a valid http(s) URL.
func (c *Config) validateMirror() error {
	if err := validatePercent(c.Mirror.SamplePercent, "mirror.sample_percent"); err != nil {
		return err
	}
	if c.Mirror.Timeout < 0 {
		return fmt.Errorf("config: mirror.timeout must be >= 0")
	}
	if c.Mirror.Enabled {
		if err := validateHTTPURL(c.Mirror.URL, "mirror"); err != nil {
			return err
		}
	}
	return nil
}

// validateFaultInjection enforces the fault-injection contract: DelayPercent and
// AbortPercent in 0..100, a non-negative delay, and (when enabled) an AbortStatus
// that is a valid HTTP status code.
func (c *Config) validateFaultInjection() error {
	if err := validatePercent(c.FaultInjection.DelayPercent, "fault_injection.delay_percent"); err != nil {
		return err
	}
	if err := validatePercent(c.FaultInjection.AbortPercent, "fault_injection.abort_percent"); err != nil {
		return err
	}
	if c.FaultInjection.Delay < 0 {
		return fmt.Errorf("config: fault_injection.delay must be >= 0")
	}
	if c.FaultInjection.Enabled {
		if s := c.FaultInjection.AbortStatus; s < 100 || s > 599 {
			return fmt.Errorf("config: fault_injection.abort_status %d must be a valid HTTP status (100-599)", s)
		}
	}
	return nil
}

// knownCipherSuites returns the set of cipher-suite names Go recognizes,
// including both secure and insecure suites.
func knownCipherSuites() map[string]bool {
	names := make(map[string]bool)
	for _, cs := range tls.CipherSuites() {
		names[cs.Name] = true
	}
	for _, cs := range tls.InsecureCipherSuites() {
		names[cs.Name] = true
	}
	return names
}

// validateTLS enforces the server TLS contract: MinVersion in {1.2,1.3},
// ClientAuth in {none,request,require_and_verify}, known cipher-suite names, and
// a ClientCAFile when ClientAuth requires verification.
func (c *Config) validateTLS() error {
	if c.TLS.MinVersion != "1.2" && c.TLS.MinVersion != "1.3" {
		return fmt.Errorf("config: tls.min_version %q must be \"1.2\" or \"1.3\"", c.TLS.MinVersion)
	}
	if !validClientAuth[c.TLS.ClientAuth] {
		return fmt.Errorf("config: tls.client_auth %q must be one of none,request,require_and_verify", c.TLS.ClientAuth)
	}
	if len(c.TLS.CipherSuites) > 0 {
		known := knownCipherSuites()
		for _, name := range c.TLS.CipherSuites {
			if !known[name] {
				return fmt.Errorf("config: tls.cipher_suites entry %q is not a known Go cipher suite", name)
			}
		}
	}
	if c.TLS.ClientAuth == "require_and_verify" && c.TLS.ClientCAFile == "" {
		return fmt.Errorf("config: tls.client_ca_file is required when tls.client_auth is require_and_verify")
	}
	if c.TLS.ACME.Enabled {
		if len(c.TLS.ACME.Domains) == 0 {
			return fmt.Errorf("config: tls.acme.domains must have at least one domain when acme is enabled")
		}
		for i, d := range c.TLS.ACME.Domains {
			if strings.TrimSpace(d) == "" {
				return fmt.Errorf("config: tls.acme.domains[%d] must not be empty", i)
			}
		}
		if c.TLS.ACME.HTTPChallengePort < 1 || c.TLS.ACME.HTTPChallengePort > 65535 {
			return fmt.Errorf("config: tls.acme.http_challenge_port %d out of range (1-65535)", c.TLS.ACME.HTTPChallengePort)
		}
		if c.TLS.ACME.DirectoryURL != "" {
			if err := validateHTTPURL(c.TLS.ACME.DirectoryURL, "tls.acme.directory_url"); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateHTTP3 enforces the HTTP/3 config contract: HTTP/3 requires TLS to be
// configured (QUIC mandates TLS 1.3), and the port must be in range when enabled.
func (c *Config) validateHTTP3() error {
	if !c.Server.HTTP3.Enabled {
		return nil
	}
	if !c.TLS.Enabled {
		return fmt.Errorf("config: http3 requires tls to be configured")
	}
	if c.Server.HTTP3.Port < 1 || c.Server.HTTP3.Port > 65535 {
		return fmt.Errorf("config: server.http3.port %d out of range (1-65535)", c.Server.HTTP3.Port)
	}
	return nil
}

// validateSecurity enforces the security middleware contract: a known Auth.Type,
// a JWT secret when Type is jwt (for HS256), and parseable ACL Allow/Deny CIDRs.
func (c *Config) validateSecurity() error {
	if !validAuthTypes[c.Security.Auth.Type] && c.Security.Auth.Type != "" {
		return fmt.Errorf("config: security.auth.type %q must be one of none,basic,apikey,jwt", c.Security.Auth.Type)
	}
	if c.Security.Auth.Type == "jwt" {
		switch c.Security.Auth.JWTAlg {
		case "HS256":
			if c.Security.Auth.JWTSecret == "" {
				return fmt.Errorf("config: security.auth.jwt_secret is required when security.auth.type is jwt and jwt_alg is HS256")
			}
		case "RS256":
			hasKey := c.Security.Auth.JWTPublicKey != ""
			hasJWKS := c.Security.Auth.JWKSURL != ""
			if hasKey == hasJWKS {
				return fmt.Errorf("config: security.auth requires exactly one of jwt_public_key or jwks_url when jwt_alg is RS256")
			}
			if hasJWKS {
				if err := validateHTTPURL(c.Security.Auth.JWKSURL, "security.auth.jwks_url"); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("config: security.auth.jwt_alg %q must be \"HS256\" or \"RS256\"", c.Security.Auth.JWTAlg)
		}
	}
	for i, entry := range c.Security.ACL.Allow {
		if _, _, err := net.ParseCIDR(entry); err != nil {
			if net.ParseIP(entry) == nil {
				return fmt.Errorf("config: security.acl.allow[%d] %q is not a valid IP or CIDR", i, entry)
			}
		}
	}
	for i, entry := range c.Security.ACL.Deny {
		if _, _, err := net.ParseCIDR(entry); err != nil {
			if net.ParseIP(entry) == nil {
				return fmt.Errorf("config: security.acl.deny[%d] %q is not a valid IP or CIDR", i, entry)
			}
		}
	}
	return nil
}

// validateRoutes enforces the per-route contract: unique names (when set), at
// least one match criterion or an explicit catch-all, a known algorithm, and at
// least one backend with a valid http(s) URL and non-negative weight/max_conns.
//
// A route with no match criteria (no host, path_prefix, methods, or headers) is
// a documented catch-all: it matches every request, so any route configured
// after it is unreachable. This is permitted (the first such route acts as a
// per-route default) and left to the operator's judgment.
func (c *Config) validateRoutes() error {
	seen := make(map[string]bool, len(c.Routes))
	for i := range c.Routes {
		r := &c.Routes[i]

		if r.Name != "" {
			if seen[r.Name] {
				return fmt.Errorf("config: routes[%d] duplicate name %q", i, r.Name)
			}
			seen[r.Name] = true
		}

		label := fmt.Sprintf("routes[%d]", i)
		if r.Name != "" {
			label = fmt.Sprintf("route %q", r.Name)
		}

		if !validAlgorithms[r.Algorithm] {
			return fmt.Errorf("config: %s unknown algorithm %q", label, r.Algorithm)
		}

		if len(r.Backends) == 0 {
			return fmt.Errorf("config: %s must have at least one backend", label)
		}
		for j, b := range r.Backends {
			u, err := url.Parse(b.URL)
			if err != nil || u.Scheme == "" || u.Host == "" {
				return fmt.Errorf("config: %s backend[%d] has invalid url %q", label, j, b.URL)
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				return fmt.Errorf("config: %s backend[%d] url %q must be http or https", label, j, b.URL)
			}
			if b.Weight < 0 {
				return fmt.Errorf("config: %s backend[%d] weight must be >= 0", label, j)
			}
			if b.MaxConns < 0 {
				return fmt.Errorf("config: %s backend[%d] max_conns must be >= 0", label, j)
			}
			if b.Tier < 0 {
				return fmt.Errorf("config: %s backend[%d] tier must be >= 0", label, j)
			}
			if b.HealthCheck != nil {
				hcLabel := fmt.Sprintf("%s backend[%d].health_check", label, j)
				if err := validateHealthCheck(*b.HealthCheck, hcLabel); err != nil {
					return err
				}
			}
		}

		if r.ConsistentHash.LoadFactor != 0 && r.ConsistentHash.LoadFactor < 1.0 {
			return fmt.Errorf("config: %s consistent_hash.load_factor %.3f must be >= 1.0",
				label, r.ConsistentHash.LoadFactor)
		}
		if r.ConsistentHash.Replicas < 0 {
			return fmt.Errorf("config: %s consistent_hash.replicas must be >= 0", label)
		}
	}
	return nil
}

// validateTracing enforces the tracing contract: SampleRate must be in [0.0,1.0].
func (c *Config) validateTracing() error {
	t := c.Tracing
	if t.SampleRate < 0 || t.SampleRate > 1.0 {
		return fmt.Errorf("config: tracing.sample_rate %.3f out of range (0.0-1.0)", t.SampleRate)
	}
	return nil
}

// GetAddr returns the host:port string the HTTP server listens on.
func (c *Config) GetAddr() string {
	return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port)
}

// ClientTLSConfig builds the *tls.Config used for connecting to https:// backends,
// or nil if no customization is needed (default system roots, full verification).
//
// When ClientCertFile/ClientKeyFile are both set, the keypair is loaded into
// Certificates so the proxy can present a client certificate for mTLS to backends.
func (b BackendTLSConfig) ClientTLSConfig() (*tls.Config, error) {
	hasClientCert := b.ClientCertFile != "" && b.ClientKeyFile != ""
	if !b.InsecureSkipVerify && b.CAFile == "" && !hasClientCert {
		return nil, nil
	}

	cfg := &tls.Config{InsecureSkipVerify: b.InsecureSkipVerify} //nolint:gosec // opt-in for testing

	if b.CAFile != "" {
		pem, err := os.ReadFile(b.CAFile)
		if err != nil {
			return nil, fmt.Errorf("backend_tls: failed to read ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("backend_tls: ca_file %q contained no valid certificates", b.CAFile)
		}
		cfg.RootCAs = pool
	}

	if hasClientCert {
		cert, err := tls.LoadX509KeyPair(b.ClientCertFile, b.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("backend_tls: failed to load client keypair: %w", err)
		}
		cfg.Certificates = append(cfg.Certificates, cert)
	}

	return cfg, nil
}
