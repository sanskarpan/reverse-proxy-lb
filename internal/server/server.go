package server

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/canary"
	"reverse-proxy-lb/internal/circuit"
	"reverse-proxy-lb/internal/config"
	"reverse-proxy-lb/internal/discovery"
	"reverse-proxy-lb/internal/health"
	"reverse-proxy-lb/internal/limiter"
	"reverse-proxy-lb/internal/logging"
	"reverse-proxy-lb/internal/metrics"
	"reverse-proxy-lb/internal/middleware"
	"reverse-proxy-lb/internal/netutil"
	"reverse-proxy-lb/internal/proxy"
	"reverse-proxy-lb/internal/routing"
	"reverse-proxy-lb/internal/tcpproxy"
	"reverse-proxy-lb/internal/tlsutil"
	"reverse-proxy-lb/internal/tracing"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"
)

const (
	// maxRequestBodyBytes caps client request bodies (10 MiB) to blunt memory-
	// exhaustion attacks; oversized bodies fail with http.MaxBytesError.
	maxRequestBodyBytes = 10 << 20
)

// Server is the central coordinator: it wires together the proxy, balancer, middleware chain, health checkers, and optional TLS, metrics, and gRPC health servers.
type Server struct {
	cfg           *config.Config
	configPath    string
	trusted       []*net.IPNet
	backendTLS    *tls.Config
	httpServer    *http.Server
	metricsServer *http.Server
	// acmeChallengeHandler serves the ACME HTTP-01 challenge; non-nil only when
	// cfg.TLS.ACME.Enabled. It shares the autocert.Manager backing httpServer's
	// TLSConfig, so challenge and account state are shared between the TLS
	// listener and the challenge listener. acmeServer is the tiny HTTP server
	// (started in Start, stopped in Stop) that serves it on the challenge port.
	acmeChallengeHandler http.Handler
	acmeServer           *http.Server
	// stapler refreshes and installs OCSP staples into the served certificates;
	// non-nil only when cfg.TLS.OCSPStapling and static certs are configured. It
	// is Start()ed with the server and Stop()ed during shutdown.
	stapler  *tlsutil.Stapler
	balancer balancer.Balancer
	proxy    *proxy.Proxy
	l4Proxy  *tcpproxy.Proxy
	router   *routing.Router
	// canary is the optional canary balancer group (§9.1). When canary is
	// enabled it holds a separate backend pool that the proxy splits a
	// WeightPercent share of traffic to; nil when canary is disabled.
	canary balancer.Balancer
	// healthChks holds one HealthChecker per balancer group. With no routes
	// configured this is a single checker over the default balancer; with L7
	// routing enabled it is one checker for the default group plus one for each
	// per-route group. All are Start()ed and Stop()ed together.
	healthChks []*health.HealthChecker
	limiter    *limiter.RateLimiter
	metrics    *metrics.Metrics
	metricsMux *http.ServeMux
	// discoverer, when DNS discovery targets are configured, periodically resolves
	// them and syncs the resulting backends into the DEFAULT balancer group so they
	// flow through the same live balancer (and health checkers) as static backends.
	// nil when no discovery targets are configured.
	discoverer *discovery.Discoverer
	// k8sDiscoverer, when Kubernetes Endpoints discovery is enabled, watches a k8s
	// Endpoints object via the REST API and syncs backends into the DEFAULT balancer
	// group.  nil when Kubernetes discovery is disabled.
	k8sDiscoverer k8sStarter
	// circuitBreaker is retained (when circuit breaking is enabled) so the
	// metrics snapshot callback can report per-backend circuit state.
	circuitBreaker *circuit.CircuitBreaker
	// redisSyncer, when non-nil, synchronises circuit-breaker state across
	// replica instances via Redis. Started in Start() once all backends are
	// registered, stopped in Stop() before other subsystems are torn down.
	redisSyncer *circuit.RedisSyncer

	// grpcHealthSrv is the optional native gRPC Health Checking Protocol server.
	// Non-nil only when cfg.LoadBalancer.GRPCHealth.Enabled; stopped in Stop().
	grpcHealthSrv    *grpc.Server
	grpcHealthServer *health.GRPCHealthServer

	// h3Server is the optional HTTP/3 (QUIC) server started when
	// cfg.Server.HTTP3.Enabled. Non-nil only after Start() has been called and
	// TLS is configured; closed in Stop().
	h3Server *http3.Server

	// tracingShutdown is the cleanup func returned by tracing.Setup; non-nil
	// after Start() when tracing is configured. It is called in Stop() to flush
	// and shut down the TracerProvider.
	tracingShutdown func(context.Context) error

	// reloadMu serializes reloadConfig so a SIGHUP and a POST /reload (or two
	// concurrent /reload requests) cannot race on s.cfg's fields.
	reloadMu sync.Mutex

	// watchStop signals the config file-watch goroutine (started in Start when
	// cfg.Server.WatchConfig) to exit; watchWG waits for it during Stop. Both are
	// nil/zero when watching is disabled.
	watchStop chan struct{}
	watchWG   sync.WaitGroup

	// autoPromoter, when non-nil, steps the canary weight up (or rolls it back)
	// on each StepInterval. Started in Start() and stopped in Stop().
	autoPromoter *canary.AutoPromoter
}

// New builds a Server from cfg. configPath is retained so that SIGHUP reload reads
// the same file the process was started with, not a hard-coded path.
func New(cfg *config.Config, configPath string) *Server {
	logging.Configure(cfg.Logging.Level, cfg.Logging.Format)

	backendTLS, err := cfg.BackendTLS.ClientTLSConfig()
	if err != nil {
		// Fail loud rather than silently connecting without the intended trust config.
		logging.Error("Invalid backend_tls configuration", map[string]interface{}{
			"error": err.Error(),
		})
	}

	s := &Server{
		cfg:        cfg,
		configPath: configPath,
		trusted:    netutil.ParseCIDRs(cfg.Server.TrustedProxies),
		backendTLS: backendTLS,
	}

	s.setupBalancer()
	s.setupProxy()
	s.setupLimiter()
	s.setupMetrics()

	// Build the optional DNS service-discovery controller over the DEFAULT balancer
	// group. It is only constructed (not started) here; Start launches it and Stop
	// tears it down first. Discovered backends are added to s.balancer, so they are
	// health-checked by the default group's checker set up below.
	s.setupDiscovery()

	if cfg.LoadBalancer.HealthCheck.Enabled {
		s.setupHealthCheck()
	}

	if cfg.Server.L4.Enabled {
		s.setupL4Proxy()
	}

	s.setupHTTPServer()

	return s
}

// setupL4Proxy builds the optional raw TCP (layer-4) reverse proxy. It reuses the
// same balancer as the HTTP proxy so L4 traffic honors the configured algorithm,
// health state, and per-backend connection accounting.
func (s *Server) setupL4Proxy() {
	s.l4Proxy = tcpproxy.NewProxy(s.balancer, s.cfg.Server.L4.DialTimeout)
}

// k8sDiscoverer is the optional Kubernetes Endpoints service-discovery
// controller.  It is started and stopped alongside the DNS discoverer.
// Stored as an interface so the server package does not import the discovery
// implementation type directly.
type k8sStarter interface {
	Start()
	Stop()
}

// setupDiscovery constructs the DNS service-discovery controller when discovery
// targets are configured. It syncs into the DEFAULT balancer group (s.balancer)
// using the stdlib resolver. When no targets are configured, s.discoverer stays
// nil and discovery is a no-op. Must run after setupBalancer.
// It also wires up Kubernetes Endpoints discovery when that block is enabled.
func (s *Server) setupDiscovery() {
	if len(s.cfg.Discovery.DNS) > 0 {
		s.discoverer = discovery.NewDiscoverer(s.balancer, s.cfg.Discovery.DNS, discovery.NewResolver())
	}
	if s.cfg.Discovery.Kubernetes.Enabled {
		kd, err := discovery.NewKubernetesDiscovery(s.cfg.Discovery.Kubernetes, s.balancer)
		if err != nil {
			logging.Error("Failed to initialize Kubernetes service discovery; skipping", map[string]interface{}{
				"error": err.Error(),
			})
		} else {
			s.k8sDiscoverer = kd
		}
	}
}

func (s *Server) setupBalancer() {
	lb := s.cfg.LoadBalancer

	// Construct the base algorithm. NewByAlgorithm recognizes every configured
	// algorithm name; on an unknown name (which validation should already reject)
	// fall back to round robin so the server still comes up.
	base, err := balancer.NewByAlgorithm(lb.Algorithm, balancer.Options{
		ConsistentHashReplicas:   lb.ConsistentHash.Replicas,
		ConsistentHashLoadFactor: lb.ConsistentHash.LoadFactor,
	})
	if err != nil {
		logging.Error("Unknown load-balancing algorithm, falling back to round_robin", map[string]interface{}{
			"algorithm": lb.Algorithm,
			"error":     err.Error(),
		})
		base = balancer.NewRoundRobin()
	}

	// Compose optional wrappers around the base algorithm. Order matters: the
	// innermost wrapper's restriction is applied first, so we build from the
	// inside out as PriorityTiers -> ZoneAware -> SlowStart -> OutlierDetection.
	// PriorityTiers narrows to the lowest healthy tier, ZoneAware then prefers the
	// local zone within that tier, SlowStart ramps freshly-healthy members, and
	// OutlierDetection sits outermost so its passive ejections and reinstatements
	// gate every selection.
	var b balancer.Balancer = base

	// PriorityTiers is only meaningful when backends actually declare tiers; apply
	// it whenever any backend uses a non-zero tier so failover groups take effect.
	if backendsUseTiers(s.cfg.Backends) {
		b = balancer.NewPriorityTiers(b)
	}

	if lb.PreferSameZone {
		b = balancer.NewZoneAware(b, s.cfg.Server.Zone, lb.PreferSameZone)
	}

	if lb.SlowStart > 0 {
		b = balancer.NewSlowStart(b, lb.SlowStart)
	}

	if lb.OutlierDetection.Enabled {
		od := lb.OutlierDetection
		b = balancer.NewOutlierDetection(
			b,
			od.ErrorRateThreshold,
			od.MinRequests,
			od.BaseEjection,
			od.MaxEjectionPercent,
		)
	}

	s.balancer = b

	for _, bc := range s.cfg.Backends {
		backend := balancer.NewBackend(bc)
		s.balancer.Add(backend)
	}
}

// backendsUseTiers reports whether any backend declares a non-zero priority tier,
// in which case the PriorityTiers wrapper should be enabled.
func backendsUseTiers(backends []config.BackendConfig) bool {
	for _, bc := range backends {
		if bc.Tier != 0 {
			return true
		}
	}
	return false
}

func (s *Server) setupProxy() {
	var cb *circuit.CircuitBreaker
	if s.cfg.CircuitBreaker.Enabled {
		cbc := s.cfg.CircuitBreaker
		// Map the config mode string onto the circuit package's Mode enum. Any
		// value other than "rolling" (including the "consecutive" default) keeps
		// the original consecutive-failure behavior.
		mode := circuit.ModeConsecutive
		if cbc.Mode == "rolling" {
			mode = circuit.ModeRolling
		}
		cb = circuit.NewCircuitBreakerWithConfig(circuit.Config{
			Mode:               mode,
			FailureThreshold:   cbc.FailureThreshold,
			SuccessThreshold:   cbc.SuccessThreshold,
			Timeout:            cbc.Timeout,
			RollingWindow:      cbc.RollingWindow,
			ErrorRateThreshold: cbc.ErrorRateThreshold,
			MinRequests:        cbc.MinRequests,
		})

		// Log every circuit state transition (ENHANCEMENTS 3.7). The hook runs
		// outside the breaker's lock, so logging here is safe.
		cb.SetOnStateChange(func(backend *balancer.Backend, from, to circuit.State) {
			logging.Info("Circuit breaker state change", map[string]interface{}{
				"backend": backend.URL,
				"from":    from.String(),
				"to":      to.String(),
			})
		})
	}
	s.circuitBreaker = cb

	// Wire the optional distributed circuit-breaker state syncer (Bug 2).
	// When SharedState.Enabled and a circuit breaker is active, construct a
	// RedisSyncer so state changes propagate across replicas. The syncer's
	// background goroutine is started in Start() after all backends are
	// registered; it is stopped in Stop() during shutdown.
	if cb != nil && s.cfg.CircuitBreaker.SharedState.Enabled {
		ss := s.cfg.CircuitBreaker.SharedState
		rc := redis.NewClient(&redis.Options{Addr: redisAddrFromURL(ss.RedisURL)})
		adapter := circuit.NewGoRedisAdapter(rc)
		syncer := circuit.NewRedisSyncer(cb, adapter, ss.KeyPrefix, ss.KeyTTL, ss.SyncInterval, "")
		// Register every static backend so the syncer tracks it from the start.
		for _, b := range s.balancer.All() {
			if b != nil {
				syncer.Track(b)
			}
		}
		s.redisSyncer = syncer
		logging.Info("Distributed circuit-breaker state sync enabled", map[string]interface{}{
			"redis_url":     ss.RedisURL,
			"sync_interval": ss.SyncInterval.String(),
			"key_prefix":    ss.KeyPrefix,
		})
	}

	s.proxy = proxy.New(s.balancer, cb, s.cfg.Retry, s.cfg.LoadBalancer.Algorithm, s.trusted, s.backendTLS, s.cfg.Server.Upstream)

	// Apply WebSocket idle/message limits (§5.7). Opt-in: a zero-value
	// WebSocketConfig leaves the proxy's default (unlimited) behavior unchanged.
	s.proxy.SetWebSocket(s.cfg.Server.WebSocket)

	// Wire the failure classes that count toward circuit tripping. Empty preserves
	// the proxy's default {connect,timeout}.
	if s.cfg.CircuitBreaker.Enabled {
		s.proxy.SetTripOn(s.cfg.CircuitBreaker.TripOn)
	}
	// RetryOn is auto-applied inside proxy.New from retryCfg when non-empty; set it
	// explicitly here as well so the wiring is unambiguous.
	s.proxy.SetRetryOn(s.cfg.Retry.RetryOn)

	// Enable cookie-based session affinity when configured. The proxy's New
	// signature is unchanged; sticky is opt-in via this setter.
	if s.cfg.LoadBalancer.Sticky.Enabled {
		s.proxy.SetSticky(s.cfg.LoadBalancer.Sticky)
	}

	// Install the optional L7 router (§L7). When routes are configured, build a
	// Router over per-route balancer groups with the existing (wrapped) balancer
	// as the default/fallback group, and hand it to the proxy. With no routes
	// configured the proxy keeps using s.balancer for every request, unchanged.
	s.setupRouter()

	// Install the optional canary weighted split (§9.1). When canary is enabled,
	// build a separate canary balancer group (algorithm + backends, same builder
	// the L7 router uses) and hand it to the proxy with the configured weight.
	// Disabled (the default) leaves the proxy without a canary, unchanged.
	s.setupCanary()
}

// setupCanary builds the canary balancer group and installs it on the proxy when
// cfg.Canary.Enabled. The group is a base balancer (algorithm + backends) built
// via routing.BuildGroup, matching the per-route groups; no advanced wrappers are
// applied. When canary is disabled or has no backends, nothing is installed and
// the proxy has no canary, so behavior is unchanged.
func (s *Server) setupCanary() {
	if !s.cfg.Canary.Enabled || len(s.cfg.Canary.Backends) == 0 {
		return
	}
	// Reuse the route group builder by mapping the canary block onto a RouteConfig
	// (algorithm + consistent-hash + backends). This keeps canary group semantics
	// identical to per-route groups.
	group, err := routing.BuildGroup(config.RouteConfig{
		Algorithm:      s.cfg.Canary.Algorithm,
		ConsistentHash: s.cfg.Canary.ConsistentHash,
		Backends:       s.cfg.Canary.Backends,
	})
	if err != nil {
		logging.Error("Failed to build canary group; canary disabled", map[string]interface{}{
			"error": err.Error(),
		})
		return
	}
	s.canary = group
	s.proxy.SetCanary(group, s.cfg.Canary.WeightPercent)
}

// routerAdapter bridges *routing.Router to the proxy.Router interface. The proxy
// defines its own minimal Router interface (to avoid an import cycle), so the
// server adapts the concrete routing.Router to it.
type routerAdapter struct {
	r *routing.Router
}

func (a routerAdapter) Route(req *http.Request) balancer.Balancer {
	return a.r.Route(req)
}

// setupRouter builds the L7 router (if routes are configured) and installs it on
// the proxy. The default/fallback group is the existing s.balancer, so unmatched
// requests keep the full wrapper stack and current behavior. When no routes are
// configured, no router is installed and the proxy uses s.balancer for every
// request exactly as before.
func (s *Server) setupRouter() {
	if len(s.cfg.Routes) == 0 {
		return
	}
	router, err := routing.NewRouter(s.cfg.Routes, s.balancer)
	if err != nil {
		// A route group failed to build (e.g. an unknown algorithm that validation
		// somehow let through). Fail loud but keep serving via the default balancer
		// rather than installing a partial router.
		logging.Error("Failed to build L7 router; falling back to default balancer", map[string]interface{}{
			"error": err.Error(),
		})
		return
	}
	s.router = router
	s.proxy.SetRouter(routerAdapter{r: router})
}

func (s *Server) setupLimiter() {
	if !s.cfg.RateLimiter.Enabled {
		return
	}

	rl := s.cfg.RateLimiter
	s.limiter = limiter.NewRateLimiterWithOptions(limiter.Options{
		Algorithm:   rl.Algorithm,
		PerKeyRPS:   float64(rl.RequestsPerSecond),
		PerKeyBurst: rl.Burst,
		GlobalRPS:   float64(rl.GlobalRPS),
		GlobalBurst: rl.GlobalBurst,
	})
	// Register per-route rules under the canonical names the middleware looks up.
	middleware.RegisterRules(s.limiter, rl)

	// Wire optional distributed rate-limit Store (ENHANCEMENTS 4.4).
	ss := rl.SharedStore
	if ss.Enabled {
		switch ss.Backend {
		case "redis":
			rs, err := limiter.NewRedisStore(
				ss.Redis.Addr,
				ss.Redis.Password,
				ss.Redis.DB,
				ss.Redis.Prefix,
			)
			if err != nil {
				logging.Error("Failed to connect to Redis rate-limit store; falling back to memory store", map[string]interface{}{
					"addr":  ss.Redis.Addr,
					"error": err.Error(),
				})
				ms := limiter.NewMemStore()
				s.limiter.SetStore(ms, float64(rl.RequestsPerSecond), rl.Burst, ss.Key)
			} else {
				s.limiter.SetStore(rs, float64(rl.RequestsPerSecond), rl.Burst, ss.Key)
			}
		default:
			// "memory" or any unrecognised backend: use the in-process MemStore.
			ms := limiter.NewMemStore()
			s.limiter.SetStore(ms, float64(rl.RequestsPerSecond), rl.Burst, ss.Key)
		}
	}

	s.limiter.Start()
}

func (s *Server) setupMetrics() {
	s.metrics = s.proxy.GetMetrics()
	s.metricsMux = http.NewServeMux()

	// Register a scrape-time snapshot so the Prometheus handler can emit
	// per-backend up/circuit-state gauges. The closure walks every balancer group
	// (the default group plus any L7 routing groups) and reads live health +
	// circuit state at exposition time.
	s.metrics.SetSnapshotFunc(s.backendGauges)

	auth := s.adminAuth
	// /metrics serves the Prometheus text-exposition format; /metrics.json keeps
	// the legacy JSON handler available for existing consumers.
	s.metricsMux.HandleFunc("/metrics", auth(s.metrics.PrometheusHandler))
	s.metricsMux.HandleFunc("/metrics.json", auth(s.metrics.Handler))
	s.metricsMux.HandleFunc("/reload", auth(s.handleReload))

	// Admin API (§ops): inspect and mutate live backend state across every balancer
	// group. All endpoints are behind adminAuth like the rest of the admin mux.
	//   GET  /admin/backends       -> JSON list of every backend across all groups
	//   POST /admin/drain?url=      -> SetHealthy(false) (stop sending traffic)
	//   POST /admin/undrain?url=    -> SetHealthy(true)
	//   POST /admin/weight?url=&weight=N -> UpdateWeight on the owning group
	//   POST /admin/circuit/reset?url= -> circuit breaker Reset for the backend
	s.metricsMux.HandleFunc("/admin/backends", auth(s.handleAdminBackends))
	s.metricsMux.HandleFunc("/admin/drain", auth(s.handleAdminDrain))
	s.metricsMux.HandleFunc("/admin/undrain", auth(s.handleAdminUndrain))
	s.metricsMux.HandleFunc("/admin/weight", auth(s.handleAdminWeight))
	s.metricsMux.HandleFunc("/admin/circuit/reset", auth(s.handleAdminCircuitReset))
	//   GET  /admin/canary/status -> JSON snapshot of the auto-promoter state
	s.metricsMux.HandleFunc("/admin/canary/status", auth(s.handleAdminCanaryStatus))

	// Liveness/readiness probes for orchestrators (k8s, load balancers). These are
	// intentionally UNauthenticated and served on the admin listener, separate from
	// the proxied data plane (where "/health" would be forwarded to a backend).
	// /healthz: the process is up. /readyz: at least one backend is healthy.
	s.metricsMux.HandleFunc("/healthz", s.handleHealthz)
	s.metricsMux.HandleFunc("/readyz", s.handleReadyz)

	// net/http/pprof profiling endpoints, guarded by the same admin auth as the
	// rest of the metrics mux. The admin/metrics server binds to cfg.Metrics.Host
	// (loopback by default), so these are not exposed to the data-plane listener.
	s.metricsMux.HandleFunc("/debug/pprof/", auth(pprof.Index))
	s.metricsMux.HandleFunc("/debug/pprof/cmdline", auth(pprof.Cmdline))
	s.metricsMux.HandleFunc("/debug/pprof/profile", auth(pprof.Profile))
	s.metricsMux.HandleFunc("/debug/pprof/symbol", auth(pprof.Symbol))
	s.metricsMux.HandleFunc("/debug/pprof/trace", auth(pprof.Trace))
}

// backendGauges snapshots every balancer group's backends into per-backend
// health/circuit-state gauges for the metrics scrape. It reads IsHealthy() for
// backend_up and, when a circuit breaker is configured, GetState() for the
// circuit state (0=closed, 1=open, 2=half-open). Duplicate URLs across groups
// are de-duplicated so each backend appears once.
func (s *Server) backendGauges() []metrics.BackendGauge {
	groups := []balancer.Balancer{s.balancer}
	if s.router != nil {
		// Groups()[0] is the default balancer (already included above); appending
		// the full slice is fine because de-duplication below collapses the repeat.
		groups = append(groups, s.router.Groups()...)
	}
	if s.canary != nil {
		groups = append(groups, s.canary)
	}

	seen := make(map[string]struct{})
	var gauges []metrics.BackendGauge
	for _, g := range groups {
		if g == nil {
			continue
		}
		for _, b := range g.All() {
			if b == nil {
				continue
			}
			if _, dup := seen[b.URL]; dup {
				continue
			}
			seen[b.URL] = struct{}{}

			state := 0
			if s.circuitBreaker != nil {
				state = int(s.circuitBreaker.GetState(b))
			}
			gauges = append(gauges, metrics.BackendGauge{
				URL:          b.URL,
				Up:           b.IsHealthy(),
				CircuitState: state,
			})
		}
	}
	return gauges
}

// adminAuth wraps an admin handler, enforcing a bearer token when
// cfg.Metrics.AuthToken is set. With no token configured it is a passthrough.
func (s *Server) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	token := s.cfg.Metrics.AuthToken
	if token == "" {
		return next
	}
	expected := "Bearer " + token
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(expected)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// handleHealthz is a liveness probe: 200 whenever the process is serving.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleReadyz is a readiness probe: 200 when at least one backend across any
// balancer group (default, routes, canary) is healthy; 503 otherwise.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.hasHealthyBackend() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
		return
	}
	http.Error(w, "no healthy backends", http.StatusServiceUnavailable)
}

// hasHealthyBackend reports whether any backend in any balancer group is healthy.
func (s *Server) hasHealthyBackend() bool {
	groups := []balancer.Balancer{s.balancer}
	if s.router != nil {
		groups = append(groups, s.router.Groups()...)
	}
	if s.canary != nil {
		groups = append(groups, s.canary)
	}
	for _, g := range groups {
		if g != nil && len(g.GetHealthy()) > 0 {
			return true
		}
	}
	return false
}

// handleReload triggers a configuration reload. Only POST is accepted.
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	s.reloadConfig()
	w.WriteHeader(http.StatusOK)
}

// balancerGroups returns every distinct balancer group the server manages: the
// default group, each per-route group (when the L7 router is installed), and the
// canary group (when enabled). nil groups are skipped. Used by the admin API to
// inspect and mutate backends across all groups.
func (s *Server) balancerGroups() []balancer.Balancer {
	groups := []balancer.Balancer{s.balancer}
	if s.router != nil {
		// Groups()[0] is the default balancer (already included); duplicates are
		// harmless because callers de-duplicate by pointer/URL.
		groups = append(groups, s.router.Groups()...)
	}
	if s.canary != nil {
		groups = append(groups, s.canary)
	}
	return groups
}

// findBackend locates the live *Backend with the given URL across the default,
// per-route, and canary balancer groups, returning the backend and its owning
// group (so the caller can call UpdateWeight on the right group). It returns
// (nil, nil) when no group holds a backend with that URL.
func (s *Server) findBackend(url string) (*balancer.Backend, balancer.Balancer) {
	for _, g := range s.balancerGroups() {
		if g == nil {
			continue
		}
		for _, b := range g.All() {
			if b != nil && b.URL == url {
				return b, g
			}
		}
	}
	return nil, nil
}

// adminBackend is the JSON shape returned by GET /admin/backends.
type adminBackend struct {
	URL          string `json:"url"`
	Healthy      bool   `json:"healthy"`
	ActiveConns  int    `json:"active_conns"`
	Weight       int    `json:"weight"`
	CircuitState string `json:"circuit_state"`
}

// handleAdminBackends serves GET /admin/backends: a JSON array of every backend
// across all balancer groups, de-duplicated by URL. Each entry reports live
// health, active connections, weight, and (when a circuit breaker is configured)
// the breaker state string; with no breaker the state is reported as "closed".
func (s *Server) handleAdminBackends(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	seen := make(map[string]struct{})
	out := make([]adminBackend, 0)
	for _, g := range s.balancerGroups() {
		if g == nil {
			continue
		}
		for _, b := range g.All() {
			if b == nil {
				continue
			}
			if _, dup := seen[b.URL]; dup {
				continue
			}
			seen[b.URL] = struct{}{}

			state := circuit.StateClosed
			if s.circuitBreaker != nil {
				state = s.circuitBreaker.GetState(b)
			}
			out = append(out, adminBackend{
				URL:          b.URL,
				Healthy:      b.IsHealthy(),
				ActiveConns:  b.GetActiveConns(),
				Weight:       b.GetWeight(),
				CircuitState: state.String(),
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// adminBackendByURL is the common preamble for the mutating admin endpoints: it
// enforces POST, reads the ?url= query param, and resolves the backend. On any
// failure it writes the appropriate error response and returns (nil, nil, false);
// callers should return immediately when ok is false.
func (s *Server) adminBackendByURL(w http.ResponseWriter, r *http.Request) (*balancer.Backend, balancer.Balancer, bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return nil, nil, false
	}
	url := r.URL.Query().Get("url")
	if url == "" {
		http.Error(w, "missing url parameter", http.StatusBadRequest)
		return nil, nil, false
	}
	be, group := s.findBackend(url)
	if be == nil {
		http.Error(w, "backend not found", http.StatusNotFound)
		return nil, nil, false
	}
	return be, group, true
}

// handleAdminDrain serves POST /admin/drain?url=...: marks the backend unhealthy
// so the balancer stops routing to it (connection draining), without removing it.
func (s *Server) handleAdminDrain(w http.ResponseWriter, r *http.Request) {
	be, _, ok := s.adminBackendByURL(w, r)
	if !ok {
		return
	}
	be.SetHealthy(false)
	logging.Info("Backend drained via admin API", map[string]interface{}{"backend": be.URL})
	w.WriteHeader(http.StatusOK)
}

// handleAdminUndrain serves POST /admin/undrain?url=...: marks the backend healthy
// again so it re-enters rotation.
func (s *Server) handleAdminUndrain(w http.ResponseWriter, r *http.Request) {
	be, _, ok := s.adminBackendByURL(w, r)
	if !ok {
		return
	}
	be.SetHealthy(true)
	logging.Info("Backend undrained via admin API", map[string]interface{}{"backend": be.URL})
	w.WriteHeader(http.StatusOK)
}

// handleAdminWeight serves POST /admin/weight?url=...&weight=N: updates the
// backend's weight on its owning balancer group via UpdateWeight.
func (s *Server) handleAdminWeight(w http.ResponseWriter, r *http.Request) {
	be, group, ok := s.adminBackendByURL(w, r)
	if !ok {
		return
	}
	weightStr := r.URL.Query().Get("weight")
	weight, err := strconv.Atoi(weightStr)
	if err != nil {
		http.Error(w, "invalid weight parameter", http.StatusBadRequest)
		return
	}
	if weight < 0 {
		http.Error(w, "weight must be non-negative", http.StatusBadRequest)
		return
	}
	group.UpdateWeight(be, weight)
	logging.Info("Backend weight updated via admin API", map[string]interface{}{
		"backend": be.URL,
		"weight":  weight,
	})
	w.WriteHeader(http.StatusOK)
}

// handleAdminCircuitReset serves POST /admin/circuit/reset?url=...: resets the
// circuit breaker state for the backend (back to closed). When no circuit breaker
// is configured it responds 200 as a no-op.
func (s *Server) handleAdminCircuitReset(w http.ResponseWriter, r *http.Request) {
	be, _, ok := s.adminBackendByURL(w, r)
	if !ok {
		return
	}
	if s.circuitBreaker != nil {
		s.circuitBreaker.Reset(be)
		logging.Info("Circuit breaker reset via admin API", map[string]interface{}{"backend": be.URL})
	}
	w.WriteHeader(http.StatusOK)
}

// handleAdminCanaryStatus serves GET /admin/canary/status. When no
// auto-promoter is configured it responds with {"enabled":false}; otherwise it
// encodes the full AutoPromoterStatus snapshot as JSON.
func (s *Server) handleAdminCanaryStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.autoPromoter == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"enabled":false}`))
		return
	}
	status := s.autoPromoter.Status()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(status); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}

func (s *Server) setupHealthCheck() {
	// Run one HealthChecker per balancer group so every routed group's backends
	// are probed independently. The default group's overrides come from the
	// top-level cfg.Backends; each per-route group's overrides come from that
	// route's own backends. When no routes are configured this is exactly one
	// checker over the default balancer, matching the previous behavior.
	s.healthChks = append(s.healthChks, health.NewHealthChecker(
		s.balancer,
		s.cfg.LoadBalancer.HealthCheck,
		backendOverrides(s.cfg.Backends),
		s.backendTLS,
	))

	// Per-route groups (only when the router is installed). Groups()[0] is the
	// default balancer (already covered above), so skip it and map each remaining
	// group to its route's backends in order.
	if s.router != nil {
		groups := s.router.Groups()
		for i, rc := range s.cfg.Routes {
			// groups[0] is the default; per-route groups follow in cfg.Routes order.
			gi := i + 1
			if gi >= len(groups) {
				break
			}
			s.healthChks = append(s.healthChks, health.NewHealthChecker(
				groups[gi],
				s.cfg.LoadBalancer.HealthCheck,
				backendOverrides(rc.Backends),
				s.backendTLS,
			))
		}
	}

	// Canary group (§9.1): when a canary pool is installed, probe it with its own
	// health checker so canary backends are ejected/reinstated independently, using
	// the canary block's per-backend overrides.
	if s.canary != nil {
		s.healthChks = append(s.healthChks, health.NewHealthChecker(
			s.canary,
			s.cfg.LoadBalancer.HealthCheck,
			backendOverrides(s.cfg.Canary.Backends),
			s.backendTLS,
		))
	}

	for _, hc := range s.healthChks {
		hc.Start()
	}
}

// backendOverrides builds the per-backend health-check override map from any
// backend that declares its own health_check block; backends without one fall
// through to the global config. Returns nil when no backend overrides exist.
func backendOverrides(backends []config.BackendConfig) map[string]config.HealthCheckConfig {
	var overrides map[string]config.HealthCheckConfig
	for _, bc := range backends {
		if bc.HealthCheck != nil {
			if overrides == nil {
				overrides = make(map[string]config.HealthCheckConfig)
			}
			overrides[bc.URL] = *bc.HealthCheck
		}
	}
	return overrides
}

func (s *Server) setupHTTPServer() {
	var handler http.Handler = s.proxy

	// Cache sits closest to the proxy (inside Gzip) so it stores and replays the
	// UNCOMPRESSED upstream response body; Gzip then compresses cache hits and
	// misses alike on the way out. Opt-in: a disabled cache is a passthrough, so a
	// zero-value/disabled Cache config leaves the chain unchanged.
	if s.cfg.Cache.Enabled {
		handler = middleware.Cache(s.cfg.Cache)(handler)
	}

	// Gzip sits closest to the proxy so it compresses the upstream response body.
	// GzipWithConfig applies the ContentTypes allowlist and MinSize threshold; a
	// zero-value Compression config behaves exactly like the legacy Gzip.
	if s.cfg.Compression.Enabled {
		handler = middleware.GzipWithConfig(s.cfg.Compression, handler)
	}

	if s.cfg.RateLimiter.Enabled {
		handler = middleware.RateLimit(s.cfg.RateLimiter, s.limiter, s.trusted, s.metrics)(handler)
	}

	handler = middleware.Metrics(s.metrics)(handler)
	handler = middleware.Logging(handler)

	// MaxBytes caps request body size early in the chain (before logging/metrics
	// so oversized payloads are rejected without being fully read downstream).
	maxBody := s.cfg.Server.MaxRequestBodyBytes
	if maxBody == 0 {
		maxBody = maxRequestBodyBytes // fallback to hardcoded default
	}
	handler = middleware.MaxBytes(maxBody)(handler)

	// Transform middleware (§9). Each block is opt-in and only installed when
	// enabled, so a zero-value config leaves the chain unchanged. Wrapping is
	// applied inside-out below, yielding this outer->inner request flow around the
	// stack built above:
	//
	//   ... -> Rewrite/HTTPSRedirect -> FaultInjection -> Mirror -> MaxBytes -> ...
	//
	// Rewrite sits outermost of the three so an HTTPS redirect or path/header
	// rewrite happens before fault injection and mirroring; its response-header
	// edits wrap (and therefore apply to) responses produced anywhere downstream,
	// including fault-abort responses. FaultInjection runs next so injected
	// delays/aborts short-circuit before the request is buffered for mirroring.
	// Mirror sits innermost (just above MaxBytes) so the shadow copy carries the
	// already-rewritten request and never mirrors a request that Fault aborted.
	if s.cfg.Mirror.Enabled {
		handler = middleware.Mirror(s.cfg.Mirror)(handler)
	}
	if s.cfg.FaultInjection.Enabled {
		handler = middleware.FaultInjection(s.cfg.FaultInjection)(handler)
	}
	handler = middleware.Rewrite(s.cfg.Rewrite)(handler)

	// Edge security middleware (§6). Each block is opt-in and only installed
	// when enabled/non-empty, so a zero-value Security config leaves the chain
	// unchanged. Wrapping order below is applied inside-out, yielding this
	// outer->inner request flow around the stack built above:
	//
	//   Recover -> SecurityHeaders -> CORS -> ACL -> Auth -> OIDCIntrospect -> MaxBytes -> ...
	//
	// ACL and Auth run early so unauthorized/blocked requests are rejected
	// before any proxying or body reading; SecurityHeaders and CORS sit
	// outermost (nearest the client) so their headers apply to every response,
	// including those short-circuited by Auth/ACL.
	sec := s.cfg.Security

	// OIDCIntrospect is the innermost of the auth block: it validates Bearer
	// tokens via RFC 7662 after JWT/basic/apikey auth has already passed. It must
	// be applied first (innermost) so that Auth, applied next, becomes the outer
	// (first-executing) wrapper. Execution order is Auth → OIDCIntrospect → proxy.
	// When Auth type is "none" and only OIDC introspection is configured, it acts
	// as the sole auth gate and the Auth wrapper is absent.
	if sec.Auth.OIDCIntrospection.Enabled {
		handler = middleware.OIDCIntrospect(sec.Auth.OIDCIntrospection)(handler)
	}

	// Auth sits just outside OIDCIntrospect so it runs first: a request that
	// fails JWT/basic/apikey auth is rejected before the introspection round-trip.
	if authEnabled(sec.Auth) {
		handler = middleware.Auth(sec.Auth)(handler)
	}
	if aclEnabled(sec.ACL) {
		handler = middleware.ACL(sec.ACL, s.trusted)(handler)
	}
	if sec.CORS.Enabled {
		handler = middleware.CORS(sec.CORS)(handler)
	}
	if sec.Headers.Enabled {
		handler = middleware.SecurityHeaders(sec.Headers)(handler)
	}

	// Observability (§7): AccessLog wraps the response so it records the final
	// status/bytes/duration for every request, and RequestID runs just outside it
	// so the request id is minted before AccessLog logs and is visible (via
	// context + request header) to every inner middleware and the upstream. These
	// are enabled by default; AccessLog samples every request (sampleN=1).
	handler = middleware.AccessLog(1)(handler)
	handler = middleware.RequestID(middleware.DefaultRequestIDHeader)(handler)

	// Recover is the OUTERMOST middleware so it catches panics from every inner
	// middleware and the proxy handler.
	handler = middleware.Recover(handler)

	// Tracing wraps the fully assembled handler so spans cover the complete
	// request lifecycle including all middleware. Only installed when tracing is
	// enabled to avoid importing otelhttp overhead in the disabled case (the noop
	// provider still avoids any network activity, but skipping the wrapper is
	// cleaner in production configs that do not use tracing).
	if s.cfg.Tracing.Enabled {
		handler = tracing.Middleware()(handler)
	}

	// Admission ceiling is the absolute outermost gate: it rejects or queues
	// requests before any other middleware runs, bounding the total concurrency
	// of the server under load.
	if s.cfg.Server.MaxInflightRequests > 0 {
		handler = middleware.Admission(
			s.cfg.Server.MaxInflightRequests,
			s.cfg.Server.MaxInflightQueue,
			s.cfg.Server.QueueTimeout,
			s.metrics,
		)(handler)
	}

	s.httpServer = &http.Server{
		Addr:              s.cfg.GetAddr(),
		Handler:           handler,
		ReadTimeout:       s.cfg.Server.ReadTimeout,
		ReadHeaderTimeout: s.cfg.Server.ReadHeaderTimeout,
		WriteTimeout:      s.cfg.Server.WriteTimeout,
		IdleTimeout:       s.cfg.Server.IdleTimeout,
		MaxHeaderBytes:    s.cfg.Server.MaxHeaderBytes,
	}

	// When TLS is enabled, build the server *tls.Config from the full TLS config
	// (min version, cipher suites, SNI certificates, client-auth/mTLS, hot
	// reload). Installing it on the http.Server lets Start call
	// ListenAndServeTLS("", "") so the certificates come from the tls.Config
	// rather than the file arguments. A plain cert_file/key_file-only config
	// still works: ServerTLSConfig loads that primary keypair and installs a
	// GetCertificate resolver that returns it, preserving current behavior.
	if s.cfg.TLS.Enabled {
		s.setupTLS()
	}
}

// setupTLS builds the server *tls.Config and installs it on httpServer. It
// handles three cases:
//
//   - ACME (cfg.TLS.ACME.Enabled): the TLS config's GetCertificate is backed by
//     an autocert.Manager that obtains/renews certificates automatically. The
//     shared HTTP-01 challenge handler is retained for Start to serve on
//     cfg.TLS.ACME.HTTPChallengePort. ACME takes precedence over static certs.
//   - static cert_file/key_file (with optional SNI, mTLS, and hot-reload): the
//     primary keypair (and any SNI certificates) are loaded and served.
//   - OCSP stapling (cfg.TLS.OCSPStapling, static path only): a Stapler is built
//     over the EXACT served certificates so refreshed staples take effect on the
//     next handshake. Started/stopped with the server.
//
// On any construction error it logs and leaves httpServer.TLSConfig nil so Start
// surfaces the problem instead of silently serving plaintext.
func (s *Server) setupTLS() {
	if s.cfg.TLS.ACME.Enabled {
		// Build the shared ACME state once so the TLS listener and the HTTP-01
		// challenge listener use the same autocert.Manager (shared account and
		// challenge state).
		tlsCfg, challengeHandler, err := tlsutil.NewACMEManager(s.cfg.TLS)
		if err != nil {
			logging.Error("Invalid ACME TLS configuration", map[string]interface{}{
				"error": err.Error(),
			})
			return
		}
		s.httpServer.TLSConfig = tlsCfg
		s.acmeChallengeHandler = challengeHandler
		return
	}

	// Static-cert path. Load the served certificates so OCSP stapling (when
	// enabled) can install staples on the exact certs presented at handshake.
	tlsCfg, certs, err := tlsutil.ServerTLSConfigWithCerts(s.cfg.TLS)
	if err != nil {
		// Fail loud: a misconfigured TLS block means the server cannot serve
		// TLS. Leave TLSConfig nil so Start surfaces the problem when it tries
		// to serve (ListenAndServeTLS with empty file args and no config
		// certificates errors), rather than silently serving plaintext.
		logging.Error("Invalid TLS configuration", map[string]interface{}{
			"error": err.Error(),
		})
		return
	}
	s.httpServer.TLSConfig = tlsCfg

	if s.cfg.TLS.OCSPStapling && len(certs) > 0 {
		// NewStapler skips certs that advertise no OCSP responder or lack an
		// issuer in their chain, so a self-signed cert simply yields an empty
		// (no-op) Stapler. The Stapler is started in Start and stopped in Stop.
		s.stapler = tlsutil.NewStapler(certs)
	}
}

// startGRPCHealth creates and starts the gRPC Health Checking Protocol server
// when cfg.LoadBalancer.GRPCHealth.Enabled. It registers the health service and
// optionally gRPC reflection, then listens on the configured port. A background
// goroutine polls hasHealthyBackend() every 5 s to keep the "proxy" service
// status current. Must be called from Start().
func (s *Server) startGRPCHealth() {
	ghCfg := s.cfg.LoadBalancer.GRPCHealth
	if !ghCfg.Enabled {
		return
	}

	s.grpcHealthServer = health.NewGRPCHealthServer()

	// Seed the "proxy" service status before accepting connections.
	if s.hasHealthyBackend() {
		s.grpcHealthServer.SetServingStatus("proxy", grpc_health_v1.HealthCheckResponse_SERVING)
	} else {
		s.grpcHealthServer.SetServingStatus("proxy", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	}

	s.grpcHealthSrv = grpc.NewServer()
	s.grpcHealthServer.Register(s.grpcHealthSrv)
	if ghCfg.Reflection {
		health.RegisterReflection(s.grpcHealthSrv)
	}

	addr := net.JoinHostPort(s.cfg.Server.Host, strconv.Itoa(ghCfg.Port))
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		logging.Error("Failed to start gRPC health server", map[string]interface{}{
			"addr":  addr,
			"error": err.Error(),
		})
		s.grpcHealthSrv = nil
		s.grpcHealthServer = nil
		return
	}

	logging.Info("Starting gRPC health server", map[string]interface{}{
		"addr":       addr,
		"reflection": ghCfg.Reflection,
	})

	go func() {
		if err := s.grpcHealthSrv.Serve(lis); err != nil {
			logging.Error("gRPC health server error", map[string]interface{}{
				"error": err.Error(),
			})
		}
	}()

	// Background goroutine: refresh the "proxy" service status every 5 s so
	// health-check clients see up-to-date state as backends come and go.
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if s.grpcHealthServer == nil {
				return
			}
			if s.hasHealthyBackend() {
				s.grpcHealthServer.SetServingStatus("proxy", grpc_health_v1.HealthCheckResponse_SERVING)
			} else {
				s.grpcHealthServer.SetServingStatus("proxy", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
			}
		}
	}()
}

// authEnabled reports whether the Auth middleware should be installed: only when
// a concrete auth Type other than "none" is configured. An empty or "none" Type
// means no authentication, matching middleware.Auth's own passthrough behavior.
func authEnabled(cfg config.AuthConfig) bool {
	t := strings.ToLower(strings.TrimSpace(cfg.Type))
	return t != "" && t != "none"
}

// aclEnabled reports whether the ACL middleware should be installed: only when
// at least one rule set (Allow/Deny CIDRs, Methods allowlist, or BlockedPaths)
// is non-empty. This mirrors middleware.ACL's own no-op short-circuit.
func aclEnabled(cfg config.ACLConfig) bool {
	return len(cfg.Allow) > 0 || len(cfg.Deny) > 0 || len(cfg.Methods) > 0 || len(cfg.BlockedPaths) > 0
}

// Start begins serving on all configured listeners and blocks until the server is stopped.
func (s *Server) Start() error {
	logging.Info("Starting server", map[string]interface{}{
		"addr": s.cfg.GetAddr(),
	})

	// Setup OpenTelemetry tracing. When cfg.Tracing.Enabled is false, Setup
	// installs a noop provider and returns a no-op shutdown, so this call is safe
	// to make unconditionally. The returned shutdown is stored so Stop() can flush
	// any buffered spans before the process exits.
	tracingCfg := tracing.Config{
		Enabled:     s.cfg.Tracing.Enabled,
		Exporter:    s.cfg.Tracing.Exporter,
		Endpoint:    s.cfg.Tracing.Endpoint,
		SampleRate:  s.cfg.Tracing.SampleRate,
		ServiceName: s.cfg.Tracing.ServiceName,
	}
	tracingShutdown, tracingErr := tracing.Setup(tracingCfg)
	if tracingErr != nil {
		logging.Error("Failed to set up tracing; continuing without tracing", map[string]interface{}{
			"error": tracingErr.Error(),
		})
	} else {
		s.tracingShutdown = tracingShutdown
	}

	if s.cfg.Metrics.Enabled {
		s.metricsServer = &http.Server{
			Addr:              net.JoinHostPort(s.cfg.Metrics.Host, strconv.Itoa(s.cfg.Metrics.Port)),
			Handler:           s.metricsMux,
			ReadTimeout:       10 * time.Second,
			ReadHeaderTimeout: s.cfg.Server.ReadHeaderTimeout,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    s.cfg.Server.MaxHeaderBytes,
		}
		go func() {
			logging.Info("Starting metrics server", map[string]interface{}{
				"addr": s.metricsServer.Addr,
			})
			if err := s.metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logging.Error("Metrics server error", map[string]interface{}{
					"error": err.Error(),
				})
			}
		}()
	}

	// Start the optional gRPC Health Checking Protocol server (if enabled).
	s.startGRPCHealth()

	// Start the optional config file-watch loop. When cfg.Server.WatchConfig is
	// set, a background goroutine polls the config file's mtime every
	// WatchInterval and calls reloadConfig on change. Disabled by default, so
	// existing SIGHUP / POST /reload behavior is unchanged when off.
	s.startConfigWatch()

	// Start DNS service discovery (if configured) before serving so discovered
	// backends can be resolved and registered as the listener comes up. Each target
	// resolves immediately on Start, then re-resolves on its interval.
	if s.discoverer != nil {
		logging.Info("Starting DNS service discovery", map[string]interface{}{
			"targets": len(s.cfg.Discovery.DNS),
		})
		s.discoverer.Start()
	}

	// Start Kubernetes Endpoints service discovery (if configured) before serving
	// so discovered backends are available as the listener comes up.
	if s.k8sDiscoverer != nil {
		logging.Info("Starting Kubernetes Endpoints service discovery", map[string]interface{}{
			"namespace": s.cfg.Discovery.Kubernetes.Namespace,
			"service":   s.cfg.Discovery.Kubernetes.Service,
		})
		s.k8sDiscoverer.Start()
	}

	// Start the optional canary auto-promoter. When canary is enabled and
	// AutoPromote.Enabled is true, the promoter steps the canary weight up each
	// StepInterval (rolling back on degradation if configured). It is stopped in
	// Stop() before the main HTTP listener shuts down.
	if s.cfg.Canary.Enabled && s.cfg.Canary.AutoPromote.Enabled {
		s.autoPromoter = canary.New(s.proxy, s.metrics, s.cfg.Canary.AutoPromote)
		s.autoPromoter.WithMetricsUpdater(s.metrics)
		logging.Info("Starting canary auto-promoter", map[string]interface{}{
			"step_percent":         s.cfg.Canary.AutoPromote.StepPercent,
			"step_interval":        s.cfg.Canary.AutoPromote.StepInterval.String(),
			"max_weight_percent":   s.cfg.Canary.AutoPromote.MaxWeightPercent,
			"error_rate_threshold": s.cfg.Canary.AutoPromote.ErrorRateThreshold,
		})
		s.autoPromoter.Start()
	}

	// Start the optional raw TCP (L4) proxy on its own port. Start binds the
	// listener and serves in the background, so a bind failure surfaces
	// synchronously here while the accept loop runs concurrently with the HTTP
	// server below.
	if s.l4Proxy != nil {
		l4Addr := net.JoinHostPort(s.cfg.Server.Host, strconv.Itoa(s.cfg.Server.L4.Port))
		logging.Info("Starting L4 (TCP) proxy", map[string]interface{}{
			"addr": l4Addr,
		})
		if err := s.l4Proxy.Start(l4Addr); err != nil {
			return err
		}
	}

	// Start the ACME HTTP-01 challenge listener (if ACME is enabled) on its own
	// port before serving TLS, so the CA can reach the challenge endpoint while
	// the manager provisions certificates on the first handshake. The handler is
	// the shared autocert manager's HTTPHandler, so challenge state is shared with
	// the TLS listener's GetCertificate.
	if s.acmeChallengeHandler != nil {
		port := s.cfg.TLS.ACME.HTTPChallengePort
		if port == 0 {
			port = 80
		}
		acmeAddr := net.JoinHostPort(s.cfg.Server.Host, strconv.Itoa(port))
		s.acmeServer = &http.Server{
			Addr:              acmeAddr,
			Handler:           s.acmeChallengeHandler,
			ReadTimeout:       10 * time.Second,
			ReadHeaderTimeout: s.cfg.Server.ReadHeaderTimeout,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    s.cfg.Server.MaxHeaderBytes,
		}
		go func() {
			logging.Info("Starting ACME HTTP-01 challenge server", map[string]interface{}{
				"addr": acmeAddr,
			})
			if err := s.acmeServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logging.Error("ACME challenge server error", map[string]interface{}{
					"error": err.Error(),
				})
			}
		}()
	}

	// Start OCSP stapling (if enabled): an initial synchronous refresh installs
	// staples onto the served certificates, then a background loop re-fetches
	// ahead of each response's NextUpdate. A failing responder is logged but does
	// not block startup (transient outages self-heal on the next tick).
	if s.stapler != nil {
		logging.Info("Starting OCSP stapling", nil)
		if err := s.stapler.Start(context.Background()); err != nil {
			logging.Warn("Initial OCSP staple refresh failed", map[string]interface{}{
				"error": err.Error(),
			})
		}
	}

	// Start HTTP/3 (QUIC) listener if configured. The Alt-Svc middleware is
	// applied to the main handler so clients receive the upgrade advertisement
	// on every response from the HTTPS listener too.
	if s.cfg.Server.HTTP3.Enabled {
		h3Handler := altSvcMiddleware(s.cfg.Server.HTTP3.Port)(s.httpServer.Handler)
		h3srv, h3err := s.startH3(h3Handler)
		if h3err != nil {
			logging.Error("Failed to start HTTP/3 server", map[string]interface{}{
				"error": h3err.Error(),
			})
		} else {
			s.h3Server = h3srv
			logging.Info("HTTP/3 server started", map[string]interface{}{
				"port": s.cfg.Server.HTTP3.Port,
			})
			// Also inject Alt-Svc on the main HTTPS listener so upgrade
			// hints reach clients before they switch to QUIC.
			s.httpServer.Handler = h3Handler
		}
	}

	var err error
	if s.cfg.TLS.Enabled {
		// Certificates (including SNI selection, mTLS client-auth, and hot
		// reload) are supplied by s.httpServer.TLSConfig, built in
		// setupHTTPServer. Pass empty file arguments so ListenAndServeTLS uses
		// the tls.Config's GetCertificate/Certificates instead of loading files
		// again.
		err = s.httpServer.ListenAndServeTLS("", "")
	} else {
		err = s.httpServer.ListenAndServe()
	}

	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Stop gracefully shuts down all listeners and background goroutines within the configured shutdown timeout.
func (s *Server) Stop() error {
	logging.Info("Shutting down server", nil)

	timeout := s.cfg.Server.ShutdownTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Stop the config file-watch goroutine (if running) before the rest of the
	// teardown so it cannot fire a reload mid-shutdown.
	s.stopConfigWatch()

	// Stop the canary auto-promoter (if running) before other subsystems so it
	// cannot issue a weight update mid-shutdown.
	if s.autoPromoter != nil {
		s.autoPromoter.Stop()
	}

	// Stop DNS discovery FIRST (before the health checkers) so no new backends are
	// added to the balancer mid-shutdown; a resolve in flight cannot register a
	// backend after the health checkers have been torn down.
	if s.discoverer != nil {
		s.discoverer.Stop()
	}

	// Stop Kubernetes Endpoints discovery alongside DNS discovery, for the same
	// reason: no new backends should be registered mid-shutdown.
	if s.k8sDiscoverer != nil {
		s.k8sDiscoverer.Stop()
	}

	if s.limiter != nil {
		s.limiter.Stop()
	}

	// Stop the distributed circuit-breaker syncer before the health checkers so
	// no more Redis pushes can race with shutdown (Bug 2 — syncer must be stopped).
	if s.redisSyncer != nil {
		s.redisSyncer.Stop()
	}

	for _, hc := range s.healthChks {
		hc.Stop()
	}

	if s.l4Proxy != nil {
		// Stop closes the listener, force-closes in-flight connections, and waits
		// for all handler goroutines to finish.
		if err := s.l4Proxy.Stop(); err != nil {
			logging.Error("L4 proxy shutdown error", map[string]interface{}{
				"error": err.Error(),
			})
		}
	}

	// Stop OCSP staple refreshing before shutting down the TLS listener so its
	// background goroutines exit cleanly. Safe to call when never started.
	if s.stapler != nil {
		s.stapler.Stop()
	}

	// Shut down the ACME HTTP-01 challenge listener (if running).
	if s.acmeServer != nil {
		_ = s.acmeServer.Shutdown(ctx)
	}

	if s.metricsServer != nil {
		_ = s.metricsServer.Shutdown(ctx)
	}

	// Gracefully stop the gRPC health server (if running). GracefulStop drains
	// in-flight RPCs before closing; it is safe to call when grpcHealthSrv is nil.
	if s.grpcHealthSrv != nil {
		s.grpcHealthSrv.GracefulStop()
	}

	// Shut down the HTTP/3 (QUIC) server if it was started. stopH3 is a no-op
	// when s.h3Server is nil. Errors are logged inside stopH3; they do not
	// prevent the main HTTP listener from being shut down.
	stopH3(s.h3Server, timeout)

	// Flush and shut down the TracerProvider (if started). This must happen after
	// all HTTP listeners are closed to guarantee no more spans are created.
	if s.tracingShutdown != nil {
		if tErr := s.tracingShutdown(ctx); tErr != nil {
			logging.Error("Tracing shutdown error", map[string]interface{}{
				"error": tErr.Error(),
			})
		}
	}

	return s.httpServer.Shutdown(ctx)
}

// Run starts the server and blocks, handling SIGINT/SIGTERM (graceful stop) and SIGHUP (config reload).
func (s *Server) Run() error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	// Surface a startup failure (e.g. port already in use) instead of blocking on
	// signals forever with no server listening.
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Start()
	}()

	for {
		select {
		case err := <-errCh:
			if err != nil {
				logging.Error("Server failed to start", map[string]interface{}{
					"error": err.Error(),
				})
				return err
			}
			// Start returned nil because the server was shut down elsewhere.
			return nil
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				logging.Info("Received SIGHUP, reloading configuration", nil)
				s.reloadConfig()
				continue
			}
			logging.Info("Received shutdown signal", nil)
			return s.Stop()
		}
	}
}

// reloadConfig re-reads the config from the path the process was started with and
// applies the settings that can be changed at runtime. Rate-limiter and logging
// changes are applied live, as are DEFAULT-GROUP backend topology changes (adds,
// removes, and weight updates) via applyBackendChanges. The load-balancing
// ALGORITHM and ROUTE/CANARY group topology still require a restart; those
// limitations are logged rather than silently ignored.
func (s *Server) reloadConfig() {
	// Serialize reloads so a SIGHUP and a POST /reload cannot race on s.cfg, and
	// so two reloads cannot concurrently diff/mutate the balancer.
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	newCfg, err := config.Load(s.configPath)
	if err != nil {
		logging.Error("Failed to reload config", map[string]interface{}{
			"error": err.Error(),
		})
		return
	}

	logging.Configure(newCfg.Logging.Level, newCfg.Logging.Format)

	if newCfg.RateLimiter.Enabled && s.limiter != nil {
		nrl := newCfg.RateLimiter
		s.limiter.UpdateRates(
			float64(nrl.RequestsPerSecond),
			nrl.Burst,
			float64(nrl.GlobalRPS),
			nrl.GlobalBurst,
		)
	}

	// Apply default-group backend changes live (add/remove/weight). This mutates
	// the balancer, so it runs under reloadMu. The health checker re-reads
	// balancer.All() each cycle, so freshly added backends are health-checked
	// automatically; NewBackend defaults healthy=true, so an added backend can
	// receive traffic before its first probe (documented, acceptable).
	s.applyBackendChanges(newCfg.Backends)

	// Algorithm changes are NOT live: the base balancer + wrapper stack is built
	// once at New. Warn (only) when the algorithm changed so the operator knows a
	// restart is required.
	if s.cfg.LoadBalancer.Algorithm != newCfg.LoadBalancer.Algorithm {
		logging.Warn("Load-balancing algorithm change requires a restart to take effect", nil)
	}

	// Route hot-reload: when the route table changed and a router is installed,
	// atomically swap in the new route table. Readers (serveHTTP) see either the
	// full old or the full new table, never a mix. If the new config adds routes
	// but no router exists yet (i.e. the original config had none), we can't
	// install one without rebuilding the proxy's handler chain, so we warn instead.
	if !routeGroupsEqual(s.cfg.Routes, newCfg.Routes) {
		if s.router != nil {
			if err := s.router.UpdateRoutes(newCfg.Routes); err != nil {
				logging.Error("Failed to apply route changes; existing routes unchanged", map[string]interface{}{
					"error": err.Error(),
				})
			} else {
				logging.Info("Routes reloaded", map[string]interface{}{
					"routes": len(newCfg.Routes),
				})
			}
		} else {
			logging.Warn("Route changes require a restart when no router was initially configured", nil)
		}
	}

	// Canary weight hot-reload: when only the weight changed, update it atomically
	// via UpdateCanaryWeight (which is serialized by randMu). Topology changes
	// (different backends or algorithm) still require a restart.
	if !canaryEqual(s.cfg.Canary, newCfg.Canary) {
		oldBackendsOnly := s.cfg.Canary.Enabled == newCfg.Canary.Enabled &&
			s.cfg.Canary.Algorithm == newCfg.Canary.Algorithm &&
			backendsEqual(s.cfg.Canary.Backends, newCfg.Canary.Backends)
		if oldBackendsOnly && s.cfg.Canary.WeightPercent != newCfg.Canary.WeightPercent {
			s.proxy.UpdateCanaryWeight(newCfg.Canary.WeightPercent)
			logging.Info("Canary weight updated", map[string]interface{}{
				"weight_percent": newCfg.Canary.WeightPercent,
			})
		} else {
			logging.Warn("Canary topology changes require a restart to take effect", nil)
		}
	}

	// Mirror and fault-injection config are baked into the HTTP handler chain at
	// startup and cannot be swapped live without rebuilding the chain. Warn when
	// they change so the operator knows a restart is required.
	if s.cfg.Mirror != newCfg.Mirror {
		logging.Warn("Mirror config changes require a restart to take effect", nil)
	}
	if s.cfg.FaultInjection != newCfg.FaultInjection {
		logging.Warn("Fault-injection config changes require a restart to take effect", nil)
	}

	s.cfg.Logging = newCfg.Logging
	s.cfg.RateLimiter = newCfg.RateLimiter
	s.cfg.Backends = newCfg.Backends
	s.cfg.Routes = newCfg.Routes
	s.cfg.Canary = newCfg.Canary

	logging.Info("Configuration reloaded", nil)
}

// applyBackendChanges diffs the current default-group backends (s.cfg.Backends)
// against newBackends by URL and mutates the balancer to match:
//   - a URL present only in newBackends is added (NewBackend + balancer.Add);
//   - a URL present only in the old set is removed (balancer.Remove) and, when a
//     circuit breaker is configured, its per-backend state is Reset so it does not
//     leak after the *Backend pointer is dropped;
//   - a URL in both whose weight changed is updated via balancer.UpdateWeight.
//
// It must be called with s.reloadMu held. It only touches the default balancer
// group; route and canary groups are out of scope.
func (s *Server) applyBackendChanges(newBackends []config.BackendConfig) {
	// Index the live *Backend pointers by URL so removals/weight changes can find
	// the exact pointer the balancer (and circuit breaker) already track.
	existing := make(map[string]*balancer.Backend)
	for _, be := range s.balancer.All() {
		if be != nil {
			existing[be.URL] = be
		}
	}

	// Index the desired backends by URL for add/weight decisions and removal set.
	desired := make(map[string]config.BackendConfig, len(newBackends))
	for _, bc := range newBackends {
		desired[bc.URL] = bc
	}

	// Adds and weight changes.
	for _, bc := range newBackends {
		be, ok := existing[bc.URL]
		if !ok {
			s.balancer.Add(balancer.NewBackend(bc))
			logging.Info("Backend added via reload", map[string]interface{}{"backend": bc.URL})
			continue
		}
		if be.GetWeight() != bc.Weight {
			s.balancer.UpdateWeight(be, bc.Weight)
			logging.Info("Backend weight updated via reload", map[string]interface{}{
				"backend": bc.URL,
				"weight":  bc.Weight,
			})
		}
	}

	// Removals: any live backend whose URL is no longer desired.
	for url, be := range existing {
		if _, keep := desired[url]; keep {
			continue
		}
		s.balancer.Remove(be)
		if s.circuitBreaker != nil {
			// Drop the breaker's per-backend state so it does not leak once the
			// *Backend pointer is no longer referenced by the balancer.
			s.circuitBreaker.Reset(be)
		}
		logging.Info("Backend removed via reload", map[string]interface{}{"backend": url})
	}
}

// routeGroupsEqual reports whether two route slices are equivalent for the
// purpose of deciding whether a restart-required warning should fire. It compares
// path/algorithm and each route's backend set (URL + weight), which is enough to
// detect the route topology changes reloadConfig cannot apply live.
func routeGroupsEqual(a, b []config.RouteConfig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].PathPrefix != b[i].PathPrefix || a[i].Algorithm != b[i].Algorithm {
			return false
		}
		if !backendsEqual(a[i].Backends, b[i].Backends) {
			return false
		}
	}
	return true
}

// canaryEqual reports whether two canary blocks are equivalent for the
// restart-required warning: enablement, weight, algorithm, and backend set.
func canaryEqual(a, b config.CanaryConfig) bool {
	return a.Enabled == b.Enabled &&
		a.WeightPercent == b.WeightPercent &&
		a.Algorithm == b.Algorithm &&
		backendsEqual(a.Backends, b.Backends)
}

func backendsEqual(a, b []config.BackendConfig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].URL != b[i].URL || a[i].Weight != b[i].Weight || a[i].MaxConns != b[i].MaxConns {
			return false
		}
	}
	return true
}

// startConfigWatch launches the config file-watch goroutine when
// cfg.Server.WatchConfig is enabled. The goroutine polls the config file's mtime
// (and size, to catch same-second edits) every WatchInterval and calls
// reloadConfig only when it changes, so it never fires on a no-change tick. It is
// a no-op when watching is disabled. Uses mtime polling (not fsnotify) per the
// stdlib+x/* dependency constraint.
func (s *Server) startConfigWatch() {
	if !s.cfg.Server.WatchConfig {
		return
	}

	interval := s.cfg.Server.WatchInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}

	// Seed the baseline from the file's current state so the first change (not the
	// first tick) triggers a reload.
	lastMod, lastSize := configFileStamp(s.configPath)

	s.watchStop = make(chan struct{})
	s.watchWG.Add(1)

	logging.Info("Starting config file watch", map[string]interface{}{
		"path":     s.configPath,
		"interval": interval.String(),
	})

	go func() {
		defer s.watchWG.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.watchStop:
				return
			case <-ticker.C:
				mod, size := configFileStamp(s.configPath)
				// A zero-time mod means Stat failed (e.g. transient rename during an
				// editor save); skip this tick without updating the baseline so the
				// next successful stat still detects the change.
				if mod.IsZero() {
					continue
				}
				if mod.Equal(lastMod) && size == lastSize {
					continue
				}
				lastMod, lastSize = mod, size
				logging.Info("Config file change detected, reloading", map[string]interface{}{
					"path": s.configPath,
				})
				s.reloadConfig()
			}
		}
	}()
}

// stopConfigWatch signals the file-watch goroutine to exit and waits for it. Safe
// to call when watching was never started (watchStop nil): it is then a no-op.
func (s *Server) stopConfigWatch() {
	if s.watchStop == nil {
		return
	}
	close(s.watchStop)
	s.watchWG.Wait()
	s.watchStop = nil
}

// configFileStamp returns the config file's modification time and size. On a Stat
// error it returns the zero time and 0 so callers can skip the tick without
// mistaking a transient failure for a change.
func configFileStamp(path string) (time.Time, int64) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, 0
	}
	return fi.ModTime(), fi.Size()
}

// Handler returns the fully assembled HTTP handler (proxy plus the entire
// middleware chain) that the server serves on its main listener. It exists so the
// complete request-handling stack can be exercised end-to-end via httptest
// without binding a real port or starting background goroutines. It is safe to
// call after New has returned, since setupHTTPServer has already composed the
// chain onto httpServer.Handler by then.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// MetricsMux returns the admin/metrics ServeMux the server serves on its metrics
// listener (cfg.Metrics.Host:Port). It carries /metrics, /metrics.json, /reload,
// and the /debug/pprof/* profiling endpoints, all behind adminAuth. It is exposed
// so the admin surface can be exercised end-to-end via httptest without binding a
// real port; it is safe to call after New returns, since setupMetrics has already
// composed the mux by then.
func (s *Server) MetricsMux() http.Handler {
	return s.metricsMux
}

// GetMetrics returns the Metrics instance used by this server.
func (s *Server) GetMetrics() *metrics.Metrics {
	return s.metrics
}

// GetBalancer returns the default balancer used by this server.
func (s *Server) GetBalancer() balancer.Balancer {
	return s.balancer
}

// redisAddrFromURL extracts a "host:port" address from a Redis URL of the
// form "redis://host:port" or "rediss://host:port". If the URL cannot be
// parsed it is returned verbatim so the caller gets a descriptive dial error.
func redisAddrFromURL(rawURL string) string {
	// Strip the scheme prefix and return the remainder as host:port.
	for _, prefix := range []string{"redis://", "rediss://"} {
		if len(rawURL) > len(prefix) && rawURL[:len(prefix)] == prefix {
			return rawURL[len(prefix):]
		}
	}
	return rawURL
}
