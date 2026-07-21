package health

import (
	"context"
	"crypto/tls"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/config"
	"reverse-proxy-lb/internal/logging"
	"strings"
	"sync"
	"time"
)

// maxHealthBodyBytes caps how much of a health-check response body we read when
// ExpectedBody matching is required. This bounds memory even if a backend
// streams an unbounded response.
const maxHealthBodyBytes = 64 * 1024

// HealthChecker periodically probes backends and flips their health flag using
// separate rise/fall thresholds. Effective per-backend configuration is
// resolved from overrides (keyed by backend URL) falling back to the global
// config, and is recomputed each run so no mutable threshold state is shared
// across goroutines.
type HealthChecker struct {
	balancer  balancer.Balancer
	client    *http.Client
	cfg       config.HealthCheckConfig
	overrides map[string]config.HealthCheckConfig

	startedAt time.Time
	rng       *rand.Rand

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewHealthChecker constructs a HealthChecker. cfg is the global health-check
// configuration; overrides maps a backend URL to a per-backend configuration
// that fully replaces the global one for that backend. backendTLS configures
// TLS for https backends.
func NewHealthChecker(b balancer.Balancer, cfg config.HealthCheckConfig, overrides map[string]config.HealthCheckConfig, backendTLS *tls.Config) *HealthChecker {
	return &HealthChecker{
		balancer: b,
		client: &http.Client{
			Timeout: cfg.Timeout,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				IdleConnTimeout:     30 * time.Second,
				MaxIdleConnsPerHost: 10,
				TLSClientConfig:     backendTLS,
			},
		},
		cfg:       cfg,
		overrides: overrides,
		rng:       rand.New(rand.NewSource(time.Now().UnixNano())), // #nosec G404 -- jitter does not require crypto-strength randomness
		stopCh:    make(chan struct{}),
	}
}

func (h *HealthChecker) Start() {
	h.startedAt = time.Now()
	h.wg.Add(1)
	go h.run()
	logging.Info("Health checker started", map[string]interface{}{
		"interval": h.cfg.Interval,
		"timeout":  h.cfg.Timeout,
	})
}

func (h *HealthChecker) Stop() {
	close(h.stopCh)
	h.wg.Wait()
	logging.Info("Health checker stopped", nil)
}

func (h *HealthChecker) run() {
	defer h.wg.Done()

	// Run an initial pass immediately, then sleep jittered intervals so checks
	// de-synchronize across instances.
	h.checkAll()

	timer := time.NewTimer(h.jitteredInterval())
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			h.checkAll()
			timer.Reset(h.jitteredInterval())
		case <-h.stopCh:
			return
		}
	}
}

// jitteredInterval returns the configured interval randomized by +/- Jitter
// fraction so periodic checks spread out over time.
func (h *HealthChecker) jitteredInterval() time.Duration {
	interval := h.cfg.Interval
	if interval <= 0 {
		return interval
	}
	j := h.cfg.Jitter
	if j <= 0 {
		return interval
	}
	if j > 1 {
		j = 1
	}
	// delta in [-j, +j] * interval
	delta := (h.rng.Float64()*2 - 1) * j
	d := time.Duration(float64(interval) * (1 + delta))
	if d <= 0 {
		d = interval
	}
	return d
}

func (h *HealthChecker) checkAll() {
	backends := h.balancer.All()
	for _, backend := range backends {
		eff := h.effectiveConfig(backend)
		h.checkBackend(backend, eff)
	}
}

// effectiveConfig resolves the per-backend configuration: an override keyed by
// the backend URL if present, otherwise the global config.
func (h *HealthChecker) effectiveConfig(backend *balancer.Backend) config.HealthCheckConfig {
	if h.overrides != nil {
		if ov, ok := h.overrides[backend.URL]; ok {
			return ov
		}
	}
	return h.cfg
}

// checkBackend performs a single probe against backend using the effective
// config and records the outcome. cfg is immutable for the duration of the
// call, so no shared mutable threshold state is read.
func (h *HealthChecker) checkBackend(backend *balancer.Backend, cfg config.HealthCheckConfig) {
	var ok bool
	if strings.EqualFold(cfg.Type, "tcp") {
		ok = h.checkTCP(backend, cfg)
	} else {
		ok = h.checkHTTP(backend, cfg)
	}

	if ok {
		h.markHealthy(backend, cfg)
	} else {
		h.markUnhealthy(backend, cfg)
	}
}

// checkHTTP probes an HTTP(S) backend. It succeeds when the response status is
// accepted (in ExpectedStatuses, or any 2xx when that list is empty) and, if
// ExpectedBody is set, the body contains that substring.
func (h *HealthChecker) checkHTTP(backend *balancer.Backend, cfg config.HealthCheckConfig) bool {
	timeout := cfg.Timeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	method := cfg.Method
	if method == "" {
		method = http.MethodGet
	}

	target := backend.URL + cfg.Path
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return false
	}

	if cfg.Host != "" {
		req.Host = cfg.Host
	}
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return false
	}
	defer func() {
		// Drain a bounded amount so the connection can be reused, then close.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxHealthBodyBytes))
		resp.Body.Close()
	}()

	if !statusAccepted(resp.StatusCode, cfg.ExpectedStatuses) {
		return false
	}

	if cfg.ExpectedBody != "" {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxHealthBodyBytes))
		if err != nil {
			return false
		}
		if !strings.Contains(string(body), cfg.ExpectedBody) {
			return false
		}
	}

	return true
}

// checkTCP succeeds when a TCP connection to the backend host:port can be
// established within the timeout.
func (h *HealthChecker) checkTCP(backend *balancer.Backend, cfg config.HealthCheckConfig) bool {
	addr, err := hostPort(backend.URL)
	if err != nil {
		return false
	}
	conn, err := net.DialTimeout("tcp", addr, cfg.Timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// statusAccepted reports whether code is acceptable: present in expected, or any
// 2xx when expected is empty.
func statusAccepted(code int, expected []int) bool {
	if len(expected) == 0 {
		return code >= 200 && code < 300
	}
	for _, s := range expected {
		if s == code {
			return true
		}
	}
	return false
}

// hostPort derives a dial address (host:port) from a backend URL, filling in the
// default port for the scheme when none is specified.
func hostPort(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(host, port), nil
}

func (h *HealthChecker) markHealthy(backend *balancer.Backend, cfg config.HealthCheckConfig) {
	backend.RecordSuccess()
	threshold := cfg.HealthyThreshold
	if threshold < 1 {
		threshold = 1
	}
	if backend.GetSuccesses() >= threshold && !backend.IsHealthy() {
		backend.SetHealthy(true)
		logging.Info("Backend marked healthy", map[string]interface{}{
			"url": backend.URL,
		})
	}
}

func (h *HealthChecker) markUnhealthy(backend *balancer.Backend, cfg config.HealthCheckConfig) {
	backend.RecordFailure()
	threshold := cfg.UnhealthyThreshold
	if threshold < 1 {
		threshold = 1
	}
	// During the startup grace period, failures do not eject a backend. This
	// lets slow-starting backends come up without being immediately marked
	// unhealthy; successes may still promote them.
	if cfg.StartupGracePeriod > 0 && time.Since(h.startedAt) < cfg.StartupGracePeriod {
		return
	}
	if backend.GetFailures() >= threshold && backend.IsHealthy() {
		backend.SetHealthy(false)
		logging.Warn("Backend marked unhealthy", map[string]interface{}{
			"url": backend.URL,
		})
	}
}
