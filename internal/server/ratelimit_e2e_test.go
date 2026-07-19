package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reverse-proxy-lb/internal/config"
	"testing"
)

// This file drives the §4 rate-limiting feature end-to-end through the real
// server stack. Each scenario stands up a single always-200 httptest backend,
// builds an in-memory *config.Config with rate limiting enabled, constructs the
// Server via New(cfg, ""), and fires real HTTP requests through the server's
// fully assembled handler (Server.Handler(), i.e. the entire middleware chain
// including RateLimit, in front of the proxy).
//
// Requests set RemoteAddr to a loopback peer (trusted in rlBaseConfig) and vary
// X-Forwarded-For to control the resolved client IP / limiting key, mirroring how
// a real deployment sits behind a trusted proxy. Header-keyed scenarios instead
// vary X-Api-Key. Assertions count 200 vs 429 responses across bursts of rapid
// requests. No assertion is weakened to force a pass.

// rlBackend starts a trivial backend that always returns 200 with a fixed body.
// Rate-limited requests are rejected by the middleware before ever reaching it,
// so its only job is to make non-throttled requests observably succeed.
func rlBackend(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "backend-ok")
	}))
	t.Cleanup(s.Close)
	return s
}

// rlBaseConfig returns a minimal valid config wired to one backend with rate
// limiting enabled and every other optional subsystem off. Loopback is trusted so
// tests can steer the resolved client IP via X-Forwarded-For.
func rlBaseConfig(backendURL string, rl config.RateLimiterConfig) *config.Config {
	rl.Enabled = true
	return &config.Config{
		Server: config.ServerConfig{
			Host:           "127.0.0.1",
			Port:           8080,
			TrustedProxies: []string{"127.0.0.1/8", "::1/128"},
		},
		Backends: []config.BackendConfig{
			{URL: backendURL, Weight: 1, MaxConns: 100},
		},
		LoadBalancer: config.LoadBalancerConfig{
			Algorithm: "round_robin",
		},
		RateLimiter: rl,
		Logging:     config.LoggingConfig{Level: "error", Format: "text"},
	}
}

// rlDo issues one request through handler. xff (X-Forwarded-For) steers the
// resolved client IP; apiKey, when non-empty, sets X-Api-Key. It returns the
// status code and the full response (headers/body already read into a recorder).
func rlDo(handler http.Handler, method, target, xff, apiKey string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	// httptest.NewRequest defaults RemoteAddr to a non-loopback address, which
	// would cause the trusted-proxy logic to ignore X-Forwarded-For. Force a
	// loopback peer so XFF is honored as the client IP.
	req.RemoteAddr = "127.0.0.1:34567"
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	if apiKey != "" {
		req.Header.Set("X-Api-Key", apiKey)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// -----------------------------------------------------------------------------
// Global vs per-key independence
// -----------------------------------------------------------------------------

// TestE2E_RateLimit_GlobalVsPerKeyIndependence verifies two things at once:
//   - Per-key isolation: distinct client keys have distinct buckets, so each new
//     key gets to spend its own per-key allowance rather than sharing one.
//   - The global cap still bounds AGGREGATE throughput across all keys: even
//     though each key has a generous per-key budget, once the shared global
//     bucket is drained every further request (regardless of key) is 429'd.
func TestE2E_RateLimit_GlobalVsPerKeyIndependence(t *testing.T) {
	be := rlBackend(t)

	// Per-key budget is generous (10) so per-key limits never bite here; the
	// global bucket is the binding constraint at 5 tokens with no refill in the
	// test window (1 rps).
	const globalCap = 5
	cfg := rlBaseConfig(be.URL, config.RateLimiterConfig{
		RequestsPerSecond: 10,
		Burst:             10,
		GlobalRPS:         1,
		GlobalBurst:       globalCap,
		Key:               "ip",
		Message:           "Rate limit exceeded",
		RetryAfterSeconds: 1,
	})
	h := New(cfg, "").Handler()

	// Fire a rapid burst from MANY distinct client IPs (one request each). Per-key
	// isolation means none of these is denied by its own bucket, so the ONLY thing
	// that can produce a 429 is the shared global bucket draining. Exactly
	// globalCap requests should pass; the rest are throttled globally.
	const total = 40
	ok, throttled := 0, 0
	for i := 0; i < total; i++ {
		ip := fmt.Sprintf("10.1.%d.%d", i/256, i%256)
		rec := rlDo(h, http.MethodGet, "http://proxy.test/", ip, "")
		switch rec.Code {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			throttled++
		default:
			t.Fatalf("distinct-IP request %d: unexpected status %d", i, rec.Code)
		}
	}

	// The global bucket admits exactly its burst before refill; the test runs far
	// faster than 1 token/sec, so we expect precisely globalCap successes.
	if ok != globalCap {
		t.Errorf("global cap: %d/%d requests from distinct keys passed, want exactly %d (aggregate must be bounded by the global bucket)", ok, total, globalCap)
	}
	if throttled != total-globalCap {
		t.Errorf("global cap: %d requests throttled, want %d", throttled, total-globalCap)
	}
	if ok == total {
		t.Fatalf("global cap did not bound aggregate throughput: all %d requests passed despite a global burst of %d", total, globalCap)
	}
}

// TestE2E_RateLimit_PerKeyBucketsAreIndependent verifies that with a GENEROUS
// global limiter, each distinct key gets its own per-key bucket: exhausting one
// key's bucket does not throttle a different key.
func TestE2E_RateLimit_PerKeyBucketsAreIndependent(t *testing.T) {
	be := rlBackend(t)

	// Global is effectively unbounded for the test window; per-key burst is 1 so
	// each key gets exactly one request before being throttled.
	cfg := rlBaseConfig(be.URL, config.RateLimiterConfig{
		RequestsPerSecond: 1,
		Burst:             1,
		GlobalRPS:         10000,
		GlobalBurst:       10000,
		Key:               "ip",
		Message:           "Rate limit exceeded",
		RetryAfterSeconds: 1,
	})
	h := New(cfg, "").Handler()

	const keyA = "203.0.113.10"
	const keyB = "203.0.113.20"

	// Key A: first request passes, second is denied (its single token is spent).
	if rec := rlDo(h, http.MethodGet, "http://proxy.test/", keyA, ""); rec.Code != http.StatusOK {
		t.Fatalf("key A first request: got %d, want 200", rec.Code)
	}
	if rec := rlDo(h, http.MethodGet, "http://proxy.test/", keyA, ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("key A second request: got %d, want 429 (per-key bucket should be drained)", rec.Code)
	}

	// Key B is a different bucket: its first request must still pass despite key A
	// being throttled.
	if rec := rlDo(h, http.MethodGet, "http://proxy.test/", keyB, ""); rec.Code != http.StatusOK {
		t.Fatalf("key B first request: got %d, want 200 (buckets must be independent per key)", rec.Code)
	}
}

// -----------------------------------------------------------------------------
// Header keying
// -----------------------------------------------------------------------------

// TestE2E_RateLimit_HeaderKeying verifies keying by X-Api-Key: two requests with
// different keys are limited independently, while two requests with the same key
// share a bucket. Same source IP throughout, so only the header can distinguish
// the buckets.
func TestE2E_RateLimit_HeaderKeying(t *testing.T) {
	be := rlBackend(t)

	// Per-key burst 1; global generous so it never masks per-key isolation.
	cfg := rlBaseConfig(be.URL, config.RateLimiterConfig{
		RequestsPerSecond: 1,
		Burst:             1,
		GlobalRPS:         10000,
		GlobalBurst:       10000,
		Key:               "header:X-Api-Key",
		Message:           "Rate limit exceeded",
		RetryAfterSeconds: 1,
	})
	h := New(cfg, "").Handler()

	// Same client IP for every request; the ONLY differentiator is X-Api-Key.
	const clientIP = "198.51.100.5"

	// Two different keys are independent: both first requests pass.
	if rec := rlDo(h, http.MethodGet, "http://proxy.test/", clientIP, "alice"); rec.Code != http.StatusOK {
		t.Fatalf("alice first request: got %d, want 200", rec.Code)
	}
	if rec := rlDo(h, http.MethodGet, "http://proxy.test/", clientIP, "bob"); rec.Code != http.StatusOK {
		t.Fatalf("bob first request: got %d, want 200 (distinct header keys must be isolated)", rec.Code)
	}

	// Same key shares a bucket: alice's second request is throttled.
	if rec := rlDo(h, http.MethodGet, "http://proxy.test/", clientIP, "alice"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("alice second request: got %d, want 429 (same key shares one bucket)", rec.Code)
	}
	// bob's own bucket is unaffected by alice being throttled — but bob has also
	// spent his single token, so his second request is throttled too. This
	// confirms the buckets are per-key, not global.
	if rec := rlDo(h, http.MethodGet, "http://proxy.test/", clientIP, "bob"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("bob second request: got %d, want 429 (bob's own bucket is now drained)", rec.Code)
	}
	// A brand-new key still has a full bucket, proving isolation is per-value.
	if rec := rlDo(h, http.MethodGet, "http://proxy.test/", clientIP, "carol"); rec.Code != http.StatusOK {
		t.Fatalf("carol first request: got %d, want 200 (a fresh key must have its own bucket)", rec.Code)
	}
}

// -----------------------------------------------------------------------------
// Per-route rule: /api stricter than /
// -----------------------------------------------------------------------------

// TestE2E_RateLimit_PerRouteRule verifies a route rule throttles /api/* on its
// own strict budget while requests to / are unaffected by that throttling.
func TestE2E_RateLimit_PerRouteRule(t *testing.T) {
	be := rlBackend(t)

	// Generous default (100) so the default path ("/") never denies here; the /api
	// rule is strict (burst 2). Global is generous so it is not the constraint.
	cfg := rlBaseConfig(be.URL, config.RateLimiterConfig{
		RequestsPerSecond: 100,
		Burst:             100,
		GlobalRPS:         10000,
		GlobalBurst:       10000,
		Key:               "ip",
		Message:           "Rate limit exceeded",
		RetryAfterSeconds: 1,
		Rules: []config.RateLimitRule{
			{PathPrefix: "/api", RPS: 1, Burst: 2},
		},
	})
	h := New(cfg, "").Handler()

	// All from the same client IP so the /api rule's per-key bucket is the only
	// constraint that can bite.
	const clientIP = "192.0.2.55"

	// A rapid burst to "/" uses the generous default: every request passes,
	// demonstrating that / is NOT affected by the /api throttle.
	const rootBurst = 20
	for i := 0; i < rootBurst; i++ {
		rec := rlDo(h, http.MethodGet, "http://proxy.test/", clientIP, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("root path request %d: got %d, want 200 (/ must be unaffected by the /api rule)", i, rec.Code)
		}
	}

	// A rapid burst to /api/* is bound by the strict rule (burst 2, ~1 rps): only
	// the first couple pass, the rest are throttled.
	const apiBurst = 20
	apiOK, apiThrottled := 0, 0
	for i := 0; i < apiBurst; i++ {
		rec := rlDo(h, http.MethodGet, "http://proxy.test/api/users", clientIP, "")
		switch rec.Code {
		case http.StatusOK:
			apiOK++
		case http.StatusTooManyRequests:
			apiThrottled++
		default:
			t.Fatalf("/api request %d: unexpected status %d", i, rec.Code)
		}
	}

	// The rule's burst is 2, so at most a small handful pass; the vast majority
	// must be throttled. Tolerate the burst allowance but require real throttling.
	if apiOK > 4 {
		t.Errorf("/api rule: %d/%d requests passed, want <= ~burst (2); the /api route is not being throttled stricter than /", apiOK, apiBurst)
	}
	if apiThrottled == 0 {
		t.Fatalf("/api rule: no requests throttled; the stricter /api budget is not being applied")
	}

	// Sanity: even AFTER /api is throttled, / still serves (independent budgets).
	if rec := rlDo(h, http.MethodGet, "http://proxy.test/", clientIP, ""); rec.Code != http.StatusOK {
		t.Fatalf("root path after /api throttling: got %d, want 200 (/ budget is independent)", rec.Code)
	}
}

// -----------------------------------------------------------------------------
// Allowlist: allowlisted client IP is never 429'd
// -----------------------------------------------------------------------------

// TestE2E_RateLimit_AllowlistNeverThrottled verifies an allowlisted client IP is
// never 429'd even when it far exceeds the limit, while a non-allowlisted IP under
// the same tiny limit IS throttled (proving the limit is genuinely in force).
func TestE2E_RateLimit_AllowlistNeverThrottled(t *testing.T) {
	be := rlBackend(t)

	const allowlisted = "203.0.113.99"
	// rps/burst of 1 so a second request from any non-allowlisted key is denied.
	cfg := rlBaseConfig(be.URL, config.RateLimiterConfig{
		RequestsPerSecond: 1,
		Burst:             1,
		GlobalRPS:         1,
		GlobalBurst:       1,
		Key:               "ip",
		Message:           "Rate limit exceeded",
		RetryAfterSeconds: 1,
		Allowlist:         []string{allowlisted},
	})
	h := New(cfg, "").Handler()

	// The allowlisted IP blasts far past the limit: every request must be 200.
	const burst = 50
	for i := 0; i < burst; i++ {
		rec := rlDo(h, http.MethodGet, "http://proxy.test/", allowlisted, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("allowlisted IP request %d: got %d, want 200 (allowlisted clients must never be throttled)", i, rec.Code)
		}
	}

	// A non-allowlisted IP under the identical config IS throttled after its single
	// token, proving the limit is real and the allowlist is what exempts the other.
	const otherIP = "198.51.100.77"
	if rec := rlDo(h, http.MethodGet, "http://proxy.test/", otherIP, ""); rec.Code != http.StatusOK {
		t.Fatalf("non-allowlisted first request: got %d, want 200", rec.Code)
	}
	if rec := rlDo(h, http.MethodGet, "http://proxy.test/", otherIP, ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("non-allowlisted second request: got %d, want 429 (the limit must actually be enforced for non-allowlisted clients)", rec.Code)
	}
}

// -----------------------------------------------------------------------------
// 429 response: Retry-After header + configured body
// -----------------------------------------------------------------------------

// TestE2E_RateLimit_429CarriesRetryAfterAndBody verifies a throttled request
// gets a 429 whose Retry-After header and body match the configuration.
func TestE2E_RateLimit_429CarriesRetryAfterAndBody(t *testing.T) {
	be := rlBackend(t)

	const wantBody = "please slow down"
	const wantRetryAfter = "7"
	cfg := rlBaseConfig(be.URL, config.RateLimiterConfig{
		RequestsPerSecond: 1,
		Burst:             1,
		GlobalRPS:         1,
		GlobalBurst:       1,
		Key:               "ip",
		Message:           wantBody,
		RetryAfterSeconds: 7,
	})
	h := New(cfg, "").Handler()

	const clientIP = "192.0.2.200"

	// Spend the single token.
	if rec := rlDo(h, http.MethodGet, "http://proxy.test/", clientIP, ""); rec.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", rec.Code)
	}
	// This one is throttled and must carry the configured Retry-After and body.
	rec := rlDo(h, http.MethodGet, "http://proxy.test/", clientIP, "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled request: got %d, want 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra != wantRetryAfter {
		t.Errorf("429 Retry-After = %q, want %q (the configured RetryAfterSeconds floor)", ra, wantRetryAfter)
	}
	// http.Error appends a trailing newline to the body.
	if body := rec.Body.String(); body != wantBody+"\n" {
		t.Errorf("429 body = %q, want %q (the configured Message)", body, wantBody+"\n")
	}
}
