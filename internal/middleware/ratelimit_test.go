package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"reverse-proxy-lb/internal/config"
	"reverse-proxy-lb/internal/limiter"
)

// okHandler is a trivial downstream handler that records whether it was reached.
func okHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	})
}

func do(h http.Handler, method, target, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestRateLimit_AllowlistBypasses verifies an allowlisted IP is never throttled
// even when the underlying limiter would otherwise deny.
func TestRateLimit_AllowlistBypasses(t *testing.T) {
	// rps/burst of 1 so the second request would normally be denied.
	rl := limiter.NewRateLimiter(1, 1)
	cfg := config.RateLimiterConfig{
		Enabled:           true,
		RequestsPerSecond: 1,
		Burst:             1,
		Key:               "ip",
		Message:           "Rate limit exceeded",
		RetryAfterSeconds: 1,
		Allowlist:         []string{"203.0.113.7"},
	}
	mw := RateLimit(cfg, rl, nil, nil)
	reached := false
	h := mw(okHandler(&reached))

	for i := 0; i < 5; i++ {
		reached = false
		rec := do(h, http.MethodGet, "http://x/", "203.0.113.7:1234", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: allowlisted IP got status %d, want 200", i, rec.Code)
		}
		if !reached {
			t.Fatalf("request %d: downstream handler not reached for allowlisted IP", i)
		}
	}
}

// TestRateLimit_HeaderKeyingIsolatesKeys verifies that keying by a header gives
// each distinct header value its own bucket.
func TestRateLimit_HeaderKeyingIsolatesKeys(t *testing.T) {
	// Per-key burst of 1: each API key gets one request before denial. The
	// global limiter is generous so it does not mask per-key isolation.
	rl := limiter.NewRateLimiterWithOptions(limiter.Options{
		PerKeyRPS:   1,
		PerKeyBurst: 1,
		GlobalRPS:   1000,
		GlobalBurst: 1000,
	})
	cfg := config.RateLimiterConfig{
		Enabled:           true,
		RequestsPerSecond: 1,
		Burst:             1,
		Key:               "header:X-Api-Key",
		Message:           "Rate limit exceeded",
		RetryAfterSeconds: 1,
	}
	mw := RateLimit(cfg, rl, nil, nil)
	reached := false
	h := mw(okHandler(&reached))

	// Same source IP, two different API keys — must not share a bucket.
	rec := do(h, http.MethodGet, "http://x/", "9.9.9.9:1", map[string]string{"X-Api-Key": "alice"})
	if rec.Code != http.StatusOK {
		t.Fatalf("alice first request: got %d, want 200", rec.Code)
	}
	rec = do(h, http.MethodGet, "http://x/", "9.9.9.9:2", map[string]string{"X-Api-Key": "bob"})
	if rec.Code != http.StatusOK {
		t.Fatalf("bob first request: got %d, want 200 (keys must be isolated)", rec.Code)
	}
	// alice's second request should now be denied (her single token is spent).
	rec = do(h, http.MethodGet, "http://x/", "9.9.9.9:3", map[string]string{"X-Api-Key": "alice"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("alice second request: got %d, want 429", rec.Code)
	}
}

// TestRateLimit_HeaderKeyingFallsBackToIP verifies a missing header falls back to
// keying by client IP.
func TestRateLimit_HeaderKeyingFallsBackToIP(t *testing.T) {
	rl := limiter.NewRateLimiter(1, 1)
	cfg := config.RateLimiterConfig{
		Enabled:           true,
		RequestsPerSecond: 1,
		Burst:             1,
		Key:               "header:X-Api-Key",
		Message:           "Rate limit exceeded",
		RetryAfterSeconds: 1,
	}
	h := RateLimit(cfg, rl, nil, nil)(okHandler(new(bool)))

	// No header => keyed by IP. Same IP twice: second is denied.
	rec := do(h, http.MethodGet, "http://x/", "5.5.5.5:1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("first no-header request: got %d, want 200", rec.Code)
	}
	rec = do(h, http.MethodGet, "http://x/", "5.5.5.5:2", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second no-header request from same IP: got %d, want 429", rec.Code)
	}
}

// TestRateLimit_RouteRuleThrottlesStricter verifies a route rule limits /api on
// its own (stricter) budget, independent of the generous default per-key bucket.
func TestRateLimit_RouteRuleThrottlesStricter(t *testing.T) {
	// Generous default (100 rps/burst) so the default path never denies here;
	// the /api rule is strict (burst 1).
	rl := limiter.NewRateLimiter(100, 100)
	cfg := config.RateLimiterConfig{
		Enabled:           true,
		RequestsPerSecond: 100,
		Burst:             100,
		Key:               "ip",
		Message:           "Rate limit exceeded",
		RetryAfterSeconds: 1,
		Rules: []config.RateLimitRule{
			{PathPrefix: "/api", RPS: 1, Burst: 1},
		},
	}
	RegisterRules(rl, cfg)
	h := RateLimit(cfg, rl, nil, nil)(okHandler(new(bool)))

	// A non-/api path uses the generous default: many requests all pass.
	for i := 0; i < 10; i++ {
		rec := do(h, http.MethodGet, "http://x/other", "1.2.3.4:1", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("default-path request %d: got %d, want 200", i, rec.Code)
		}
	}

	// The /api rule allows one request then throttles.
	rec := do(h, http.MethodGet, "http://x/api/users", "1.2.3.4:2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("first /api request: got %d, want 200", rec.Code)
	}
	rec = do(h, http.MethodGet, "http://x/api/users", "1.2.3.4:3", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second /api request: got %d, want 429 (rule should throttle)", rec.Code)
	}
}

// TestRateLimit_RuleMethodMatch verifies the optional Method matcher only applies
// the rule to the configured method.
func TestRateLimit_RuleMethodMatch(t *testing.T) {
	rl := limiter.NewRateLimiter(100, 100)
	cfg := config.RateLimiterConfig{
		Enabled:           true,
		RequestsPerSecond: 100,
		Burst:             100,
		Key:               "ip",
		RetryAfterSeconds: 1,
		Rules: []config.RateLimitRule{
			{PathPrefix: "/api", Method: "POST", RPS: 1, Burst: 1},
		},
	}
	RegisterRules(rl, cfg)
	h := RateLimit(cfg, rl, nil, nil)(okHandler(new(bool)))

	// GET /api does not match the POST-only rule: uses the generous default.
	for i := 0; i < 5; i++ {
		rec := do(h, http.MethodGet, "http://x/api", "8.8.8.8:1", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api request %d: got %d, want 200", i, rec.Code)
		}
	}
	// POST /api matches: first OK, second throttled.
	rec := do(h, http.MethodPost, "http://x/api", "8.8.8.8:2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("first POST /api: got %d, want 200", rec.Code)
	}
	rec = do(h, http.MethodPost, "http://x/api", "8.8.8.8:3", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second POST /api: got %d, want 429", rec.Code)
	}
}

// TestRateLimit_429CarriesRetryAfterAndBody verifies the 429 response includes the
// configured Retry-After and the custom body.
func TestRateLimit_429CarriesRetryAfterAndBody(t *testing.T) {
	rl := limiter.NewRateLimiter(1, 1)
	cfg := config.RateLimiterConfig{
		Enabled:           true,
		RequestsPerSecond: 1,
		Burst:             1,
		Key:               "ip",
		Message:           "slow down please",
		RetryAfterSeconds: 7,
	}
	h := RateLimit(cfg, rl, nil, nil)(okHandler(new(bool)))

	// Exhaust the single token.
	if rec := do(h, http.MethodGet, "http://x/", "4.4.4.4:1", nil); rec.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", rec.Code)
	}
	rec := do(h, http.MethodGet, "http://x/", "4.4.4.4:2", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("denied request: got %d, want 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("429 response missing Retry-After header")
	}
	// http.Error appends a newline to the body.
	if body := rec.Body.String(); body != "slow down please\n" {
		t.Fatalf("429 body = %q, want custom message", body)
	}
}

// TestRateLimit_DefaultConfigBehavesLikeBefore verifies that with no allowlist,
// no rules, and default IP keying, behavior matches the shipped per-IP + global
// limiting: one IP is throttled after its burst, a different IP is unaffected by
// the per-key bucket.
func TestRateLimit_DefaultConfigBehavesLikeBefore(t *testing.T) {
	// Global generous, per-key burst 1.
	rl := limiter.NewRateLimiterWithOptions(limiter.Options{
		PerKeyRPS:   1,
		PerKeyBurst: 1,
		GlobalRPS:   1000,
		GlobalBurst: 1000,
	})
	cfg := config.RateLimiterConfig{
		Enabled:           true,
		RequestsPerSecond: 1,
		Burst:             1,
		Key:               "ip",
		Message:           "Rate limit exceeded",
		RetryAfterSeconds: 1,
	}
	h := RateLimit(cfg, rl, nil, nil)(okHandler(new(bool)))

	if rec := do(h, http.MethodGet, "http://x/", "7.7.7.7:1", nil); rec.Code != http.StatusOK {
		t.Fatalf("IP A first request: got %d, want 200", rec.Code)
	}
	if rec := do(h, http.MethodGet, "http://x/", "7.7.7.7:2", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("IP A second request: got %d, want 429", rec.Code)
	}
	// A different IP still has its own token.
	if rec := do(h, http.MethodGet, "http://x/", "7.7.7.8:1", nil); rec.Code != http.StatusOK {
		t.Fatalf("IP B first request: got %d, want 200 (per-key isolation)", rec.Code)
	}
}

// TestRateLimit_NilLimiterPassthrough verifies the middleware is a defensive
// no-op when the limiter is nil.
func TestRateLimit_NilLimiterPassthrough(t *testing.T) {
	cfg := config.RateLimiterConfig{Enabled: true, Key: "ip"}
	reached := false
	h := RateLimit(cfg, nil, nil, nil)(okHandler(&reached))
	rec := do(h, http.MethodGet, "http://x/", "1.1.1.1:1", nil)
	if rec.Code != http.StatusOK || !reached {
		t.Fatalf("nil limiter should pass through: code=%d reached=%v", rec.Code, reached)
	}
}
