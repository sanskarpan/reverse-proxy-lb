package middleware

import (
	"bufio"
	"container/list"
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"reverse-proxy-lb/internal/config"
)

// Cache returns middleware that serves an in-memory HTTP response cache. It is a
// no-op passthrough when cfg.Enabled is false, preserving current behavior.
//
// What is implemented:
//
//   - Only safe methods (cfg.Methods, default GET/HEAD) with a cacheable response
//     are stored. A request carrying Cache-Control: no-store or no-cache bypasses
//     the cache. Responses with Cache-Control: no-store/no-cache/private, a
//     Set-Cookie header, a non-200 status, or a body larger than MaxBodyBytes are
//     never stored (no-cache would require origin revalidation this cache does not
//     perform, so it is not stored rather than served stale).
//   - Authorization (RFC 7234 §3.2): a stored response is neither stored for nor
//     reused to serve a request carrying an Authorization header UNLESS the response
//     is explicitly Cache-Control: public (or s-maxage) — so one authenticated
//     user's response is never served to another.
//   - TTL: the response's Cache-Control: max-age (or s-maxage) sets the freshness
//     lifetime; otherwise cfg.DefaultTTL is used. This is a cache-by-default policy:
//     a backend serving per-request/per-user content MUST send Vary or
//     Cache-Control: private/no-store, per the HTTP caching contract.
//   - Key: method + host + request URI, plus the request values of any Vary
//     headers the response declares. A change in a resource's Vary set purges its
//     previously cached variants to prevent mis-keyed reuse (Vary: * disables it).
//   - On a fresh hit: the stored response is replayed with an Age header and
//     X-Cache: HIT. Conditional revalidation is supported: If-None-Match matching
//     the stored ETag (or If-Modified-Since >= stored Last-Modified) yields 304.
//   - stale-while-revalidate (best-effort): when a response was stored with a
//     Cache-Control: stale-while-revalidate=N directive and the entry is expired
//     but still within that stale window, the stale body is served immediately
//     (X-Cache: STALE) and a single bounded background refresh is triggered. Only
//     one background refresh per key runs at a time.
//   - Bounded and thread-safe: an LRU with a mutex evicts the least-recently-used
//     entry once MaxEntries is exceeded.
//   - Streaming/upgrades: the capture writer forwards Flush and Hijack. A response
//     that is hijacked (WebSocket) or flushed before completion is treated as
//     uncacheable and streamed straight through, so WS/SSE keep working.
func Cache(cfg config.CacheConfig) func(http.Handler) http.Handler {
	if !cfg.Enabled {
		return func(next http.Handler) http.Handler { return next }
	}

	methods := cfg.Methods
	if len(methods) == 0 {
		methods = []string{http.MethodGet, http.MethodHead}
	}
	methodSet := make(map[string]bool, len(methods))
	for _, m := range methods {
		methodSet[strings.ToUpper(strings.TrimSpace(m))] = true
	}

	defaultTTL := cfg.DefaultTTL
	if defaultTTL <= 0 {
		defaultTTL = 60 * time.Second
	}
	maxEntries := cfg.MaxEntries
	if maxEntries <= 0 {
		maxEntries = 1000
	}
	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = 1 << 20
	}

	c := &responseCache{
		methods:    methodSet,
		defaultTTL: defaultTTL,
		maxEntries: maxEntries,
		maxBody:    maxBody,
		entries:    make(map[string]*list.Element),
		lru:        list.New(),
		refreshing: make(map[string]bool),
		varyHints:  make(map[string][]string),
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.serve(w, r, next)
		})
	}
}

// cacheEntry is a stored response plus the metadata needed to serve, expire, and
// revalidate it.
type cacheEntry struct {
	key      string // baseKey + Vary suffix
	baseKey  string // method + host + URI (without Vary), for the Vary hint index
	status   int
	header   http.Header
	body     []byte
	storedAt time.Time
	ttl      time.Duration
	swr      time.Duration // stale-while-revalidate window (0 = none)
	etag     string
	lastMod  string
	// varyNames are the response's declared Vary header names (canonicalized).
	// Empty when the response had no Vary. Recorded so lookups can reconstruct the
	// request-specific store key.
	varyNames []string
	// public is true when the response carried Cache-Control: public (or s-maxage).
	// Per RFC 7234 §3.2 a shared cache must not reuse a stored response for a request
	// with an Authorization header unless the response is explicitly public.
	public bool
}

// fresh reports whether the entry is still within its TTL at time now.
func (e *cacheEntry) fresh(now time.Time) bool {
	return now.Sub(e.storedAt) < e.ttl
}

// serveableStale reports whether an expired entry is still within its
// stale-while-revalidate window at time now.
func (e *cacheEntry) serveableStale(now time.Time) bool {
	if e.swr <= 0 {
		return false
	}
	age := now.Sub(e.storedAt)
	return age >= e.ttl && age < e.ttl+e.swr
}

type responseCache struct {
	methods    map[string]bool
	defaultTTL time.Duration
	maxEntries int
	maxBody    int

	mu         sync.Mutex
	entries    map[string]*list.Element // key -> *list.Element(*cacheEntry)
	lru        *list.List
	refreshing map[string]bool     // keys with an in-flight background refresh
	varyHints  map[string][]string // baseKey -> Vary header names seen for it
}

func (c *responseCache) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	// Non-cacheable methods pass straight through.
	if !c.methods[r.Method] {
		next.ServeHTTP(w, r)
		return
	}
	// A request that forbids the cache (no-store/no-cache) bypasses it entirely.
	if requestBypassesCache(r) {
		next.ServeHTTP(w, r)
		return
	}

	baseKey := requestKey(r)

	// RFC 7234 §3.2: a shared cache MUST NOT reuse a stored response to a request
	// with an Authorization header unless the stored response is explicitly public.
	authed := r.Header.Get("Authorization") != ""

	now := time.Now()
	if entry, hit := c.lookup(baseKey, r); hit && (!authed || entry.public) {
		if entry.fresh(now) {
			c.writeFromCache(w, r, entry, now, "HIT")
			return
		}
		if entry.serveableStale(now) {
			// Serve stale immediately, refresh in the background (bounded to one
			// in-flight refresh per key).
			c.triggerRefresh(entry.key, r, next)
			c.writeFromCache(w, r, entry, now, "STALE")
			return
		}
		// Expired and not serveable-stale: fall through to a fresh fetch, which
		// will overwrite the entry.
	}

	c.fetchAndMaybeStore(w, r, next, baseKey)
}

// fetchAndMaybeStore runs the downstream handler through a capturing writer and,
// if the result is cacheable, stores it under baseKey (extended with Vary).
func (c *responseCache) fetchAndMaybeStore(w http.ResponseWriter, r *http.Request, next http.Handler, baseKey string) {
	cw := &cacheCapture{
		ResponseWriter: w,
		maxBody:        c.maxBody,
		status:         http.StatusOK,
	}
	next.ServeHTTP(cw, r)

	// Do not add cache metadata (Age/X-Cache) to a MISS response; the client sees
	// exactly what the backend sent.

	entry := c.entryFromCapture(r, baseKey, cw)
	if entry == nil {
		return
	}
	c.store(entry)
}

// entryFromCapture builds a cacheEntry from a captured response, or returns nil
// when the response is not cacheable.
func (c *responseCache) entryFromCapture(r *http.Request, baseKey string, cw *cacheCapture) *cacheEntry {
	if cw.hijacked || cw.streamed || cw.overflow {
		return nil
	}
	if cw.status != http.StatusOK {
		return nil
	}
	respCC := parseCacheControl(cw.Header().Get("Cache-Control"))
	// no-store/private: never store. no-cache (response directive): the response may
	// only be reused after successful revalidation with the origin — which this cache
	// does not perform on a fresh hit — so we do not store it, avoiding serving stale
	// content as a fresh HIT.
	if respCC.noStore || respCC.private || respCC.noCache {
		return nil
	}
	if cw.Header().Get("Set-Cookie") != "" {
		return nil
	}
	// Do not store a response to an authenticated request unless it is explicitly
	// public (RFC 7234 §3.2), so one user's response is never cached and served to
	// another. s-maxage also implies shared-cacheability.
	isPublic := respCC.public || (respCC.maxAgeSet && strings.Contains(strings.ToLower(cw.Header().Get("Cache-Control")), "s-maxage"))
	if r.Header.Get("Authorization") != "" && !isPublic {
		return nil
	}
	varyNames := parseVary(cw.Header().Get("Vary"))
	for _, n := range varyNames {
		if n == "*" {
			return nil // Vary: * -> uncacheable
		}
	}

	ttl := c.defaultTTL
	if respCC.maxAgeSet {
		if respCC.maxAge <= 0 {
			return nil // max-age=0 -> effectively do not cache
		}
		ttl = time.Duration(respCC.maxAge) * time.Second
	}

	// Copy the captured body so the caller's buffer can be reused/freed.
	body := make([]byte, len(cw.body))
	copy(body, cw.body)

	return &cacheEntry{
		key:       baseKey + buildVaryKey(varyNames, r),
		baseKey:   baseKey,
		status:    cw.status,
		header:    cloneHeader(cw.Header()),
		body:      body,
		storedAt:  time.Now(),
		ttl:       ttl,
		swr:       time.Duration(respCC.swr) * time.Second,
		etag:      cw.Header().Get("ETag"),
		lastMod:   cw.Header().Get("Last-Modified"),
		varyNames: varyNames,
		public:    isPublic,
	}
}

// writeFromCache replays a stored entry to the client, honoring conditional
// revalidation. cacheState is the X-Cache value ("HIT" or "STALE").
func (c *responseCache) writeFromCache(w http.ResponseWriter, r *http.Request, e *cacheEntry, now time.Time, cacheState string) {
	// Conditional revalidation: a matching validator lets us answer 304 without a
	// body.
	if notModified(r, e) {
		h := w.Header()
		if e.etag != "" {
			h.Set("ETag", e.etag)
		}
		if e.lastMod != "" {
			h.Set("Last-Modified", e.lastMod)
		}
		h.Set("X-Cache", cacheState)
		h.Set("Age", ageSeconds(e, now))
		w.WriteHeader(http.StatusNotModified)
		return
	}

	h := w.Header()
	copyHeader(h, e.header)
	h.Set("Age", ageSeconds(e, now))
	h.Set("X-Cache", cacheState)
	w.WriteHeader(e.status)
	// HEAD responses carry no body; GET replays the stored bytes.
	if r.Method != http.MethodHead {
		_, _ = w.Write(e.body)
	}
}

// triggerRefresh launches at most one background refresh per key. The refresh
// re-runs the handler with a detached (background) request and stores the fresh
// response, replacing the stale entry.
func (c *responseCache) triggerRefresh(key string, r *http.Request, next http.Handler) {
	c.mu.Lock()
	if c.refreshing[key] {
		c.mu.Unlock()
		return
	}
	c.refreshing[key] = true
	c.mu.Unlock()

	// Detach from the client's context so the client returning (and its context
	// being cancelled) does not abort the background refresh. Clone with a fresh
	// background context; it shares no mutable client state.
	rr := r.Clone(context.Background())

	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.refreshing, key)
			c.mu.Unlock()
			// Swallow a panic in the background handler; a failed refresh simply
			// leaves the stale entry to expire.
			_ = recover()
		}()
		cw := &cacheCapture{
			ResponseWriter: newDiscardWriter(),
			maxBody:        c.maxBody,
			status:         http.StatusOK,
		}
		next.ServeHTTP(cw, rr)
		entry := c.entryFromCapture(rr, requestKey(rr), cw)
		if entry != nil {
			c.store(entry)
		}
	}()
}

// lookup returns the entry for r, resolving Vary. It refreshes LRU recency on a
// hit. The returned entry is a live pointer; its immutable fields are safe to
// read outside the lock (body/header are never mutated after store).
func (c *responseCache) lookup(baseKey string, r *http.Request) (*cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// A response that declared a Vary header is stored under baseKey + the request
	// values of those headers. Reconstruct that key from the recorded Vary hint;
	// otherwise fall back to the plain base key (no-Vary responses).
	key := baseKey
	if names, ok := c.varyHints[baseKey]; ok {
		key = baseKey + buildVaryKey(names, r)
	}
	if el, ok := c.entries[key]; ok {
		c.lru.MoveToFront(el)
		return el.Value.(*cacheEntry), true
	}
	return nil, false
}

// store inserts or replaces an entry and enforces the LRU bound.
func (c *responseCache) store(e *cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// If the Vary set for this resource changed (it started/stopped varying, or now
	// varies on different headers), purge every previously cached variant under the
	// same base key. Otherwise a request could be served an entry that was keyed
	// under the old Vary rules — "Vary poisoning". This makes the recorded hint
	// authoritative for the base key.
	prev, had := c.varyHints[e.baseKey]
	if (had || len(e.varyNames) > 0) && !sameStrings(prev, e.varyNames) {
		c.purgeBaseLocked(e.baseKey)
		if len(e.varyNames) > 0 {
			c.varyHints[e.baseKey] = e.varyNames
		} else {
			delete(c.varyHints, e.baseKey)
		}
	}

	if el, ok := c.entries[e.key]; ok {
		el.Value = e
		c.lru.MoveToFront(el)
		return
	}
	el := c.lru.PushFront(e)
	c.entries[e.key] = el

	for c.lru.Len() > c.maxEntries {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		c.lru.Remove(oldest)
		old := oldest.Value.(*cacheEntry)
		delete(c.entries, old.key)
	}
}

// purgeBaseLocked removes every cached variant whose base key matches. Caller holds mu.
func (c *responseCache) purgeBaseLocked(baseKey string) {
	for k, el := range c.entries {
		if el.Value.(*cacheEntry).baseKey == baseKey {
			c.lru.Remove(el)
			delete(c.entries, k)
		}
	}
}

// sameStrings reports whether two string slices are equal in order.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- keying ----

// requestKey builds the base cache key (method + host + request URI). Vary values
// are appended by store/lookup.
func requestKey(r *http.Request) string {
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	return r.Method + " " + host + " " + r.URL.RequestURI()
}

// buildVaryKey renders the request values of the given Vary header names.
func buildVaryKey(names []string, r *http.Request) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(" vary:")
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte('=')
		b.WriteString(r.Header.Get(n))
		b.WriteByte(';')
	}
	return b.String()
}

// parseVary splits and normalizes a Vary header into a stable, lowercased list.
func parseVary(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = http.CanonicalHeaderKey(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---- conditional / cache-control ----

// notModified reports whether the request's conditional validators are satisfied
// by the stored entry, meaning a 304 may be returned.
func notModified(r *http.Request, e *cacheEntry) bool {
	if inm := r.Header.Get("If-None-Match"); inm != "" && e.etag != "" {
		for _, tag := range strings.Split(inm, ",") {
			tag = strings.TrimSpace(tag)
			if tag == "*" || etagMatch(tag, e.etag) {
				return true
			}
		}
		return false
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" && e.lastMod != "" {
		imsTime, err1 := http.ParseTime(ims)
		lmTime, err2 := http.ParseTime(e.lastMod)
		if err1 == nil && err2 == nil && !lmTime.After(imsTime) {
			return true
		}
	}
	return false
}

// etagMatch compares two ETags with weak-comparison semantics (the W/ prefix is
// ignored), which is correct for cache revalidation.
func etagMatch(a, b string) bool {
	return strings.TrimPrefix(a, "W/") == strings.TrimPrefix(b, "W/")
}

// cacheControl holds the response/request directives we act on.
type cacheControl struct {
	noStore   bool
	noCache   bool
	private   bool
	public    bool
	maxAge    int
	maxAgeSet bool
	swr       int // stale-while-revalidate seconds
}

// parseCacheControl parses a Cache-Control header value. s-maxage overrides
// max-age for our (shared-cache) purposes.
func parseCacheControl(v string) cacheControl {
	var cc cacheControl
	if v == "" {
		return cc
	}
	var sMaxAge = -1
	var maxAge = -1
	for _, raw := range strings.Split(v, ",") {
		d := strings.TrimSpace(strings.ToLower(raw))
		switch {
		case d == "no-store":
			cc.noStore = true
		case d == "no-cache":
			cc.noCache = true
		case d == "private":
			cc.private = true
		case d == "public":
			cc.public = true
		case strings.HasPrefix(d, "s-maxage="):
			if n, err := strconv.Atoi(strings.TrimPrefix(d, "s-maxage=")); err == nil {
				sMaxAge = n
			}
		case strings.HasPrefix(d, "max-age="):
			if n, err := strconv.Atoi(strings.TrimPrefix(d, "max-age=")); err == nil {
				maxAge = n
			}
		case strings.HasPrefix(d, "stale-while-revalidate="):
			if n, err := strconv.Atoi(strings.TrimPrefix(d, "stale-while-revalidate=")); err == nil && n > 0 {
				cc.swr = n
			}
		}
	}
	if sMaxAge >= 0 {
		cc.maxAge, cc.maxAgeSet = sMaxAge, true
	} else if maxAge >= 0 {
		cc.maxAge, cc.maxAgeSet = maxAge, true
	}
	return cc
}

// requestBypassesCache reports whether the request's Cache-Control forbids using
// the cache (no-store or no-cache).
func requestBypassesCache(r *http.Request) bool {
	cc := parseCacheControl(r.Header.Get("Cache-Control"))
	return cc.noStore || cc.noCache
}

// ageSeconds renders the Age header value for an entry at time now.
func ageSeconds(e *cacheEntry, now time.Time) string {
	age := int(now.Sub(e.storedAt) / time.Second)
	if age < 0 {
		age = 0
	}
	return strconv.Itoa(age)
}

// ---- header helpers ----

func cloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[k] = cp
	}
	return out
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		dst[k] = append([]string(nil), vs...)
	}
}

// ---- capturing response writer ----

// cacheCapture buffers a response so it can be cached, while remaining fully
// transparent to the client: everything written is also forwarded downstream.
// If the response is hijacked (WebSocket), flushed mid-stream, or exceeds the
// body cap, it marks itself uncacheable and stops buffering.
type cacheCapture struct {
	http.ResponseWriter
	maxBody     int
	status      int
	wroteHeader bool
	body        []byte
	overflow    bool // body exceeded maxBody
	streamed    bool // Flush was called -> treat as streaming, do not cache
	hijacked    bool
}

func (c *cacheCapture) WriteHeader(code int) {
	if c.wroteHeader {
		return
	}
	c.wroteHeader = true
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *cacheCapture) Write(b []byte) (int, error) {
	if !c.wroteHeader {
		c.WriteHeader(http.StatusOK)
	}
	if !c.overflow && !c.streamed {
		if len(c.body)+len(b) > c.maxBody {
			c.overflow = true
			c.body = nil
		} else {
			c.body = append(c.body, b...)
		}
	}
	return c.ResponseWriter.Write(b)
}

// Flush marks the response as streaming (uncacheable) and forwards, so SSE and
// chunked streaming keep working through the cache middleware.
func (c *cacheCapture) Flush() {
	c.streamed = true
	c.body = nil
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack marks the response as hijacked (uncacheable) and forwards, so WebSocket
// upgrades keep working.
func (c *cacheCapture) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c.hijacked = true
	if h, ok := c.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
}

// discardWriter is a minimal ResponseWriter used for background refreshes, where
// the response body is discarded (nothing is sent to any client) but the headers
// are still captured so the refreshed entry can be stored.
type discardWriter struct {
	header http.Header
}

func newDiscardWriter() *discardWriter {
	return &discardWriter{header: make(http.Header)}
}

func (d *discardWriter) Header() http.Header         { return d.header }
func (d *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardWriter) WriteHeader(int)             {}
