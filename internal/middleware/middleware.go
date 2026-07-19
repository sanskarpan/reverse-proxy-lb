package middleware

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"reverse-proxy-lb/internal/config"
	"reverse-proxy-lb/internal/limiter"
	"reverse-proxy-lb/internal/logging"
	"reverse-proxy-lb/internal/metrics"
	"reverse-proxy-lb/internal/netutil"
)

func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		logging.Debug("Request received", map[string]interface{}{
			"method": r.Method,
			"url":    r.URL.String(),
			"ip":     r.RemoteAddr,
		})
		next.ServeHTTP(w, r)
		logging.Debug("Request completed", map[string]interface{}{
			"method":   r.Method,
			"url":      r.URL.String(),
			"duration": time.Since(start),
		})
	})
}

func Metrics(m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.IncrRequest()
			m.IncInFlight()
			defer m.DecInFlight()
			start := time.Now()
			rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
			next.ServeHTTP(rw, r)
			m.RecordResponseTime(time.Since(start))
			// RecordResponseTime already feeds the latency histogram; record the
			// response's status class for the by-class counter.
			m.IncrStatusClass(rw.statusCode)
		})
	}
}

// RuleName is the canonical name under which the rate-limiter rule at index i is
// registered on the limiter. The server integrator MUST register config rules
// with these exact names (see RegisterRules) so the middleware can look them up
// via limiter.AllowRule. Keeping the naming convention in one place avoids the
// middleware and the server drifting apart.
func RuleName(i int) string {
	return "rule:" + strconv.Itoa(i)
}

// RegisterRules registers every config rule on the limiter under RuleName(i).
// The server integrator calls this once, after constructing the limiter with
// limiter.NewRateLimiterWithOptions, so that RateLimit can throttle matched
// routes against their own budgets. It is a no-op when there are no rules.
func RegisterRules(rl *limiter.RateLimiter, cfg config.RateLimiterConfig) {
	for i, rule := range cfg.Rules {
		rl.AddRule(RuleName(i), float64(rule.RPS), rule.Burst)
	}
}

// RateLimit builds the rate-limiting middleware from the rate-limiter config, the
// (already constructed and started) limiter, and the trusted-proxy set used to
// resolve the real client IP.
//
// Behavior, all opt-in and defaulting to the shipped per-IP + global limiting:
//   - Allowlist: a request whose resolved client IP is in cfg.Allowlist (CIDRs or
//     bare IPs) skips limiting entirely.
//   - Keying: cfg.Key selects the limiting key. "ip" (default) keys by client IP;
//     "header:<Name>" keys by that request header, falling back to the client IP
//     when the header is empty.
//   - Rules: cfg.Rules are matched in order (first PathPrefix + optional Method
//     match wins). A matched request is limited against that rule's own limiter;
//     otherwise the default per-key limiter is used. The global limiter is always
//     checked first (inside the limiter itself).
//   - On 429: the Retry-After header is set (from the limiter's suggested wait,
//     rounded up to whole seconds, or cfg.RetryAfterSeconds as a floor) and the
//     configured Message body is written.
//
// When rl is nil the middleware is a no-op passthrough (defensive: the server only
// installs it when rate limiting is enabled).
//
// m, when non-nil, receives an IncrRateLimited() call on every rejected (429)
// request so the rate_limited_total metric tracks throttling. Passing nil disables
// that accounting (kept optional so callers without a metrics sink still work).
//
// New signature (was: RateLimit(cfg config.RateLimiterConfig, rl *limiter.RateLimiter, trusted []*net.IPNet)):
//
//	RateLimit(cfg config.RateLimiterConfig, rl *limiter.RateLimiter, trusted []*net.IPNet, m *metrics.Metrics) func(http.Handler) http.Handler
func RateLimit(cfg config.RateLimiterConfig, rl *limiter.RateLimiter, trusted []*net.IPNet, m *metrics.Metrics) func(http.Handler) http.Handler {
	allowNets := netutil.ParseCIDRs(cfg.Allowlist)

	// Precompute the header name for header keying so the hot path does no
	// string work beyond a map lookup.
	headerName := ""
	if strings.HasPrefix(cfg.Key, "header:") {
		headerName = strings.TrimPrefix(cfg.Key, "header:")
	}

	message := cfg.Message
	if message == "" {
		message = "Rate limit exceeded"
	}
	retryAfterFloor := cfg.RetryAfterSeconds
	if retryAfterFloor <= 0 {
		retryAfterFloor = 1
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rl == nil {
				next.ServeHTTP(w, r)
				return
			}

			ip := netutil.ClientIP(r, trusted)

			// Allowlisted clients bypass limiting entirely.
			if len(allowNets) > 0 && ipInNets(ip, allowNets) {
				next.ServeHTTP(w, r)
				return
			}

			// Resolve the limiting key.
			key := ip
			if headerName != "" {
				if v := r.Header.Get(headerName); v != "" {
					key = v
				}
			}

			// Match a per-route rule (first match wins); an empty result means
			// use the default per-key limiter.
			ruleName := matchRule(cfg.Rules, r)

			var (
				allowed bool
				retry   time.Duration
			)
			if ruleName != "" {
				allowed, retry = rl.AllowRule(ruleName, key)
			} else {
				allowed, retry = rl.AllowKey(key)
			}

			if !allowed {
				if m != nil {
					m.IncrRateLimited()
				}
				w.Header().Set("Retry-After", retryAfterHeader(retry, retryAfterFloor))
				http.Error(w, message, http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// matchRule returns the RuleName of the first rule whose PathPrefix and optional
// Method match the request, or "" if none match. An empty PathPrefix matches any
// path; an empty Method matches any method.
func matchRule(rules []config.RateLimitRule, r *http.Request) string {
	for i, rule := range rules {
		if rule.PathPrefix != "" && !strings.HasPrefix(r.URL.Path, rule.PathPrefix) {
			continue
		}
		if rule.Method != "" && !strings.EqualFold(rule.Method, r.Method) {
			continue
		}
		return RuleName(i)
	}
	return ""
}

// ipInNets reports whether the given IP string is contained in any of nets.
func ipInNets(ip string, nets []*net.IPNet) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// retryAfterHeader renders the Retry-After value in whole seconds. The limiter's
// suggested wait is rounded up to the next second; floor is the configured
// minimum (RetryAfterSeconds) so a sub-second wait still reports at least the
// configured value.
func retryAfterHeader(retry time.Duration, floor int) string {
	secs := floor
	if retry > 0 {
		ceil := int((retry + time.Second - 1) / time.Second)
		if ceil > secs {
			secs = ceil
		}
	}
	if secs < 0 {
		secs = 0
	}
	return fmt.Sprintf("%d", secs)
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Flush and Hijack are forwarded so that this wrapper does not disable streaming/SSE
// or WebSocket upgrades for handlers further down the chain.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := rw.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
}
