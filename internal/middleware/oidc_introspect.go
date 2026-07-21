package middleware

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"reverse-proxy-lb/internal/config"
)

// introspectResult is a cached introspection outcome for a single token (keyed
// by its SHA-256 hex digest to avoid holding raw tokens in memory).
type introspectResult struct {
	active    bool
	expiresAt time.Time // wall-clock time after which this cached entry is stale
}

// lruCache is a fixed-capacity LRU cache implemented with a map + doubly-linked
// list from container/list. All methods are guarded by the embedded mutex so the
// cache is safe for concurrent use.
type lruCache struct {
	mu       sync.Mutex
	cap      int
	items    map[string]*list.Element
	eviction *list.List
}

type lruEntry struct {
	key string
	val introspectResult
}

func newLRUCache(capacity int) *lruCache {
	if capacity <= 0 {
		capacity = 1000
	}
	return &lruCache{
		cap:      capacity,
		items:    make(map[string]*list.Element, capacity),
		eviction: list.New(),
	}
}

// get retrieves a cached result. ok is false when the key is absent or the entry
// has expired (in which case the entry is lazily removed from the cache).
func (c *lruCache) get(key string, now time.Time) (introspectResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return introspectResult{}, false
	}
	entry := el.Value.(*lruEntry)
	if now.After(entry.val.expiresAt) {
		// Expired: evict lazily.
		c.eviction.Remove(el)
		delete(c.items, key)
		return introspectResult{}, false
	}
	// Move to front (most-recently-used).
	c.eviction.MoveToFront(el)
	return entry.val, true
}

// set inserts or updates a key with the given result. When the cache is at
// capacity the least-recently-used entry is evicted first.
func (c *lruCache) set(key string, val introspectResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.eviction.MoveToFront(el)
		el.Value.(*lruEntry).val = val
		return
	}
	if c.eviction.Len() >= c.cap {
		// Evict the least-recently-used (tail) entry.
		tail := c.eviction.Back()
		if tail != nil {
			c.eviction.Remove(tail)
			delete(c.items, tail.Value.(*lruEntry).key)
		}
	}
	el := c.eviction.PushFront(&lruEntry{key: key, val: val})
	c.items[key] = el
}

// tokenKey returns the SHA-256 hex digest of token, used as the cache key so
// raw tokens are never stored in memory.
func tokenKey(token string) string {
	h := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", h)
}

// introspectionResponse is the RFC 7662 JSON response shape.
type introspectionResponse struct {
	Active bool   `json:"active"`
	Scope  string `json:"scope"`
	Exp    int64  `json:"exp"`
	Sub    string `json:"sub"`
}

// oidcIntrospector holds all state needed by the OIDCIntrospect middleware.
type oidcIntrospector struct {
	cfg        config.OIDCIntrospectionConfig
	cache      *lruCache
	httpClient *http.Client
	now        func() time.Time
}

// OIDCIntrospect returns middleware that validates Bearer tokens via RFC 7662
// token introspection. It fails closed on network errors (503) and rejects
// inactive or expired tokens with 401.
//
// The middleware maintains an LRU cache of validated tokens (keyed by the
// SHA-256 of the raw token) with a configurable TTL for positive results and
// a shorter TTL (CacheTTL/10, minimum 3 s) for negative results.
func OIDCIntrospect(cfg config.OIDCIntrospectionConfig) func(http.Handler) http.Handler {
	if !cfg.Enabled {
		return passthrough
	}
	insp := &oidcIntrospector{
		cfg:        cfg,
		cache:      newLRUCache(cfg.CacheSize),
		httpClient: &http.Client{Timeout: 10 * time.Second},
		now:        time.Now,
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			active, err := insp.validate(r.Context(), token)
			if err != nil {
				// Network / server error: fail closed.
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
				return
			}
			if !active {
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// validate returns (true, nil) when the token is active and satisfies all
// configured scope requirements; (false, nil) when the token is inactive/expired/
// scope-mismatched; and (false, non-nil) when the introspection endpoint cannot
// be reached (caller should return 503).
func (o *oidcIntrospector) validate(ctx context.Context, token string) (bool, error) {
	now := o.now()
	key := tokenKey(token)

	// Cache hit.
	if res, ok := o.cache.get(key, now); ok {
		return res.active, nil
	}

	// Cache miss: call the introspection endpoint.
	resp, err := o.introspect(ctx, token)
	if err != nil {
		return false, err
	}

	// Determine token validity.
	active := resp.Active
	if active && resp.Exp != 0 && now.Unix() >= resp.Exp {
		active = false
	}
	if active && len(o.cfg.ScopesRequired) > 0 {
		if !hasAllScopes(resp.Scope, o.cfg.ScopesRequired) {
			active = false
		}
	}

	// Cache the result.
	var ttl time.Duration
	if active {
		ttl = o.cfg.CacheTTL
		// Cap to the remaining time until expiry so we never serve a stale
		// positive result past the token's actual exp.
		if resp.Exp != 0 {
			remaining := time.Duration(resp.Exp-now.Unix()) * time.Second
			if remaining < ttl {
				ttl = remaining
			}
		}
	} else {
		// Negative results are cached for a shorter duration (CacheTTL/10,
		// minimum 3 s) so revocations propagate quickly.
		ttl = o.cfg.CacheTTL / 10
		if ttl < 3*time.Second {
			ttl = 3 * time.Second
		}
	}
	if ttl > 0 {
		o.cache.set(key, introspectResult{
			active:    active,
			expiresAt: now.Add(ttl),
		})
	}

	return active, nil
}

// introspect POSTs to the configured IntrospectionURL with HTTP Basic auth and
// returns the parsed RFC 7662 response. A non-nil error signals a network or
// protocol failure; a nil error with an inactive response is still a valid (but
// rejected) introspection outcome. The provided context is forwarded so that a
// client disconnect cancels the outbound call rather than letting it run until
// the http.Client timeout fires.
func (o *oidcIntrospector) introspect(ctx context.Context, token string) (*introspectionResponse, error) {
	body := url.Values{
		"token":           {token},
		"token_type_hint": {"access_token"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.cfg.IntrospectionURL, strings.NewReader(body.Encode()))
	if err != nil {
		return nil, fmt.Errorf("oidc_introspect: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if o.cfg.ClientID != "" || o.cfg.ClientSecret != "" {
		req.SetBasicAuth(o.cfg.ClientID, o.cfg.ClientSecret)
	}

	resp, err := o.httpClient.Do(req) // #nosec G107 -- URL is operator-supplied config
	if err != nil {
		return nil, fmt.Errorf("oidc_introspect: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc_introspect: unexpected status %d from introspection endpoint", resp.StatusCode)
	}

	var result introspectionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("oidc_introspect: decode response: %w", err)
	}
	return &result, nil
}

// hasAllScopes reports whether scopeStr (a space-separated list of scopes)
// contains every scope in required.
func hasAllScopes(scopeStr string, required []string) bool {
	granted := make(map[string]struct{})
	for _, s := range strings.Fields(scopeStr) {
		granted[s] = struct{}{}
	}
	for _, r := range required {
		if _, ok := granted[r]; !ok {
			return false
		}
	}
	return true
}
