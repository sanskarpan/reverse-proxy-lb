package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"hash/fnv"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/circuit"
	"reverse-proxy-lb/internal/config"
	"reverse-proxy-lb/internal/logging"
	"reverse-proxy-lb/internal/metrics"
	"reverse-proxy-lb/internal/netutil"
	"reverse-proxy-lb/internal/randutil"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/http2"
)

type Proxy struct {
	balancer       balancer.Balancer
	circuitBreaker *circuit.CircuitBreaker
	metrics        *metrics.Metrics
	retryConfig    config.RetryConfig
	transport      http.RoundTripper
	algorithm      string
	trusted        []*net.IPNet

	// upstream holds the config-driven upstream transport tuning (timeouts, pool
	// sizes, HTTP/2). Each per-backend ReverseProxy builds its OWN *http.Transport
	// from this so MaxIdleConnsPerHost / MaxConnsPerHost apply per backend.
	upstream config.UpstreamConfig
	// backendTLS is the TLS config applied to every per-backend transport for
	// https:// backends (preserved across per-backend pools).
	backendTLS *tls.Config
	// websocket holds opt-in WebSocket idle-timeout / max-message limits enforced on
	// hijacked (Upgrade) connections. Zero-value = unlimited (current behavior).
	websocket config.WebSocketConfig

	// tripOn is the set of failure classes ("connect","timeout","5xx") that count
	// as a circuit-breaker failure. It defaults to {"connect","timeout"} (current
	// behavior) and is configured via SetTripOn. A class not in the set is treated
	// as a circuit success for accounting purposes.
	tripOn map[string]bool
	// retryOn is the set of pre-response failure classes ("connect","timeout") that
	// are eligible for retry/failover. Defaults to {"connect","timeout"}.
	retryOn map[string]bool

	// sticky holds cookie-based session-affinity configuration. When
	// sticky.Enabled is true, the proxy pins a client to a backend via an
	// affinity cookie (see selectBackend / setStickyCookie).
	sticky config.StickyConfig

	// Retry-budget accounting (opt-in via RetryConfig.Budget > 0). retryReqs counts
	// requests that reached attemptBackend; retryCount counts retries actually made.
	// A retry is permitted only while retryCount/retryReqs stays within Budget, with
	// a small constant floor so low-traffic still retries.
	retryReqs  uint64
	retryCount uint64

	// Opt-in feature counters (kept local to the proxy so the shared metrics
	// package is not modified). Surfaced via the Get* accessors below.
	budgetDenied uint64
	rejections   uint64
	hedged       uint64
	hedgeWins    uint64

	// rng drives full-jitter backoff. It is seedable/injectable for deterministic
	// tests via SetRand; access is serialized by randMu because math/rand's default
	// source is not safe for concurrent use here (we use a private source).
	randMu sync.Mutex
	rng    *rand.Rand

	// router, when non-nil, selects the per-request balancer (L7 routing). When nil
	// the proxy uses p.balancer exactly as before, preserving single-balancer
	// behavior. It is set opt-in via SetRouter; the server passes an adapter around
	// routing.Router. The interface is defined here (rather than importing routing)
	// to avoid an import cycle.
	router Router

	// canary is an optional balancer for a canary backend pool, with canaryWeight
	// being the percentage (0..100) of requests routed to it. Both are set opt-in via
	// SetCanary. When canary is nil or canaryWeight <= 0 the proxy behaves exactly as
	// before (all traffic through the normal router/p.balancer path). When a request's
	// weighted-random dice lands in the canary fraction, the canary balancer is pinned
	// on the request context (via balancerCtxKey) so selection, in-group failover,
	// observers and sticky all operate consistently on the canary group for the WHOLE
	// request — reusing the same context-pinning mechanism as the L7 router.
	canary       balancer.Balancer
	canaryWeight int

	mu      sync.RWMutex
	proxies map[string]*httputil.ReverseProxy
}

// Router selects the balancer.Balancer that should serve a given request (L7
// routing). It is an interface (defined by the proxy) so the routing package can
// import the balancer package without the proxy importing routing, avoiding an
// import cycle. The server wires an adapter around *routing.Router.
type Router interface {
	Route(*http.Request) balancer.Balancer
}

// ctxKey is a private context key type so it cannot collide with other packages.
type ctxKey int

const (
	errCtxKey ctxKey = iota
	// balancerCtxKey stashes the routed balancer for THIS request so downstream
	// helpers (observe/failover/sticky) operate on the same group even when they
	// only receive the *http.Request.
	balancerCtxKey
)

// Upstream transport timeouts. These bound how long the proxy waits on a slow or
// unresponsive backend at each stage of a request so that a single bad backend
// cannot pin front-end goroutines/connections indefinitely.
const (
	dialTimeout           = 5 * time.Second
	dialKeepAlive         = 30 * time.Second
	tlsHandshakeTimeout   = 5 * time.Second
	responseHeaderTimeout = 30 * time.Second
	expectContinueTimeout = 1 * time.Second
	idleConnTimeout       = 90 * time.Second
	maxIdleConns          = 100
	maxIdleConnsPerHost   = 100
)

// retryBudgetFloor is the small constant number of retries always permitted
// regardless of the budget ratio, so low-traffic proxies still get retries.
const retryBudgetFloor = 10

// errCapture carries the upstream transport error out of the ReverseProxy ErrorHandler.
type errCapture struct{ err error }

// bufferPool is a shared httputil.BufferPool: a sync.Pool of 32 KiB copy buffers
// reused across proxied requests to cut per-request allocations on the streaming
// hot path (ENHANCEMENTS §10.2).
var bufferPool = &syncBufferPool{
	pool: sync.Pool{New: func() any { b := make([]byte, 32*1024); return &b }},
}

type syncBufferPool struct{ pool sync.Pool }

func (p *syncBufferPool) Get() []byte  { return *(p.pool.Get().(*[]byte)) }
func (p *syncBufferPool) Put(b []byte) { p.pool.Put(&b) }

// New builds a Proxy.
//
// Signature (for the integrator):
//
//	func New(b balancer.Balancer, cb *circuit.CircuitBreaker, retryCfg config.RetryConfig,
//	         algorithm string, trusted []*net.IPNet, backendTLS *tls.Config,
//	         upstream config.UpstreamConfig) *Proxy
//
// The trailing upstream parameter is new (§5.1): it supplies config-driven upstream
// transport tuning (timeouts, pool sizes, HTTP/2). A zero-value config.UpstreamConfig
// reproduces the previous hardcoded behavior exactly, so existing callers only need to
// pass config.UpstreamConfig{}.
func New(b balancer.Balancer, cb *circuit.CircuitBreaker, retryCfg config.RetryConfig, algorithm string, trusted []*net.IPNet, backendTLS *tls.Config, upstream config.UpstreamConfig) *Proxy {
	p := &Proxy{
		balancer:       b,
		circuitBreaker: cb,
		metrics:        metrics.New(),
		retryConfig:    retryCfg,
		algorithm:      algorithm,
		trusted:        trusted,
		upstream:       upstream,
		backendTLS:     backendTLS,
		proxies:        make(map[string]*httputil.ReverseProxy),
		tripOn:         toSet([]string{"connect", "timeout"}),
		retryOn:        toSet([]string{"connect", "timeout"}),
		rng:            randutil.NewRand(), // #nosec G404 -- non-crypto canary/fault-injection selection
	}
	// A representative (host-agnostic) transport is retained on p.transport so
	// existing introspection (and the §0 timeout-hardening test) keeps working. The
	// actual per-request transports are built per backend in proxyFor so pool limits
	// apply per host.
	p.transport = p.buildTransport("https")
	// Honor an explicit RetryOn from config if present (opt-in; empty => default).
	if len(retryCfg.RetryOn) > 0 {
		p.retryOn = toSet(retryCfg.RetryOn)
	}
	return p
}

// firstDuration returns v if it is > 0, else def. Only zero-values are filled with
// the default so an explicit (small) configured value is respected.
func firstDuration(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// firstInt returns v if it is > 0, else def.
func firstInt(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// buildTransport constructs a fresh *http.Transport from the upstream config,
// falling back to the hardcoded §0 constants for any zero-valued field so the
// timeout hardening is never regressed. TLSClientConfig (backendTLS) is preserved on
// every transport. When UpstreamConfig.HTTP2 is set and the backend scheme is https,
// ForceAttemptHTTP2 negotiates h2 via ALPN; the h2c (plaintext HTTP/2) path is handled
// separately in buildH2CTransport.
//
// scheme selects HTTP/2 wiring: "https" => ForceAttemptHTTP2 when HTTP2 is enabled.
func (p *Proxy) buildTransport(scheme string) http.RoundTripper {
	u := p.upstream
	maxConnsPerHost := u.MaxConnsPerHost // 0 = unlimited (valid default), so no fill.
	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   firstDuration(u.DialTimeout, dialTimeout),
			KeepAlive: dialKeepAlive,
		}).DialContext,
		MaxIdleConns:          firstInt(u.MaxIdleConns, maxIdleConns),
		MaxIdleConnsPerHost:   firstInt(u.MaxIdleConnsPerHost, maxIdleConnsPerHost),
		MaxConnsPerHost:       maxConnsPerHost,
		IdleConnTimeout:       firstDuration(u.IdleConnTimeout, idleConnTimeout),
		TLSHandshakeTimeout:   firstDuration(u.TLSHandshakeTimeout, tlsHandshakeTimeout),
		ResponseHeaderTimeout: firstDuration(u.ResponseHeaderTimeout, responseHeaderTimeout),
		ExpectContinueTimeout: firstDuration(u.ExpectContinueTimeout, expectContinueTimeout),
		TLSClientConfig:       p.backendTLS,
	}
	if u.HTTP2 && scheme == "https" {
		// https backends: negotiate HTTP/2 over TLS via ALPN. This keeps HTTP/1.1
		// fallback intact when the backend does not advertise h2.
		tr.ForceAttemptHTTP2 = true
	}
	return tr
}

// buildH2CTransport builds an *http2.Transport for cleartext (h2c) upstreams. It uses
// AllowHTTP with a DialTLSContext that dials a plain TCP connection (no TLS), which is
// how the h2c prior-knowledge upgrade is performed. The dial timeout honors the
// upstream config. Response-header/idle timeouts are not directly expressible on
// http2.Transport; the per-attempt context deadline (RetryConfig.PerTryTimeout) and the
// front server bound overall attempt duration instead.
func (p *Proxy) buildH2CTransport() http.RoundTripper {
	dialTO := firstDuration(p.upstream.DialTimeout, dialTimeout)
	return &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			d := &net.Dialer{Timeout: dialTO, KeepAlive: dialKeepAlive}
			return d.DialContext(ctx, network, addr)
		},
	}
}

// transportFor returns the transport a given backend URL should use. Each call builds
// a fresh transport so every per-backend ReverseProxy owns its own connection pool
// (§5.2): MaxIdleConnsPerHost / MaxConnsPerHost then apply per backend rather than
// across all backends. The transport variant is chosen per scheme so h2c (§5.3) uses an
// http2.Transport while https/http use the standard tuned *http.Transport.
func (p *Proxy) transportFor(target *url.URL) http.RoundTripper {
	if p.upstream.HTTP2 && target.Scheme == "http" {
		return p.buildH2CTransport()
	}
	return p.buildTransport(target.Scheme)
}

// SetWebSocket configures opt-in WebSocket limits (idle timeout / max message bytes)
// enforced on hijacked upgrade connections. It is a setter (not a New parameter) so
// existing callers/tests keep the current unlimited behavior unchanged. The server
// agent wires this from config.ServerConfig.WebSocket.
func (p *Proxy) SetWebSocket(cfg config.WebSocketConfig) {
	p.websocket = cfg
}

// SetRouter installs an optional per-request Router for L7 routing. It is opt-in:
// when never called (router stays nil) the proxy uses p.balancer for every request,
// exactly as before. The server wires this with an adapter around routing.Router.
func (p *Proxy) SetRouter(router Router) {
	p.router = router
}

// SetCanary installs an optional canary balancer that receives weightPercent
// (0..100) of traffic via a weighted-random dice per request (§9.1). It is opt-in:
// when never called (canary stays nil) or weightPercent <= 0, every request flows
// through the normal router/p.balancer path exactly as before, byte-for-byte.
//
// When set and weightPercent > 0, ServeHTTP rolls the proxy's seedable rng once per
// request; if the roll lands in the canary fraction, the canary balancer is pinned on
// the request context so the WHOLE request — selection, in-group failover, observers
// and sticky — is served consistently from the canary group. Otherwise the request
// proceeds through the normally-routed balancer. A weightPercent >= 100 sends all
// eligible traffic to the canary; values are clamped into [0,100].
func (p *Proxy) SetCanary(b balancer.Balancer, weightPercent int) {
	if weightPercent < 0 {
		weightPercent = 0
	}
	if weightPercent > 100 {
		weightPercent = 100
	}
	p.canary = b
	p.canaryWeight = weightPercent
}

// UpdateCanaryWeight atomically updates the weight of the existing canary
// balancer without changing which balancer is installed. This allows a live
// weight-only change without rebuilding the proxy's handler chain. weightPercent
// is clamped to [0,100]. If no canary balancer has been set, the call is a no-op.
func (p *Proxy) UpdateCanaryWeight(weightPercent int) {
	if weightPercent < 0 {
		weightPercent = 0
	}
	if weightPercent > 100 {
		weightPercent = 100
	}
	p.randMu.Lock()
	p.canaryWeight = weightPercent
	p.randMu.Unlock()
}

// rollCanary rolls the proxy's seedable rng and reports whether the request should
// be served from the canary pool. It returns true with probability canaryWeight/100,
// using a uniform integer in [0,100) so weight 100 always routes to canary and weight
// 0 never does (the caller already guards weight <= 0). Access to the rng is serialized
// by randMu, matching calculateBackoff, since the private source is not concurrency-safe.
func (p *Proxy) rollCanary() bool {
	p.randMu.Lock()
	n := p.rng.Intn(100)
	p.randMu.Unlock()
	return n < p.canaryWeight
}

// balancerFor returns the balancer that owns THIS request's backend group. It
// prefers the balancer stashed on the request context (set once per request in
// ServeHTTP from the router), falling back to the router directly, then to
// p.balancer. This guarantees every downstream helper (failover, sticky,
// observe*) operates on the SAME routed group as the selected backend.
func (p *Proxy) balancerFor(r *http.Request) balancer.Balancer {
	if r != nil {
		if b, ok := r.Context().Value(balancerCtxKey).(balancer.Balancer); ok && b != nil {
			return b
		}
		if p.router != nil {
			if b := p.router.Route(r); b != nil {
				return b
			}
		}
	}
	return p.balancer
}

func toSet(items []string) map[string]bool {
	s := make(map[string]bool, len(items))
	for _, it := range items {
		s[it] = true
	}
	return s
}

// SetSticky enables cookie-based session affinity using the supplied config.
// It is an optional setter (rather than a New parameter) so existing callers and
// tests that construct a Proxy without sticky sessions keep working unchanged.
func (p *Proxy) SetSticky(cfg config.StickyConfig) {
	p.sticky = cfg
}

// SetTripOn configures which failure classes ("connect","timeout","5xx") count as
// a circuit-breaker failure. It is opt-in: if never called (or called with an
// empty slice), the default {"connect","timeout"} is preserved so existing
// behavior is unchanged. The server agent wires this from
// config.CircuitBreakerConfig.TripOn.
func (p *Proxy) SetTripOn(classes []string) {
	if len(classes) == 0 {
		return
	}
	p.tripOn = toSet(classes)
}

// SetRetryOn configures which pre-response failure classes ("connect","timeout")
// are eligible for retry/failover. Opt-in; empty preserves the default.
func (p *Proxy) SetRetryOn(classes []string) {
	if len(classes) == 0 {
		return
	}
	p.retryOn = toSet(classes)
}

// SetRand injects a deterministic *rand.Rand for full-jitter backoff (tests). A
// nil argument is ignored.
func (p *Proxy) SetRand(r *rand.Rand) {
	if r == nil {
		return
	}
	p.randMu.Lock()
	p.rng = r
	p.randMu.Unlock()
}

// GetBudgetDenied returns the number of retries suppressed by the retry budget.
func (p *Proxy) GetBudgetDenied() uint64 { return atomic.LoadUint64(&p.budgetDenied) }

// GetRejections returns the number of requests rejected because every candidate
// backend was at its connection capacity (bulkhead).
func (p *Proxy) GetRejections() uint64 { return atomic.LoadUint64(&p.rejections) }

// GetHedgedCount returns the number of speculative hedge attempts launched.
func (p *Proxy) GetHedgedCount() uint64 { return atomic.LoadUint64(&p.hedged) }

// GetHedgeWins returns the number of requests won by a hedge (non-primary) attempt.
func (p *Proxy) GetHedgeWins() uint64 { return atomic.LoadUint64(&p.hedgeWins) }

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Canary (§9.1): when a canary pool is configured with a positive weight, roll the
	// seedable rng ONCE. If the roll lands in the canary fraction, the whole request is
	// served from the canary balancer (pinned below), bypassing the normal routing path.
	// When no canary is set (or the roll misses / weight is 0) this is a no-op and
	// behavior is byte-for-byte unchanged.
	if p.canary != nil && p.canaryWeight > 0 && p.rollCanary() {
		r = r.WithContext(context.WithValue(r.Context(), balancerCtxKey, p.canary))
	} else {
		// Resolve the routed balancer ONCE for this request and pin it to the context so
		// every downstream helper (selection, failover, sticky, observe*) uses the same
		// group. With no router configured this is exactly p.balancer (unchanged).
		b := p.balancer
		if p.router != nil {
			if rb := p.router.Route(r); rb != nil {
				b = rb
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), balancerCtxKey, b))
	}

	backend, err := p.selectBackend(r)
	if err != nil {
		p.metrics.IncrError()
		http.Error(w, "No available backends", http.StatusServiceUnavailable)
		return
	}
	// backend is reserved (IncrConn) by the balancer at selection time.
	if p.sticky.Enabled {
		p.setStickyCookie(w, backend)
	}
	p.proxyRequest(w, r, backend)
}

// selectBackend chooses a backend for the request. Selection is request-aware:
//   - When sticky sessions are enabled and the request carries a valid affinity
//     cookie mapping to a currently-healthy backend, that backend is reused
//     directly (and reserved) so the client stays pinned.
//   - Otherwise, an affinity key is derived (the sticky cookie value if present,
//     else the client IP). If the balancer is a KeyedBalancer (consistent_hash,
//     ip_hash) it is asked to select via NextForKey(key); otherwise Next() is used.
//
// Every path returns a backend already reserved via IncrConn, matching the
// reserve-on-select convention (the proxy releases via DecrConn).
func (p *Proxy) selectBackend(r *http.Request) (*balancer.Backend, error) {
	clientIP := netutil.ClientIP(r, p.trusted)
	// Selection operates on the ROUTED balancer for this request (the routing group),
	// which is p.balancer when no router is configured.
	b := p.balancerFor(r)

	if p.sticky.Enabled {
		if token := p.stickyToken(r); token != "" {
			if be := p.backendForToken(r, token); be != nil {
				be.IncrConn() // reserve, matching the balancer convention
				return be, nil
			}
		}
		// Sticky enabled but no valid pin yet: key the balancer on the cookie
		// value if present, else the client IP.
		key := p.stickyToken(r)
		if key == "" {
			key = clientIP
		}
		if kb, ok := b.(balancer.KeyedBalancer); ok {
			return kb.NextForKey(key)
		}
		return b.Next()
	}

	if kb, ok := b.(balancer.KeyedBalancer); ok {
		return kb.NextForKey(clientIP)
	}
	return b.Next()
}

// backendToken returns a stable, opaque token identifying a backend, derived from
// its URL. It is used as the sticky affinity cookie value so the raw upstream URL
// is never exposed to clients.
func backendToken(b *balancer.Backend) string {
	h := fnv.New64a()
	h.Write([]byte(b.URL))
	return hex.EncodeToString(h.Sum(nil))
}

// stickyToken returns the affinity cookie value from the request, or "" if absent.
func (p *Proxy) stickyToken(r *http.Request) string {
	if p.sticky.Cookie == "" {
		return ""
	}
	c, err := r.Cookie(p.sticky.Cookie)
	if err != nil || c == nil {
		return ""
	}
	return c.Value
}

// backendForToken returns the currently-healthy backend whose token matches, or
// nil if none (unknown token, or the pinned backend is no longer healthy). The
// lookup is scoped to THIS request's routed group so a sticky pin never resolves to
// a backend in a different route's group.
func (p *Proxy) backendForToken(r *http.Request, token string) *balancer.Backend {
	for _, b := range p.balancerFor(r).GetHealthy() {
		if backendToken(b) == token {
			return b
		}
	}
	return nil
}

// setStickyCookie writes the affinity cookie mapping the client to the chosen
// backend so subsequent requests pin to it.
func (p *Proxy) setStickyCookie(w http.ResponseWriter, backend *balancer.Backend) {
	if p.sticky.Cookie == "" {
		return
	}
	// #nosec G124 -- Secure is intentionally omitted: the proxy may serve
	// plain HTTP. HttpOnly and SameSite=Lax are set; Secure must be enforced
	// at the TLS terminator (load balancer / ingress) above the proxy.
	cookie := &http.Cookie{
		Name:     p.sticky.Cookie,
		Value:    backendToken(backend),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if p.sticky.TTL > 0 {
		cookie.MaxAge = int(p.sticky.TTL / time.Second)
	}
	http.SetCookie(w, cookie)
}

// proxyRequest drives a request through the primary backend and, if that fails
// before any bytes are written to the client, iteratively fails over to other
// healthy backends. It replaces the previous recursive tryNextBackend, which could
// recurse until stack overflow and double-write response headers.
func (p *Proxy) proxyRequest(w http.ResponseWriter, r *http.Request, primary *balancer.Backend) {
	// Hedging is an opt-in, idempotent-only fast path. When it cannot apply we
	// fall through to the standard sequential failover path below.
	if p.hedgeEnabled() && isIdempotent(r) {
		if handled := p.proxyRequestHedged(w, r, primary); handled {
			return
		}
	}

	tried := make(map[*balancer.Backend]bool)
	backend := primary
	var lastErr error
	// bulkheadBlocked records whether we skipped a candidate purely because it was
	// at its connection cap (as opposed to a circuit trip). If every candidate is
	// unavailable and at least one was capacity-blocked, we surface 503 (bulkhead
	// rejection) rather than a generic 502.
	bulkheadBlocked := false

	for backend != nil {
		tried[backend] = true

		if !p.available(backend) {
			if p.atCapacity(backend) {
				bulkheadBlocked = true
			}
			// Reserved but not attempted; release the slot.
			backend.DecrConn()
		} else {
			err, class, written := p.attemptBackend(w, r, backend)
			p.observeOutcome(r, backend, err == nil)
			p.recordCircuit(backend, class)
			if err == nil {
				return
			}
			lastErr = err
			if written {
				// The response was already partially sent; we cannot fail over.
				p.metrics.IncrError()
				return
			}
			// Cross-backend failover is only safe when the request can be
			// replayed: either the method is idempotent, or the failure was a
			// pure connection-establishment error (the backend never saw the
			// request). Otherwise, replaying a non-idempotent request risks a
			// double-apply, so we stop and surface a generic 502.
			if !isIdempotent(r) && !isConnectError(err) {
				break
			}
		}

		backend = p.reserveAlternate(r, tried)
	}

	p.metrics.IncrError()
	switch {
	case lastErr != nil:
		// Do not leak internal dial/TLS/backend-URL detail to clients; the
		// detailed error is already logged server-side in attemptBackend.
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
	case bulkheadBlocked:
		// Every candidate was healthy but at its connection cap: this is a
		// bulkhead rejection, not an absence of backends.
		atomic.AddUint64(&p.rejections, 1)
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
	default:
		http.Error(w, "No available backends", http.StatusServiceUnavailable)
	}
}

// recordCircuit feeds the circuit breaker for a completed attempt given its
// classification. A class in the trip set is a failure; anything else (including
// a 5xx when "5xx" is not configured) is a success.
func (p *Proxy) recordCircuit(backend *balancer.Backend, class string) {
	if p.circuitBreaker == nil {
		return
	}
	if class != "" && p.tripOn[class] {
		p.circuitBreaker.RecordFailure(backend)
		return
	}
	p.circuitBreaker.RecordSuccess(backend)
}

// idempotentMethods are the HTTP methods that are safe to retry/replay because,
// by spec, issuing them more than once has the same effect as issuing them once.
var idempotentMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodPut:     true,
	http.MethodDelete:  true,
	http.MethodOptions: true,
	http.MethodTrace:   true,
}

// isIdempotent reports whether a request may be safely replayed against a
// backend. A request qualifies if it uses an idempotent method, or if the client
// has explicitly opted in by supplying a non-empty Idempotency-Key header.
func isIdempotent(r *http.Request) bool {
	if idempotentMethods[r.Method] {
		return true
	}
	return r.Header.Get("Idempotency-Key") != ""
}

// isConnectError reports whether err is a connection-establishment failure,
// meaning the backend definitely did not receive or process the request. Such
// failures are safe to replay even for non-idempotent methods, because nothing
// was applied upstream.
func isConnectError(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Op == "dial" {
			return true
		}
	}
	return errors.Is(err, syscall.ECONNREFUSED)
}

// classifyError maps an upstream transport error to a failure class. It only
// distinguishes pre-response classes ("connect","timeout"); a nil error is the
// caller's responsibility to classify (via status code). An unrecognized error
// is treated as "connect" conservatively only if it is a dial error; otherwise
// it is a generic transport failure classified as "timeout" is wrong, so we fall
// back to "connect" for dial and "timeout" for deadline, else "connect".
func classifyError(ctx context.Context, err error) string {
	if err == nil {
		return ""
	}
	// A deadline/cancellation on the per-try or client context => timeout.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	if ctx != nil && ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "timeout"
		}
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return "timeout"
	}
	if isConnectError(err) {
		return "connect"
	}
	// Any other transport error (reset, EOF mid-flight, etc.) means the attempt
	// did not yield a usable response; classify as "connect" for retry safety on
	// idempotent requests but note it is not a pure dial error, so non-idempotent
	// callers will still refuse to replay (isConnectError stays false above).
	return "connect"
}

// available reports whether a (already-reserved) backend may be attempted. It checks
// the connection cap first (no side effects) and then the circuit breaker.
func (p *Proxy) available(backend *balancer.Backend) bool {
	if p.atCapacity(backend) {
		return false
	}
	if p.circuitBreaker != nil {
		if err := p.circuitBreaker.Allow(backend); err != nil {
			return false
		}
	}
	return true
}

// atCapacity reports whether a reserved backend is over its per-backend connection
// cap (bulkhead). The reservation is already counted, so the check uses ">".
func (p *Proxy) atCapacity(backend *balancer.Backend) bool {
	return backend.MaxConns > 0 && backend.GetActiveConns() > backend.MaxConns
}

// reserveAlternate finds and reserves the next healthy, untried backend with spare
// capacity, or returns nil. Alternates are drawn from THIS request's routed group
// (via balancerFor) so failover never crosses into a different route's backends.
func (p *Proxy) reserveAlternate(r *http.Request, tried map[*balancer.Backend]bool) *balancer.Backend {
	for _, b := range p.balancerFor(r).GetHealthy() {
		if tried[b] {
			continue
		}
		if b.MaxConns > 0 && b.GetActiveConns() >= b.MaxConns {
			continue
		}
		b.IncrConn() // reserve
		return b
	}
	return nil
}

// attemptBackend runs the request against one backend with retries and releases the
// backend's reservation when done. It returns the last error (nil on success), the
// failure class of the final attempt ("" on success / non-5xx), and whether any
// bytes were written to the client (in which case failover is impossible).
//
//lint:ignore ST1008 error-first tuple is established in this package; all callers match
func (p *Proxy) attemptBackend(w http.ResponseWriter, r *http.Request, backend *balancer.Backend) (error, string, bool) {
	defer backend.DecrConn()

	// Count this backend-attempt toward the retry-budget denominator.
	atomic.AddUint64(&p.retryReqs, 1)

	var lastErr error
	var lastClass string
	var lastRetryAfter time.Duration
	maxRetries := p.retryConfig.MaxAttempts
	// Same-backend retries replay the request against the same upstream, so they
	// are only safe for idempotent requests. For non-idempotent requests we make
	// a single attempt and let the caller decide whether cross-backend failover
	// is safe (e.g. on a pure connection error).
	if !isIdempotent(r) {
		maxRetries = 0
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Retry budget: when configured, stop retrying once the running
			// retries/requests ratio would exceed the budget (past a small floor).
			if !p.allowRetry() {
				atomic.AddUint64(&p.budgetDenied, 1)
				break
			}
			atomic.AddUint64(&p.retryCount, 1)
			p.metrics.IncrRetry()
			logging.Info("Retrying request", map[string]interface{}{
				"backend": backend.URL,
				"attempt": attempt,
			})
			p.sleepBackoff(attempt, lastRetryAfter)
		}

		err, class, written, retryAfter := p.doRequest(w, r, backend)
		if err == nil {
			return nil, class, written
		}

		lastErr = err
		lastClass = class
		lastRetryAfter = retryAfter
		logging.Error("Request failed", map[string]interface{}{
			"backend": backend.URL,
			"error":   err.Error(),
		})
		p.metrics.RecordBackendError(backend.URL)

		if written {
			// Cannot retry once the client has started receiving the response.
			return lastErr, class, true
		}
		// Only classes in the retry set are eligible for a same-backend retry.
		if !p.retryOn[class] {
			break
		}
	}

	return lastErr, lastClass, false
}

// allowRetry reports whether another retry is permitted under the configured
// retry budget. Budget<=0 means unlimited (current behavior). A small constant
// floor of retries is always allowed so low-traffic proxies still retry.
func (p *Proxy) allowRetry() bool {
	budget := p.retryConfig.Budget
	if budget <= 0 {
		return true
	}
	retries := atomic.LoadUint64(&p.retryCount)
	if retries < retryBudgetFloor {
		return true
	}
	reqs := atomic.LoadUint64(&p.retryReqs)
	if reqs == 0 {
		return true
	}
	return float64(retries)/float64(reqs) < budget
}

// doRequest performs a single proxied request. It returns the upstream error (nil on
// success), the failure class, whether any bytes were written to the client, and any
// Retry-After delay parsed from the (5xx/429) response. Errors are captured from the
// shared ReverseProxy's ErrorHandler via the request context.
//
//lint:ignore ST1008 error-first tuple is established in this package; all callers match
func (p *Proxy) doRequest(w http.ResponseWriter, r *http.Request, backend *balancer.Backend) (error, string, bool, time.Duration) {
	start := time.Now()
	p.metrics.RecordBackendRequest(backend.URL)

	rp, err := p.proxyFor(backend)
	if err != nil {
		return err, "connect", false, 0
	}

	ec := &errCapture{}
	cw := &captureWriter{ResponseWriter: w, ws: &p.websocket}

	// Per-try timeout (opt-in): bound each attempt with its own deadline derived
	// from the client's context, so a slow backend attempt is abandoned and
	// classified "timeout". Client-side cancellation is still honored because the
	// per-try context is a child of r.Context().
	ctx := context.WithValue(r.Context(), errCtxKey, ec)
	var cancel context.CancelFunc
	if p.retryConfig.PerTryTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, p.retryConfig.PerTryTimeout)
	}
	req := r.WithContext(ctx)

	rp.ServeHTTP(cw, req)
	if cancel != nil {
		cancel()
	}

	elapsed := time.Since(start)
	p.metrics.RecordBackendLatency(backend.URL, elapsed)

	if ec.err != nil {
		return ec.err, classifyError(ctx, ec.err), cw.wrote, 0
	}

	// A response was produced. If it is 5xx, classify as "5xx" (already written to
	// the client, never retried) but surface Retry-After for a subsequent attempt
	// on a DIFFERENT backend where applicable.
	class := "ok"
	var retryAfter time.Duration
	if cw.statusCode >= 500 && cw.statusCode <= 599 {
		class = "5xx"
	}
	if p.retryConfig.HonorRetryAfter {
		retryAfter = parseRetryAfter(cw.retryAfter)
	}

	// Feed latency-aware balancers (EWMA) only on a successful upstream response,
	// so a failed/timed-out attempt does not poison the latency estimate.
	if class == "ok" {
		p.observeLatency(r, backend, elapsed)
	}
	return nil, class, cw.wrote, retryAfter
}

// parseRetryAfter parses a Retry-After header value, supporting the delta-seconds
// form (an HTTP-date form is best-effort ignored). Returns 0 if absent/invalid.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// observeLatency reports request latency to THIS request's routed balancer if it is
// a LatencyObserver, so latency estimates are fed back into the same group the
// backend was selected from.
func (p *Proxy) observeLatency(r *http.Request, backend *balancer.Backend, d time.Duration) {
	if lo, ok := p.balancerFor(r).(balancer.LatencyObserver); ok {
		lo.ObserveLatency(backend, d)
	}
}

// observeOutcome reports request success/failure to THIS request's routed balancer
// if it is an OutcomeObserver (outlier detection), keeping outlier accounting scoped
// to the same group as the selected backend.
func (p *Proxy) observeOutcome(r *http.Request, backend *balancer.Backend, ok bool) {
	if oo, isOO := p.balancerFor(r).(balancer.OutcomeObserver); isOO {
		oo.ObserveOutcome(backend, ok)
	}
}

// proxyFor returns a cached, per-backend ReverseProxy. Each backend's ReverseProxy
// owns its OWN *http.Transport (built from the upstream config), so connection pooling
// happens per backend and MaxIdleConnsPerHost / MaxConnsPerHost apply per backend
// rather than being shared across all backends (§5.2). The transport variant is chosen
// per backend scheme so h2c upstreams use an http2.Transport (§5.3).
func (p *Proxy) proxyFor(backend *balancer.Backend) (*httputil.ReverseProxy, error) {
	p.mu.RLock()
	rp, ok := p.proxies[backend.URL]
	p.mu.RUnlock()
	if ok {
		return rp, nil
	}

	target, err := url.Parse(backend.URL)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if rp, ok = p.proxies[backend.URL]; ok {
		return rp, nil
	}

	rp = httputil.NewSingleHostReverseProxy(target) // #nosec G704 -- SSRF is by design for a reverse proxy; target is validated config
	rp.Transport = p.transportFor(target)
	// Reuse copy buffers across requests to cut per-request allocations on the
	// body-streaming hot path (§10.2).
	rp.BufferPool = bufferPool

	orig := rp.Director
	rp.Director = func(req *http.Request) {
		orig(req) // sets scheme/host/path and (in ServeHTTP) appends X-Forwarded-For
		req.Host = target.Host
		req.Header.Set("X-Real-IP", netutil.ClientIP(req, p.trusted))
	}

	rp.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		if ec, ok := req.Context().Value(errCtxKey).(*errCapture); ok {
			ec.err = err
			return // do not write; let the caller retry or fail over
		}
		w.WriteHeader(http.StatusBadGateway)
	}

	p.proxies[backend.URL] = rp
	return rp, nil
}

// sleepBackoff sleeps before a retry. If a Retry-After delay was signalled by the
// previous response it is honored (bounded by MaxBackoff); otherwise a full-jitter
// exponential backoff is used. Note: transport-error retries carry no Retry-After
// header, so retryAfter is 0 for those and jitter applies.
func (p *Proxy) sleepBackoff(attempt int, retryAfter time.Duration) {
	d := p.calculateBackoff(attempt)
	if retryAfter > 0 {
		d = retryAfter
		if cap := p.retryConfig.MaxBackoff; cap > 0 && d > cap {
			d = cap
		}
	}
	if d > 0 {
		time.Sleep(d)
	}
}

// calculateBackoff returns a full-jitter exponential backoff for the given attempt
// (attempt >= 1). The sleep is a uniform random value in [0, min(cap, base*2^n)],
// where base is 1s and cap is MaxBackoff (0 => uncapped exponential ceiling). Using
// full jitter (rather than a fixed attempt^2) avoids retry storms / thundering herd.
func (p *Proxy) calculateBackoff(attempt int) time.Duration {
	if attempt < 1 {
		return 0
	}
	const base = time.Second
	// Compute base * 2^(attempt-1) with overflow guarding.
	ceiling := base
	for i := 1; i < attempt; i++ {
		ceiling <<= 1
		if ceiling <= 0 { // overflow
			ceiling = time.Duration(1) << 62
			break
		}
	}
	if maxBackoff := p.retryConfig.MaxBackoff; maxBackoff > 0 && ceiling > maxBackoff {
		ceiling = maxBackoff
	}
	if ceiling <= 0 {
		return 0
	}
	p.randMu.Lock()
	n := p.rng.Int63n(int64(ceiling) + 1)
	p.randMu.Unlock()
	return time.Duration(n)
}

func (p *Proxy) GetMetrics() *metrics.Metrics {
	return p.metrics
}

// hedgeEnabled reports whether hedged requests are configured on.
func (p *Proxy) hedgeEnabled() bool {
	return p.retryConfig.Hedge.Enabled && p.retryConfig.Hedge.MaxExtra > 0
}

// hedgeResult carries the buffered outcome of one hedge attempt.
type hedgeResult struct {
	backend *balancer.Backend
	rec     *httptest.ResponseRecorder
	err     error
	class   string
}

// proxyRequestHedged races the primary attempt against up to Hedge.MaxExtra extra
// attempts on OTHER healthy backends. The FIRST attempt to produce a usable
// response (any completed response, including 5xx) wins and is the only one copied
// to the client; losers are cancelled via context. Every reservation is released
// exactly once (each attempt DecrConn's its own backend). Exactly one writer
// touches the real ResponseWriter because all attempts write into private
// httptest recorders and only the winner is copied out here.
//
// It returns true if it fully handled the request (wrote a response, or wrote a 502
// when no attempt produced one). It returns false ONLY before launching any attempt
// (primary unavailable, or no other backend to hedge against); in that case primary
// is left RESERVED so the caller's standard sequential path — which assumes primary
// is reserved — uses it without a double-release. Once any attempt is launched this
// function fully owns all reservations (each goroutine DecrConn's its own) and
// always returns true.
func (p *Proxy) proxyRequestHedged(w http.ResponseWriter, r *http.Request, primary *balancer.Backend) bool {
	// Gather candidate backends: primary first, then up to MaxExtra other healthy,
	// available backends. On any false return we leave primary RESERVED so the
	// caller's standard sequential path (which assumes primary is reserved) can use
	// it without a double-release.
	if !p.available(primary) {
		return false
	}

	maxExtra := p.retryConfig.Hedge.MaxExtra
	extras := p.reserveHedgeBackends(r, primary, maxExtra)
	if len(extras) == 0 {
		// No other backend to hedge against: fall back to the standard single-path
		// so behavior matches the non-hedged case. primary stays reserved.
		return false
	}

	backends := append([]*balancer.Backend{primary}, extras...)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	results := make(chan hedgeResult, len(backends))
	var launched int

	launch := func(b *balancer.Backend, isPrimary bool) {
		launched++
		if !isPrimary {
			atomic.AddUint64(&p.hedged, 1)
		}
		go func() {
			rec := httptest.NewRecorder()
			hr := hedgeResult{backend: b, rec: rec}
			err, class, _, _ := p.doHedgeAttempt(ctx, r, b, rec)
			hr.err = err
			hr.class = class
			// Record circuit + outcome per attempt (each attempt is a real request).
			p.observeOutcome(r, b, err == nil)
			p.recordCircuit(b, class)
			b.DecrConn() // release this attempt's reservation exactly once
			results <- hr
		}()
	}

	// Launch the primary immediately, then the extras after Hedge.Delay unless the
	// primary has already produced a response.
	launch(primary, true)

	delay := p.retryConfig.Hedge.Delay
	timer := time.NewTimer(delay)
	defer timer.Stop()

	var winner *hedgeResult
	pending := 1 // primary in flight
	extrasLaunched := false

	launchExtras := func() {
		if extrasLaunched {
			return
		}
		extrasLaunched = true
		for _, b := range extras {
			launch(b, false)
			pending++
		}
	}

collect:
	for pending > 0 {
		if extrasLaunched {
			// All in flight; just wait for results.
			hr := <-results
			pending--
			if winner == nil && isHedgeWin(hr) {
				w2 := hr
				winner = &w2
				cancel() // stop the losers
			}
			continue
		}
		select {
		case <-timer.C:
			launchExtras()
		case hr := <-results:
			pending--
			if winner == nil && isHedgeWin(hr) {
				w2 := hr
				winner = &w2
				cancel()
				break collect
			}
			// primary failed before delay elapsed: launch extras now.
			launchExtras()
		}
	}

	// Drain any remaining in-flight attempts so their goroutines finish and their
	// reservations are already released inside the goroutine. We keep the last
	// non-winning result as a fallback error source.
	var fallback *hedgeResult
	for pending > 0 {
		hr := <-results
		pending--
		if winner == nil && isHedgeWin(hr) {
			w2 := hr
			winner = &w2
			continue
		}
		if fallback == nil {
			f := hr
			fallback = &f
		}
	}

	// Release reservations for any extras we reserved but never launched (e.g. the
	// primary produced a response before the hedge delay elapsed, so we broke out
	// before launchExtras()). Launched extras release inside their own goroutine;
	// this covers the never-launched, all-or-nothing case to avoid a slot leak.
	if !extrasLaunched {
		for _, b := range extras {
			b.DecrConn()
		}
	}

	if winner != nil {
		if winner.backend != primary {
			atomic.AddUint64(&p.hedgeWins, 1)
		}
		copyRecorded(w, winner.rec)
		return true
	}

	// No attempt produced a response. Surface a generic 502; this fully handles
	// the request (all reservations already released in the goroutines).
	_ = fallback
	p.metrics.IncrError()
	http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
	return true
}

// isHedgeWin reports whether a hedge result counts as a completed response that can
// be sent to the client. Any produced response (including 5xx) wins; only a
// transport error (err != nil) loses.
func isHedgeWin(hr hedgeResult) bool {
	return hr.err == nil
}

// reserveHedgeBackends reserves up to n additional healthy, available backends
// distinct from primary, drawn from THIS request's routed group so hedges never
// cross into a different route's backends. Each returned backend is reserved via
// IncrConn.
func (p *Proxy) reserveHedgeBackends(r *http.Request, primary *balancer.Backend, n int) []*balancer.Backend {
	var out []*balancer.Backend
	for _, b := range p.balancerFor(r).GetHealthy() {
		if len(out) >= n {
			break
		}
		if b == primary {
			continue
		}
		if b.MaxConns > 0 && b.GetActiveConns() >= b.MaxConns {
			continue
		}
		if p.circuitBreaker != nil {
			if err := p.circuitBreaker.Allow(b); err != nil {
				continue
			}
		}
		b.IncrConn()
		out = append(out, b)
	}
	return out
}

// doHedgeAttempt runs a single hedge attempt into a private recorder, using the
// shared cancellable ctx (so losers abort) and the per-try timeout if configured.
//
//lint:ignore ST1008 error-first tuple is established in this package; all callers match
func (p *Proxy) doHedgeAttempt(ctx context.Context, r *http.Request, backend *balancer.Backend, rec *httptest.ResponseRecorder) (error, string, bool, time.Duration) {
	start := time.Now()
	p.metrics.RecordBackendRequest(backend.URL)

	rp, err := p.proxyFor(backend)
	if err != nil {
		return err, "connect", false, 0
	}

	ec := &errCapture{}
	cw := &captureWriter{ResponseWriter: rec}

	attemptCtx := context.WithValue(ctx, errCtxKey, ec)
	var cancel context.CancelFunc
	if p.retryConfig.PerTryTimeout > 0 {
		attemptCtx, cancel = context.WithTimeout(attemptCtx, p.retryConfig.PerTryTimeout)
	}
	req := r.Clone(attemptCtx)

	rp.ServeHTTP(cw, req)
	if cancel != nil {
		cancel()
	}

	elapsed := time.Since(start)
	p.metrics.RecordBackendLatency(backend.URL, elapsed)

	if ec.err != nil {
		return ec.err, classifyError(attemptCtx, ec.err), cw.wrote, 0
	}
	class := "ok"
	if cw.statusCode >= 500 && cw.statusCode <= 599 {
		class = "5xx"
	}
	if class == "ok" {
		// r carries the routed-balancer context (doHedgeAttempt is passed the
		// original request), so latency is fed to the correct group.
		p.observeLatency(r, backend, elapsed)
	}
	return nil, class, cw.wrote, 0
}

// copyRecorded copies a buffered recorder's headers, status and body to the real
// ResponseWriter. This is the single, exclusive write to the client for a hedged
// request.
func copyRecorded(w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	dst := w.Header()
	for k, vv := range rec.Header() {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	code := rec.Code
	if code == 0 {
		code = http.StatusOK
	}
	w.WriteHeader(code)
	if rec.Body != nil {
		w.Write(rec.Body.Bytes())
	}
}

// captureWriter records whether any bytes/headers have been written to the client,
// which determines whether a failed request can still be retried or failed over. It
// also records the status code and a Retry-After header for failure classification.
//
// It also forwards Flush and Hijack to the underlying ResponseWriter so that
// streaming/SSE responses and WebSocket upgrades continue to work through the proxy.
type captureWriter struct {
	http.ResponseWriter
	wrote      bool
	statusCode int
	retryAfter string
	// ws, when non-nil and enabling at least one limit, wraps the hijacked
	// connection to enforce a WebSocket idle timeout and/or a max-message byte cap.
	ws *config.WebSocketConfig
}

func (c *captureWriter) WriteHeader(code int) {
	if !c.wrote {
		c.statusCode = code
		c.retryAfter = c.Header().Get("Retry-After")
	}
	c.wrote = true
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if !c.wrote {
		// An implicit 200 (Write without WriteHeader).
		c.statusCode = http.StatusOK
		c.retryAfter = c.Header().Get("Retry-After")
	}
	c.wrote = true
	return c.ResponseWriter.Write(b)
}

// Flush forwards to the underlying ResponseWriter, enabling streaming/SSE. Once data
// is flushed the response has begun, so failover is no longer possible.
func (c *captureWriter) Flush() {
	if !c.wrote {
		c.statusCode = http.StatusOK
	}
	c.wrote = true
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying ResponseWriter so WebSocket/upgrade requests work.
// When a WebSocket limit is configured (idle timeout or max message bytes), the raw
// connection is wrapped so those limits are enforced for the lifetime of the upgraded
// connection (§5.7).
func (c *captureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c.wrote = true
	h, ok := c.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
	}
	conn, rw, err := h.Hijack()
	if err != nil || conn == nil {
		return conn, rw, err
	}
	if c.ws != nil && (c.ws.IdleTimeout > 0 || c.ws.MaxMessageBytes > 0) {
		conn = newWSConn(conn, c.ws.IdleTimeout, c.ws.MaxMessageBytes)
	}
	return conn, rw, err
}

// wsConn wraps a hijacked (WebSocket/Upgrade) net.Conn to enforce, best-effort, an
// idle read timeout and a total-bytes cap (§5.7). Enforcement is precisely:
//
//   - Idle timeout: before each Read, a read deadline of idleTimeout is armed. If no
//     bytes arrive within that window the Read returns a timeout error and the caller
//     (httputil.ReverseProxy's bidirectional copy) tears the connection down, so idle
//     connections are closed. The deadline is refreshed on every Read call (i.e. it is
//     an inactivity timeout on the read side of THIS half of the proxied pair).
//   - Max message bytes: this caps the CUMULATIVE number of bytes read from this side
//     of the connection (not per-frame; the proxy operates below the WebSocket framing
//     layer, so a true per-message cap is not available here). Once the running total
//     would exceed maxBytes, Read returns an error and closes the connection. maxBytes
//     <= 0 disables the cap.
//
// Both halves of a proxied WebSocket are wrapped independently (client->backend and
// backend->client) because ReverseProxy hijacks and splices both directions.
type wsConn struct {
	net.Conn
	idleTimeout time.Duration
	maxBytes    int64
	read        int64
}

func newWSConn(c net.Conn, idleTimeout time.Duration, maxBytes int64) *wsConn {
	return &wsConn{Conn: c, idleTimeout: idleTimeout, maxBytes: maxBytes}
}

func (w *wsConn) Read(b []byte) (int, error) {
	if w.idleTimeout > 0 {
		// Arm an inactivity deadline for this read; a successful read refreshes it on
		// the next call. An idle (silent) peer trips the deadline and the connection is
		// closed by the copy loop.
		_ = w.Conn.SetReadDeadline(time.Now().Add(w.idleTimeout))
	}
	n, err := w.Conn.Read(b)
	if n > 0 && w.maxBytes > 0 {
		w.read += int64(n)
		if w.read > w.maxBytes {
			w.Conn.Close()
			return n, errors.New("websocket: max message bytes exceeded")
		}
	}
	return n, err
}
