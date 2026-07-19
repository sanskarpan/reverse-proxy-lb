package routing

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/config"
)

// route builds a RouteConfig with a single backend at url and the given algorithm.
func route(algo, url string) config.RouteConfig {
	return config.RouteConfig{
		Algorithm: algo,
		Backends:  []config.BackendConfig{{URL: url}},
	}
}

// backendURLs returns the URL of every backend on bal, for identifying which
// group a Route call returned without reserving anything.
func backendURLs(bal balancer.Balancer) []string {
	all := bal.All()
	urls := make([]string, len(all))
	for i, b := range all {
		urls[i] = b.URL
	}
	return urls
}

// onlyBackend asserts bal has exactly one backend and returns its URL.
func onlyBackend(t *testing.T, bal balancer.Balancer) string {
	t.Helper()
	urls := backendURLs(bal)
	if len(urls) != 1 {
		t.Fatalf("expected 1 backend, got %d (%v)", len(urls), urls)
	}
	return urls[0]
}

func newReq(t *testing.T, method, target string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	return req
}

func TestBuildGroup(t *testing.T) {
	rc := config.RouteConfig{
		Algorithm: "round_robin",
		Backends: []config.BackendConfig{
			{URL: "http://a:80"},
			{URL: "http://b:80"},
		},
	}
	bal, err := BuildGroup(rc)
	if err != nil {
		t.Fatalf("BuildGroup: %v", err)
	}
	if got := len(bal.All()); got != 2 {
		t.Fatalf("expected 2 backends, got %d", got)
	}
}

func TestBuildGroupConsistentHashOptions(t *testing.T) {
	rc := config.RouteConfig{
		Algorithm:      "consistent_hash",
		ConsistentHash: config.ConsistentHashConfig{Replicas: 50, LoadFactor: 1.5},
		Backends:       []config.BackendConfig{{URL: "http://a:80"}},
	}
	bal, err := BuildGroup(rc)
	if err != nil {
		t.Fatalf("BuildGroup: %v", err)
	}
	if _, ok := bal.(balancer.KeyedBalancer); !ok {
		t.Fatalf("consistent_hash balancer should implement KeyedBalancer")
	}
}

func TestBuildGroupUnknownAlgorithm(t *testing.T) {
	_, err := BuildGroup(config.RouteConfig{
		Algorithm: "nope",
		Backends:  []config.BackendConfig{{URL: "http://a:80"}},
	})
	if err == nil {
		t.Fatal("expected error for unknown algorithm")
	}
}

func TestRouteNoRoutesReturnsDefault(t *testing.T) {
	def, _ := balancer.NewByAlgorithm("round_robin", balancer.Options{})
	def.Add(balancer.NewBackend(config.BackendConfig{URL: "http://default:80"}))

	r, err := NewRouter(nil, def)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	got := r.Route(newReq(t, "GET", "http://x.test/anything"))
	if got != def {
		t.Fatalf("expected default balancer with no routes")
	}
}

func TestRouteHost(t *testing.T) {
	def, _ := balancer.NewByAlgorithm("round_robin", balancer.Options{})
	routes := []config.RouteConfig{
		{Host: "api.example.com", Algorithm: "round_robin", Backends: []config.BackendConfig{{URL: "http://api:80"}}},
		{Host: "web.example.com", Algorithm: "round_robin", Backends: []config.BackendConfig{{URL: "http://web:80"}}},
	}
	r, err := NewRouter(routes, def)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	// Case-insensitive exact Host match.
	got := r.Route(newReq(t, "GET", "http://API.EXAMPLE.COM/x"))
	if u := onlyBackend(t, got); u != "http://api:80" {
		t.Fatalf("host api route: got %s", u)
	}
	got = r.Route(newReq(t, "GET", "http://web.example.com/x"))
	if u := onlyBackend(t, got); u != "http://web:80" {
		t.Fatalf("host web route: got %s", u)
	}
	// Unknown host => default.
	if r.Route(newReq(t, "GET", "http://other.example.com/x")) != def {
		t.Fatalf("unknown host should fall back to default")
	}
}

func TestRoutePathPrefix(t *testing.T) {
	def, _ := balancer.NewByAlgorithm("round_robin", balancer.Options{})
	routes := []config.RouteConfig{
		route("round_robin", "http://apisvc:80"),
		route("round_robin", "http://staticsvc:80"),
	}
	routes[0].PathPrefix = "/api"
	routes[1].PathPrefix = "/static"

	r, err := NewRouter(routes, def)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	if u := onlyBackend(t, r.Route(newReq(t, "GET", "http://h/api/v1/users"))); u != "http://apisvc:80" {
		t.Fatalf("/api prefix: got %s", u)
	}
	if u := onlyBackend(t, r.Route(newReq(t, "GET", "http://h/static/logo.png"))); u != "http://staticsvc:80" {
		t.Fatalf("/static prefix: got %s", u)
	}
	if r.Route(newReq(t, "GET", "http://h/other")) != def {
		t.Fatalf("non-matching path should fall back to default")
	}
}

func TestRouteMethodsAnyOf(t *testing.T) {
	def, _ := balancer.NewByAlgorithm("round_robin", balancer.Options{})
	rc := route("round_robin", "http://writes:80")
	rc.Methods = []string{"POST", "put"} // mixed case; any-of
	r, err := NewRouter([]config.RouteConfig{rc}, def)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	for _, m := range []string{"POST", "PUT"} {
		if u := onlyBackend(t, r.Route(newReq(t, m, "http://h/x"))); u != "http://writes:80" {
			t.Fatalf("method %s should match, got %s", m, u)
		}
	}
	if r.Route(newReq(t, "GET", "http://h/x")) != def {
		t.Fatalf("GET should not match a POST/PUT route")
	}
}

func TestRouteHeadersAllMustMatch(t *testing.T) {
	def, _ := balancer.NewByAlgorithm("round_robin", balancer.Options{})
	rc := route("round_robin", "http://canary:80")
	rc.Headers = map[string]string{
		"X-Canary":     "true",
		"X-Tenant":     "acme",
		"content-type": "application/json", // non-canonical name in config
	}
	r, err := NewRouter([]config.RouteConfig{rc}, def)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	// All headers present and exact => match.
	req := newReq(t, "GET", "http://h/x")
	req.Header.Set("X-Canary", "true")
	req.Header.Set("X-Tenant", "acme")
	req.Header.Set("Content-Type", "application/json")
	if u := onlyBackend(t, r.Route(req)); u != "http://canary:80" {
		t.Fatalf("all headers match => canary, got %s", u)
	}

	// One header missing => no match.
	req = newReq(t, "GET", "http://h/x")
	req.Header.Set("X-Canary", "true")
	req.Header.Set("X-Tenant", "acme")
	if r.Route(req) != def {
		t.Fatalf("missing Content-Type should fall back to default")
	}

	// One header wrong value => no match.
	req = newReq(t, "GET", "http://h/x")
	req.Header.Set("X-Canary", "true")
	req.Header.Set("X-Tenant", "other")
	req.Header.Set("Content-Type", "application/json")
	if r.Route(req) != def {
		t.Fatalf("wrong X-Tenant should fall back to default")
	}
}

func TestRouteCombinedCriteria(t *testing.T) {
	def, _ := balancer.NewByAlgorithm("round_robin", balancer.Options{})
	rc := route("round_robin", "http://combo:80")
	rc.Host = "api.example.com"
	rc.PathPrefix = "/v2"
	rc.Methods = []string{"POST"}
	rc.Headers = map[string]string{"X-Key": "abc"}
	r, err := NewRouter([]config.RouteConfig{rc}, def)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	match := newReq(t, "POST", "http://api.example.com/v2/do")
	match.Header.Set("X-Key", "abc")
	if u := onlyBackend(t, r.Route(match)); u != "http://combo:80" {
		t.Fatalf("all criteria satisfied should match, got %s", u)
	}

	// Wrong path prefix, everything else right => no match.
	miss := newReq(t, "POST", "http://api.example.com/v1/do")
	miss.Header.Set("X-Key", "abc")
	if r.Route(miss) != def {
		t.Fatalf("path prefix mismatch should fall back to default")
	}
}

func TestRouteFirstMatchWins(t *testing.T) {
	def, _ := balancer.NewByAlgorithm("round_robin", balancer.Options{})
	// Two routes both match /api/*; the first configured must win.
	first := route("round_robin", "http://first:80")
	first.PathPrefix = "/api"
	second := route("round_robin", "http://second:80")
	second.PathPrefix = "/api" // also matches, but ordered second

	r, err := NewRouter([]config.RouteConfig{first, second}, def)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if u := onlyBackend(t, r.Route(newReq(t, "GET", "http://h/api/x"))); u != "http://first:80" {
		t.Fatalf("first matching route should win, got %s", u)
	}
}

func TestRouteCatchAllRoute(t *testing.T) {
	def, _ := balancer.NewByAlgorithm("round_robin", balancer.Options{})
	catchAll := route("round_robin", "http://catch:80") // no criteria
	r, err := NewRouter([]config.RouteConfig{catchAll}, def)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if u := onlyBackend(t, r.Route(newReq(t, "DELETE", "http://anything/here"))); u != "http://catch:80" {
		t.Fatalf("catch-all route should match everything, got %s", u)
	}
}

func TestRouteReservesNothing(t *testing.T) {
	def, _ := balancer.NewByAlgorithm("round_robin", balancer.Options{})
	rc := route("round_robin", "http://svc:80")
	rc.PathPrefix = "/api"
	r, err := NewRouter([]config.RouteConfig{rc}, def)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	bal := r.Route(newReq(t, "GET", "http://h/api/x"))
	for _, b := range bal.All() {
		if b.GetActiveConns() != 0 {
			t.Fatalf("Route must not reserve backends; active conns = %d", b.GetActiveConns())
		}
	}
	// Calling Route repeatedly still reserves nothing.
	for i := 0; i < 5; i++ {
		r.Route(newReq(t, "GET", "http://h/api/x"))
	}
	for _, b := range bal.All() {
		if b.GetActiveConns() != 0 {
			t.Fatalf("repeated Route reserved a backend; active conns = %d", b.GetActiveConns())
		}
	}
}

func TestGroups(t *testing.T) {
	def, _ := balancer.NewByAlgorithm("round_robin", balancer.Options{})
	def.Add(balancer.NewBackend(config.BackendConfig{URL: "http://default:80"}))
	routes := []config.RouteConfig{
		route("round_robin", "http://r1:80"),
		route("least_conn", "http://r2:80"),
	}
	r, err := NewRouter(routes, def)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	groups := r.Groups()
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups (default + 2 routes), got %d", len(groups))
	}
	if groups[0] != def {
		t.Fatalf("first group should be the default balancer")
	}
	if u := onlyBackend(t, groups[1]); u != "http://r1:80" {
		t.Fatalf("group[1] backend = %s", u)
	}
	if u := onlyBackend(t, groups[2]); u != "http://r2:80" {
		t.Fatalf("group[2] backend = %s", u)
	}
}

func TestGroupsNoRoutes(t *testing.T) {
	def, _ := balancer.NewByAlgorithm("round_robin", balancer.Options{})
	r, err := NewRouter(nil, def)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	groups := r.Groups()
	if len(groups) != 1 || groups[0] != def {
		t.Fatalf("no-routes Groups should be exactly [default]")
	}
}

func TestNewRouterPropagatesBuildError(t *testing.T) {
	def, _ := balancer.NewByAlgorithm("round_robin", balancer.Options{})
	_, err := NewRouter([]config.RouteConfig{
		{Algorithm: "bogus", Backends: []config.BackendConfig{{URL: "http://a:80"}}},
	}, def)
	if err == nil {
		t.Fatal("expected NewRouter to propagate BuildGroup error")
	}
}
