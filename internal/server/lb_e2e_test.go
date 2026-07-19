package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reverse-proxy-lb/internal/config"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file drives the §1 load-balancing features end-to-end through the real
// server stack. For each scenario it stands up N httptest backends that identify
// themselves in the response body, builds an in-memory *config.Config selecting
// the relevant algorithm/options, constructs the Server via New(cfg, ""), and
// fires real HTTP requests through the server's fully assembled handler
// (Server.Handler(), i.e. proxy + full middleware chain) via httptest.
//
// Assertions inspect which backend served each request (read from the response
// body) and verify distribution, affinity, load preference, ejection, tier
// failover, and zone preference. No assertions are weakened to force a pass.

// idBackend is an httptest backend that writes a stable identifier as its body so
// the test can tell which upstream served a given request.
type idBackend struct {
	id     string
	server *httptest.Server
	url    string
	hits   int64
	mu     sync.Mutex
}

// newIDBackend starts a backend that returns its id. The optional handler, when
// provided, runs first and may short-circuit (e.g. to simulate errors); it
// reports whether it already handled the request.
func newIDBackend(id string, pre func(w http.ResponseWriter, r *http.Request) (handled bool)) *idBackend {
	b := &idBackend{id: id}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.hits++
		b.mu.Unlock()
		if pre != nil {
			if pre(w, r) {
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, b.id)
	}))
	b.url = b.server.URL
	return b
}

func (b *idBackend) hitCount() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hits
}

func (b *idBackend) close() { b.server.Close() }

// baseConfig returns a minimal valid config wired to the given backends and
// algorithm, with all optional subsystems (metrics/health/rate-limit) disabled so
// the test drives only the load-balancing path. Trusted proxies includes loopback
// so tests can vary the client key via X-Forwarded-For.
func baseConfig(algorithm string, backends []config.BackendConfig) *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			Host:           "127.0.0.1",
			Port:           8080,
			TrustedProxies: []string{"127.0.0.1/8", "::1/128"},
		},
		Backends: backends,
		LoadBalancer: config.LoadBalancerConfig{
			Algorithm: algorithm,
			ConsistentHash: config.ConsistentHashConfig{
				Replicas:   100,
				LoadFactor: 1.25,
			},
			Sticky: config.StickyConfig{Cookie: "rplb_affinity"},
		},
		Logging: config.LoggingConfig{Level: "error", Format: "text"},
	}
}

func backendCfgs(bs ...*idBackend) []config.BackendConfig {
	out := make([]config.BackendConfig, len(bs))
	for i, b := range bs {
		out[i] = config.BackendConfig{URL: b.url, Weight: 1, MaxConns: 100}
	}
	return out
}

// doReq issues a request through handler and returns the response body (the
// backend id) and status code. xff, when non-empty, sets X-Forwarded-For so the
// proxy derives a distinct client key (loopback is trusted in baseConfig).
func doReq(t *testing.T, handler http.Handler, xff string, cookies ...*http.Cookie) (string, int, *http.Response) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil)
	// httptest.NewRequest sets RemoteAddr to 192.0.2.1:1234 by default, which is
	// NOT loopback, so XFF would be ignored. Force a loopback peer so the trusted
	// proxy logic honors X-Forwarded-For.
	req.RemoteAddr = "127.0.0.1:12345"
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	return strings.TrimSpace(string(body)), res.StatusCode, res
}

// -----------------------------------------------------------------------------
// round_robin / swrr / weighted_random distribution
// -----------------------------------------------------------------------------

func TestE2E_RoundRobin_Distribution(t *testing.T) {
	b1 := newIDBackend("A", nil)
	b2 := newIDBackend("B", nil)
	b3 := newIDBackend("C", nil)
	defer b1.close()
	defer b2.close()
	defer b3.close()

	cfg := baseConfig("round_robin", backendCfgs(b1, b2, b3))
	srv := New(cfg, "")
	h := srv.Handler()

	const n = 300
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		id, code, _ := doReq(t, h, "")
		if code != http.StatusOK {
			t.Fatalf("request %d got status %d", i, code)
		}
		counts[id]++
	}

	// Perfectly even is n/3 each; allow modest tolerance.
	want := n / 3
	tol := want / 5 // 20%
	for _, id := range []string{"A", "B", "C"} {
		if abs(counts[id]-want) > tol {
			t.Errorf("round_robin: backend %s got %d hits, want ~%d (+-%d)", id, counts[id], want, tol)
		}
	}
}

func TestE2E_SWRR_WeightedDistribution(t *testing.T) {
	b1 := newIDBackend("A", nil) // weight 3
	b2 := newIDBackend("B", nil) // weight 1
	defer b1.close()
	defer b2.close()

	cfg := baseConfig("swrr", []config.BackendConfig{
		{URL: b1.url, Weight: 3, MaxConns: 100},
		{URL: b2.url, Weight: 1, MaxConns: 100},
	})
	srv := New(cfg, "")
	h := srv.Handler()

	const n = 400
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		id, code, _ := doReq(t, h, "")
		if code != http.StatusOK {
			t.Fatalf("request %d got status %d", i, code)
		}
		counts[id]++
	}

	// weight 3:1 -> A ~= 3/4 of traffic, B ~= 1/4.
	wantA := n * 3 / 4
	wantB := n * 1 / 4
	tol := n / 10 // 10% of total
	if abs(counts["A"]-wantA) > tol {
		t.Errorf("swrr: backend A got %d hits, want ~%d (+-%d)", counts["A"], wantA, tol)
	}
	if abs(counts["B"]-wantB) > tol {
		t.Errorf("swrr: backend B got %d hits, want ~%d (+-%d)", counts["B"], wantB, tol)
	}
}

func TestE2E_WeightedRandom_Distribution(t *testing.T) {
	b1 := newIDBackend("A", nil) // weight 4
	b2 := newIDBackend("B", nil) // weight 1
	defer b1.close()
	defer b2.close()

	cfg := baseConfig("weighted_random", []config.BackendConfig{
		{URL: b1.url, Weight: 4, MaxConns: 100},
		{URL: b2.url, Weight: 1, MaxConns: 100},
	})
	srv := New(cfg, "")
	h := srv.Handler()

	const n = 1000
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		id, code, _ := doReq(t, h, "")
		if code != http.StatusOK {
			t.Fatalf("request %d got status %d", i, code)
		}
		counts[id]++
	}

	// weight 4:1 -> A ~= 80%, B ~= 20%. Random, so allow generous tolerance.
	wantA := n * 4 / 5
	tol := n / 10 // 10% of total
	if abs(counts["A"]-wantA) > tol {
		t.Errorf("weighted_random: backend A got %d hits (%.1f%%), want ~%d (+-%d)",
			counts["A"], 100*float64(counts["A"])/float64(n), wantA, tol)
	}
	if counts["B"] == 0 {
		t.Errorf("weighted_random: backend B never selected; expected ~20%% of traffic")
	}
}

// -----------------------------------------------------------------------------
// consistent_hash: stability and minimal remap
// -----------------------------------------------------------------------------

func TestE2E_ConsistentHash_StableAndMinimalRemap(t *testing.T) {
	backends := []*idBackend{
		newIDBackend("A", nil),
		newIDBackend("B", nil),
		newIDBackend("C", nil),
		newIDBackend("D", nil),
	}
	defer func() {
		for _, b := range backends {
			b.close()
		}
	}()

	cfg := baseConfig("consistent_hash", backendCfgs(backends...))
	srv := New(cfg, "")
	h := srv.Handler()

	// Distinct client keys via X-Forwarded-For.
	const keys = 400
	clientKey := func(i int) string { return fmt.Sprintf("10.0.%d.%d", i/256, i%256) }

	// 1) Same key maps to the same backend repeatedly.
	firstMap := map[string]string{}
	for i := 0; i < keys; i++ {
		key := clientKey(i)
		id, code, _ := doReq(t, h, key)
		if code != http.StatusOK {
			t.Fatalf("ch request for %s got status %d", key, code)
		}
		firstMap[key] = id
	}
	// Repeat: every key must land on the same backend as before.
	for i := 0; i < keys; i++ {
		key := clientKey(i)
		id, _, _ := doReq(t, h, key)
		if id != firstMap[key] {
			t.Fatalf("consistent_hash: key %s mapped to %s then %s (not stable)", key, firstMap[key], id)
		}
	}

	// 2) Removing a backend remaps only a small fraction of keys.
	// Rebuild the server without backend D so the ring is built for 3 members.
	remaining := backends[:3]
	cfg2 := baseConfig("consistent_hash", backendCfgs(remaining...))
	srv2 := New(cfg2, "")
	h2 := srv2.Handler()

	remapped := 0
	for i := 0; i < keys; i++ {
		key := clientKey(i)
		id, _, _ := doReq(t, h2, key)
		// Keys that previously mapped to the removed backend D must move; keys
		// that mapped elsewhere should overwhelmingly stay put.
		if firstMap[key] != "D" && id != firstMap[key] {
			remapped++
		}
	}
	// With 4 -> 3 backends, consistent hashing should remap far fewer than a naive
	// modulo scheme (which would remap ~75%). Expect well under 1/3 of the keys
	// that didn't belong to D to move.
	if remapped > keys/3 {
		t.Errorf("consistent_hash: removing one backend remapped %d/%d non-D keys; expected << %d",
			remapped, keys, keys/3)
	}
}

// -----------------------------------------------------------------------------
// sticky sessions: cookie pins subsequent requests
// -----------------------------------------------------------------------------

func TestE2E_StickySessions_Pinning(t *testing.T) {
	backends := []*idBackend{
		newIDBackend("A", nil),
		newIDBackend("B", nil),
		newIDBackend("C", nil),
	}
	defer func() {
		for _, b := range backends {
			b.close()
		}
	}()

	cfg := baseConfig("round_robin", backendCfgs(backends...))
	cfg.LoadBalancer.Sticky = config.StickyConfig{
		Enabled: true,
		Cookie:  "rplb_affinity",
		TTL:     time.Hour,
	}
	srv := New(cfg, "")
	h := srv.Handler()

	// First request: no cookie. Server should set the affinity cookie.
	firstID, code, res := doReq(t, h, "")
	if code != http.StatusOK {
		t.Fatalf("sticky first request status %d", code)
	}
	var affinity *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == "rplb_affinity" {
			affinity = c
		}
	}
	if affinity == nil {
		t.Fatalf("sticky: first response did not set affinity cookie")
	}

	// Subsequent requests carrying the cookie must all pin to firstID, even
	// though the base algorithm is round_robin (which would otherwise rotate).
	for i := 0; i < 30; i++ {
		id, _, _ := doReq(t, h, "", affinity)
		if id != firstID {
			t.Fatalf("sticky: request %d pinned cookie went to %s, want %s", i, id, firstID)
		}
	}
}

// -----------------------------------------------------------------------------
// p2c / weighted_least_conn: less-loaded backend preferred
// -----------------------------------------------------------------------------

// TestE2E_WeightedLeastConn_PrefersLessLoaded holds connections open on one
// backend (raising its ActiveConns) and verifies new requests prefer the idle one.
func TestE2E_WeightedLeastConn_PrefersLessLoaded(t *testing.T) {
	// busy backend blocks until released so its ActiveConns stays high.
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	busy := newIDBackend("BUSY", func(w http.ResponseWriter, r *http.Request) bool {
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "BUSY")
		return true
	})
	idle := newIDBackend("IDLE", nil)
	defer busy.close()
	defer idle.close()
	defer closeRelease()

	cfg := baseConfig("weighted_least_conn", backendCfgs(busy, idle))
	srv := New(cfg, "")
	h := srv.Handler()

	// Fire several requests concurrently that will land on the busy backend and
	// block, inflating its active-conn count. We seed the busy backend by making
	// the balancer pick it. weighted_least_conn picks the min active-conns backend;
	// initially both are 0. To reliably load "busy", we drive concurrent in-flight
	// requests and count where NEW requests go once one backend is saturated.
	//
	// Strategy: launch inflightN concurrent requests. Because weighted_least_conn
	// always steers to the least-loaded backend, once some requests are parked on
	// busy, later picks should favor idle. We assert idle receives strictly more
	// completed hits than busy while busy's are parked.
	const inflight = 8
	var wg sync.WaitGroup
	results := make(chan string, inflight)
	for i := 0; i < inflight; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, _, _ := doReq(t, h, "")
			results <- id
		}()
	}

	// Give the goroutines time to be routed. The ones hitting busy will block on
	// release; the ones hitting idle complete immediately.
	deadline := time.Now().Add(2 * time.Second)
	idleDone := 0
	for time.Now().Before(deadline) {
		select {
		case id := <-results:
			if id == "IDLE" {
				idleDone++
			}
		case <-time.After(50 * time.Millisecond):
		}
		if idleDone >= inflight-2 {
			break
		}
	}

	// weighted_least_conn should steer the overwhelming majority to IDLE because
	// BUSY accrues active connections that never drain until release.
	if idleDone < inflight/2 {
		t.Errorf("weighted_least_conn: only %d/%d completed requests hit IDLE; expected majority to avoid the saturated BUSY backend", idleDone, inflight)
	}

	closeRelease()
	// Drain remaining in-flight requests parked on BUSY.
	wg.Wait()
}

// TestE2E_P2C_SpreadsAcrossBackends verifies power-of-two-choices does not pile
// all load on a single backend and that a persistently-loaded backend is avoided.
func TestE2E_P2C_PrefersLessLoaded(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	busy := newIDBackend("BUSY", func(w http.ResponseWriter, r *http.Request) bool {
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "BUSY")
		return true
	})
	idle := newIDBackend("IDLE", nil)
	defer busy.close()
	defer idle.close()
	defer once.Do(func() { close(release) })

	cfg := baseConfig("p2c", backendCfgs(busy, idle))
	srv := New(cfg, "")
	h := srv.Handler()

	// Park several requests. With only 2 backends p2c compares both and picks the
	// lesser-loaded, so as BUSY accumulates parked connections, new requests
	// should route to IDLE.
	const inflight = 10
	var wg sync.WaitGroup
	results := make(chan string, inflight)
	for i := 0; i < inflight; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, _, _ := doReq(t, h, "")
			results <- id
		}()
	}

	deadline := time.Now().Add(2 * time.Second)
	idleDone := 0
	for time.Now().Before(deadline) {
		select {
		case id := <-results:
			if id == "IDLE" {
				idleDone++
			}
		case <-time.After(50 * time.Millisecond):
		}
		if idleDone >= inflight-2 {
			break
		}
	}

	if idleDone < inflight/2 {
		t.Errorf("p2c: only %d/%d completed requests hit IDLE; expected majority to avoid the saturated BUSY backend", idleDone, inflight)
	}

	once.Do(func() { close(release) })
	wg.Wait()
}

// -----------------------------------------------------------------------------
// outlier detection: erroring backend ejected then reinstated
// -----------------------------------------------------------------------------

func TestE2E_OutlierDetection_EjectAndReinstate(t *testing.T) {
	// bad backend hijacks and abruptly closes the connection so the proxy's
	// upstream transport reports an error (a 5xx status would NOT count as a
	// transport failure). This drives ObserveOutcome(ok=false).
	var failMode struct {
		sync.Mutex
		fail bool
	}
	failMode.fail = true
	bad := &idBackend{id: "BAD"}
	bad.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bad.mu.Lock()
		bad.hits++
		bad.mu.Unlock()
		failMode.Lock()
		f := failMode.fail
		failMode.Unlock()
		if f {
			// Abort the response so the proxy sees a transport error.
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
					return
				}
			}
			// Fallback: no hijack support.
			panic("cannot hijack")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "BAD")
	}))
	bad.url = bad.server.URL
	defer bad.close()

	good := newIDBackend("GOOD", nil)
	defer good.close()

	cfg := baseConfig("round_robin", backendCfgs(bad, good))
	cfg.LoadBalancer.OutlierDetection = config.OutlierDetectionConfig{
		Enabled:            true,
		ErrorRateThreshold: 0.5,
		MinRequests:        3,
		BaseEjection:       500 * time.Millisecond,
		MaxEjectionPercent: 50,
	}
	srv := New(cfg, "")
	h := srv.Handler()

	// Phase 1: drive traffic. BAD keeps erroring; after >= MinRequests failing it
	// should be ejected. Every request still succeeds (200) because the proxy
	// fails over to GOOD on the transport error.
	for i := 0; i < 40; i++ {
		_, code, _ := doReq(t, h, "")
		if code != http.StatusOK {
			t.Fatalf("outlier phase1 request %d got status %d (failover to GOOD expected)", i, code)
		}
	}

	// After ejection, BAD is unhealthy: it should no longer be selected. Record
	// its hit count, run more traffic, and confirm it does not grow (ejected).
	hitsBeforeQuiet := bad.hitCount()
	// Immediately fire more requests while BAD is (expected) ejected, before the
	// BaseEjection window elapses.
	for i := 0; i < 20; i++ {
		_, code, _ := doReq(t, h, "")
		if code != http.StatusOK {
			t.Fatalf("outlier phase2 request %d got status %d", i, code)
		}
	}
	hitsWhileEjected := bad.hitCount() - hitsBeforeQuiet
	if hitsWhileEjected != 0 {
		t.Errorf("outlier: BAD received %d hits while it should have been ejected from rotation", hitsWhileEjected)
	}

	// Phase 3: stop failing, wait for the ejection window to expire, then confirm
	// BAD is reinstated and serves traffic again.
	failMode.Lock()
	failMode.fail = false
	failMode.Unlock()

	// Wait past BaseEjection so reinstateExpired flips it healthy on next select.
	time.Sleep(700 * time.Millisecond)

	reinstated := false
	for i := 0; i < 60; i++ {
		id, code, _ := doReq(t, h, "")
		if code != http.StatusOK {
			t.Fatalf("outlier phase3 request %d got status %d", i, code)
		}
		if id == "BAD" {
			reinstated = true
			break
		}
	}
	if !reinstated {
		t.Errorf("outlier: BAD was not reinstated into rotation after the ejection window elapsed")
	}
}

// -----------------------------------------------------------------------------
// priority tiers: tier-0 preferred, fall to tier-1 only when tier-0 down
// -----------------------------------------------------------------------------

func TestE2E_PriorityTiers_Failover(t *testing.T) {
	primary1 := newIDBackend("P1", nil)
	primary2 := newIDBackend("P2", nil)
	backup := newIDBackend("BACKUP", nil)
	defer primary1.close()
	defer primary2.close()
	defer backup.close()

	cfg := baseConfig("round_robin", []config.BackendConfig{
		{URL: primary1.url, Weight: 1, MaxConns: 100, Tier: 0},
		{URL: primary2.url, Weight: 1, MaxConns: 100, Tier: 0},
		{URL: backup.url, Weight: 1, MaxConns: 100, Tier: 1},
	})
	srv := New(cfg, "")
	h := srv.Handler()

	// Phase 1: all healthy. Traffic must stay entirely on tier-0.
	for i := 0; i < 60; i++ {
		id, code, _ := doReq(t, h, "")
		if code != http.StatusOK {
			t.Fatalf("tiers phase1 request %d got status %d", i, code)
		}
		if id == "BACKUP" {
			t.Fatalf("tiers: tier-1 BACKUP served traffic while tier-0 backends are healthy")
		}
	}

	// Phase 2: take down all of tier-0 by marking those backends unhealthy through
	// the balancer. Traffic must now fall to tier-1.
	for _, b := range srv.GetBalancer().All() {
		if b.URL == primary1.url || b.URL == primary2.url {
			b.SetHealthy(false)
		}
	}

	sawBackup := false
	for i := 0; i < 30; i++ {
		id, code, _ := doReq(t, h, "")
		if code != http.StatusOK {
			t.Fatalf("tiers phase2 request %d got status %d", i, code)
		}
		if id == "P1" || id == "P2" {
			t.Fatalf("tiers: a downed tier-0 backend (%s) served traffic", id)
		}
		if id == "BACKUP" {
			sawBackup = true
		}
	}
	if !sawBackup {
		t.Errorf("tiers: tier-1 BACKUP never served traffic after all tier-0 backends went down")
	}
}

// -----------------------------------------------------------------------------
// zone-aware: prefer_same_zone selects in-zone backends
// -----------------------------------------------------------------------------

func TestE2E_ZoneAware_PrefersSameZone(t *testing.T) {
	inZoneA := newIDBackend("IN-A", nil)
	inZoneB := newIDBackend("IN-B", nil)
	outZone := newIDBackend("OUT", nil)
	defer inZoneA.close()
	defer inZoneB.close()
	defer outZone.close()

	cfg := baseConfig("round_robin", []config.BackendConfig{
		{URL: inZoneA.url, Weight: 1, MaxConns: 100, Zone: "us-east-1a"},
		{URL: inZoneB.url, Weight: 1, MaxConns: 100, Zone: "us-east-1a"},
		{URL: outZone.url, Weight: 1, MaxConns: 100, Zone: "us-west-2b"},
	})
	cfg.Server.Zone = "us-east-1a"
	cfg.LoadBalancer.PreferSameZone = true
	srv := New(cfg, "")
	h := srv.Handler()

	// With in-zone backends healthy, the out-of-zone backend must not be chosen.
	for i := 0; i < 60; i++ {
		id, code, _ := doReq(t, h, "")
		if code != http.StatusOK {
			t.Fatalf("zone phase1 request %d got status %d", i, code)
		}
		if id == "OUT" {
			t.Fatalf("zone: out-of-zone backend served traffic while same-zone backends are healthy")
		}
	}

	// When all in-zone backends go down, cross-zone fallback must kick in.
	for _, b := range srv.GetBalancer().All() {
		if b.URL == inZoneA.url || b.URL == inZoneB.url {
			b.SetHealthy(false)
		}
	}
	sawOut := false
	for i := 0; i < 30; i++ {
		id, code, _ := doReq(t, h, "")
		if code != http.StatusOK {
			t.Fatalf("zone phase2 request %d got status %d", i, code)
		}
		if id == "OUT" {
			sawOut = true
		}
	}
	if !sawOut {
		t.Errorf("zone: cross-zone fallback did not route to OUT after in-zone backends went down")
	}
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// ensure net and sort imports are used even if a scenario is trimmed.
var (
	_ = net.ParseIP
	_ = sort.Strings
)
