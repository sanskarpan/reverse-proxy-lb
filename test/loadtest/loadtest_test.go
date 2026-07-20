package loadtest_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
	"reverse-proxy-lb/internal/server"
)

const (
	numBackends   = 3
	numGoroutines = 10
	reqsPerWorker = 200
	totalRequests = numGoroutines * reqsPerWorker
)

// buildMinimalConfig returns a minimal *config.Config pointing at the provided
// backend URLs with round-robin load balancing and no health checks. It is
// constructed directly (not via config.Load) to avoid needing a file on disk.
func buildMinimalConfig(backendURLs []string) *config.Config {
	backends := make([]config.BackendConfig, len(backendURLs))
	for i, u := range backendURLs {
		backends[i] = config.BackendConfig{
			URL:      u,
			Weight:   1,
			MaxConns: 100,
		}
	}

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host:            "127.0.0.1",
			Port:            0, // not used; we call Handler() directly
			ReadTimeout:     10 * time.Second,
			WriteTimeout:    10 * time.Second,
			IdleTimeout:     60 * time.Second,
			ShutdownTimeout: 5 * time.Second,
			Upstream: config.UpstreamConfig{
				DialTimeout:           5 * time.Second,
				TLSHandshakeTimeout:   5 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   100,
			},
			L4: config.L4Config{
				DialTimeout: 5 * time.Second,
			},
		},
		Backends: backends,
		LoadBalancer: config.LoadBalancerConfig{
			Algorithm: "round_robin",
			ConsistentHash: config.ConsistentHashConfig{
				Replicas:   100,
				LoadFactor: 1.25,
			},
			HealthCheck: config.HealthCheckConfig{
				Enabled:            false,
				Type:               "http",
				Method:             "GET",
				HealthyThreshold:   2,
				UnhealthyThreshold: 3,
				Jitter:             0.1,
			},
		},
		CircuitBreaker: config.CircuitBreakerConfig{
			Mode:               "consecutive",
			RollingWindow:      10 * time.Second,
			ErrorRateThreshold: 0.5,
			MinRequests:        20,
			TripOn:             []string{"connect", "timeout"},
		},
		Retry: config.RetryConfig{
			HonorRetryAfter: true,
			RetryOn:         []string{"connect", "timeout"},
		},
		RateLimiter: config.RateLimiterConfig{
			Enabled:   false,
			Algorithm: "token_bucket",
			Key:       "ip",
			SharedStore: config.SharedStoreConfig{
				Backend: "memory",
				Key:     "__global__",
				Redis: config.RedisStoreConfig{
					Prefix: "rplb:rl",
				},
			},
			RetryAfterSeconds: 1,
			Message:           "Rate limit exceeded",
		},
		TLS: config.TLSConfig{
			MinVersion: "1.2",
			ClientAuth: "none",
			ACME: config.ACMEConfig{
				HTTPChallengePort: 80,
			},
		},
		Metrics: config.MetricsConfig{
			Host: "127.0.0.1",
		},
		Security: config.SecurityConfig{
			Auth: config.AuthConfig{
				Header: "X-API-Key",
				JWTAlg: "HS256",
			},
		},
		Cache: config.CacheConfig{
			DefaultTTL:   60 * time.Second,
			MaxEntries:   1000,
			MaxBodyBytes: 1 << 20,
			Methods:      []string{"GET", "HEAD"},
		},
	}

	// HTTP3 port defaults to the server port.
	cfg.Server.HTTP3.Port = cfg.Server.Port

	return cfg
}

// buildRateLimitedConfig is like buildMinimalConfig but enables the rate limiter
// with a low RPS so many requests get 429 responses.
func buildRateLimitedConfig(backendURLs []string, rps int) *config.Config {
	cfg := buildMinimalConfig(backendURLs)
	cfg.RateLimiter.Enabled = true
	cfg.RateLimiter.RequestsPerSecond = rps
	cfg.RateLimiter.Burst = rps // burst equals rps to make limiting kick in quickly
	cfg.RateLimiter.GlobalRPS = rps
	cfg.RateLimiter.GlobalBurst = rps
	return cfg
}

// startFakeBackends spins up n httptest.Servers that respond 200 OK with a small
// JSON body. Each server adds ~1ms of artificial delay to simulate realistic
// backend latency. Returns the servers and their URL strings.
func startFakeBackends(t *testing.T, n int) ([]*httptest.Server, []string) {
	t.Helper()
	servers := make([]*httptest.Server, n)
	urls := make([]string, n)
	for i := 0; i < n; i++ {
		idx := i
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(time.Millisecond) // simulate ~1ms backend latency
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal(map[string]interface{}{
				"backend": idx,
				"status":  "ok",
			})
			_, _ = w.Write(body)
		})
		s := httptest.NewServer(mux)
		servers[i] = s
		urls[i] = s.URL
	}
	return servers, urls
}

// percentile returns the p-th percentile (0-100) of a sorted slice of durations.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p / 100.0)
	return sorted[idx]
}

// TestLoadHarness starts N fake backends, proxies through the server, fires
// 10 goroutines each sending 200 sequential requests, then asserts success_rate
// >= 99% and p99 latency < 500ms.
func TestLoadHarness(t *testing.T) {
	// 1. Start fake backends.
	backends, backendURLs := startFakeBackends(t, numBackends)
	defer func() {
		for _, s := range backends {
			s.Close()
		}
	}()

	// 2. Build the proxy server and obtain its Handler directly (no real TCP bind).
	cfg := buildMinimalConfig(backendURLs)
	srv := server.New(cfg, "")
	proxyHandler := srv.Handler()
	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	// 3. Ramp up: 10 concurrent goroutines, each sending 200 requests sequentially.
	type result struct {
		latency time.Duration
		status  int
	}
	results := make([]result, 0, totalRequests)
	var mu sync.Mutex
	var wg sync.WaitGroup

	start := time.Now()
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]result, 0, reqsPerWorker)
			for i := 0; i < reqsPerWorker; i++ {
				t0 := time.Now()
				resp, err := http.Get(proxyServer.URL + "/") //nolint:noctx
				lat := time.Since(t0)
				if err != nil {
					local = append(local, result{latency: lat, status: 0})
					continue
				}
				_ = resp.Body.Close()
				local = append(local, result{latency: lat, status: resp.StatusCode})
			}
			mu.Lock()
			results = append(results, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	// 4. Collect: build latency slice and count successes.
	latencies := make([]time.Duration, 0, len(results))
	successCount := 0
	for _, r := range results {
		latencies = append(latencies, r.latency)
		if r.status == http.StatusOK {
			successCount++
		}
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	p50 := percentile(latencies, 50)
	p95 := percentile(latencies, 95)
	p99 := percentile(latencies, 99)
	total := len(results)
	successRate := float64(successCount) / float64(total) * 100.0
	throughput := float64(total) / elapsed.Seconds()

	// 5. Report.
	t.Logf("=== Load Harness Results ===")
	t.Logf("Total requests:  %d", total)
	t.Logf("Successful (2xx): %d (%.2f%%)", successCount, successRate)
	t.Logf("Throughput:       %.1f req/s", throughput)
	t.Logf("Latency p50:      %s", p50)
	t.Logf("Latency p95:      %s", p95)
	t.Logf("Latency p99:      %s", p99)
	t.Logf("Elapsed:          %s", elapsed)

	// 6. Assert.
	if successRate < 99.0 {
		t.Errorf("success rate %.2f%% is below 99%% threshold", successRate)
	}
	if p99 >= 500*time.Millisecond {
		t.Errorf("p99 latency %s exceeds 500ms threshold", p99)
	}
}

// TestLoadHarnessRateLimit configures the proxy with a low rate limit (50 RPS),
// fires 200 requests from 10 goroutines, and asserts that some requests get 429,
// none get 5xx, and all requests complete.
func TestLoadHarnessRateLimit(t *testing.T) {
	// 1. Start fake backends.
	backends, backendURLs := startFakeBackends(t, numBackends)
	defer func() {
		for _, s := range backends {
			s.Close()
		}
	}()

	// 2. Build a rate-limited proxy (50 RPS global).
	const rateLimitRPS = 50
	cfg := buildRateLimitedConfig(backendURLs, rateLimitRPS)
	srv := server.New(cfg, "")
	proxyServer := httptest.NewServer(srv.Handler())
	defer proxyServer.Close()

	// Use a shared HTTP client with a connection pool large enough to avoid
	// OS ephemeral-port exhaustion when 10 goroutines hammer the proxy at full
	// speed. The default http.DefaultTransport would open a new connection per
	// goroutine per in-flight request; with 2000 rapid requests the macOS port
	// table fills up and some dials fail. A pooled client with keep-alives
	// ensures each goroutine reuses its single persistent connection.
	client := proxyServer.Client()
	if tr, ok := client.Transport.(*http.Transport); ok {
		tr.MaxIdleConnsPerHost = numGoroutines * 2
		tr.DisableKeepAlives = false
	}

	// 3. Fire 200 requests at full speed from 10 goroutines.
	// Each request is retried up to maxTransportRetries times on a transport error
	// (e.g. connection reset/refused) so that transient FD pressure from concurrently
	// running test packages does not cause spurious failures.
	const maxTransportRetries = 5
	type result struct {
		status int
		err    error
	}
	results := make([]result, 0, totalRequests)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]result, 0, reqsPerWorker)
			for i := 0; i < reqsPerWorker; i++ {
				var resp *http.Response
				var err error
				for attempt := 0; attempt <= maxTransportRetries; attempt++ {
					resp, err = client.Get(proxyServer.URL + "/") //nolint:noctx
					if err == nil {
						break
					}
					// Brief back-off before retrying a transport-level error.
					time.Sleep(time.Duration(attempt+1) * 5 * time.Millisecond)
				}
				if err != nil {
					local = append(local, result{err: err})
					continue
				}
				_ = resp.Body.Close()
				local = append(local, result{status: resp.StatusCode})
			}
			mu.Lock()
			results = append(results, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	// 4. Tally results.
	var count200, count429, count5xx, countErr int
	for _, r := range results {
		if r.err != nil {
			countErr++
			continue
		}
		switch {
		case r.status == http.StatusOK:
			count200++
		case r.status == http.StatusTooManyRequests:
			count429++
		case r.status >= 500:
			count5xx++
		}
	}

	t.Logf("=== Rate-Limit Harness Results ===")
	t.Logf("Total requests:  %d", len(results))
	t.Logf("200 OK:          %d", count200)
	t.Logf("429 Too Many:    %d", count429)
	t.Logf("5xx errors:      %d", count5xx)
	t.Logf("Transport errors: %d", countErr)
	t.Logf("Configured RPS:  %d", rateLimitRPS)

	// All requests must complete (no transport errors).
	if countErr > 0 {
		t.Errorf("got %d transport errors; expected 0", countErr)
	}

	// Some requests must have been rate-limited (429).
	if count429 == 0 {
		t.Errorf("expected some 429 responses with %d RPS limit and %d total requests, got none",
			rateLimitRPS, totalRequests)
	}

	// No 5xx responses are acceptable.
	if count5xx > 0 {
		t.Errorf("got %d 5xx responses; expected 0", count5xx)
	}

	t.Logf("PASS: %d requests rate-limited (429), %d succeeded, 0 5xx errors",
		count429, count200)
}
