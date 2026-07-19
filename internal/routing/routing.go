// Package routing implements optional L7 request routing. Requests are matched
// (first match wins) against a list of routes, each backed by its own balancer
// group (algorithm + backends). Unmatched requests fall through to the default
// balancer. When no routes are configured the Router simply returns the default
// balancer for every request, preserving the single-balancer behavior.
//
// Per-route groups here are base balancers only: no advanced wrappers (priority
// tiers, zone awareness, slow start, outlier detection) are applied. Those
// remain on the default group; per-route wrappers are a documented follow-up.
package routing

import (
	"net/http"
	"strings"

	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/config"
)

// BuildGroup constructs a base balancer for a single route. It builds the
// balancer named by rc.Algorithm (via balancer.NewByAlgorithm, passing the
// route's consistent-hash options), then creates and adds a backend for each
// entry in rc.Backends. No wrappers are applied. Reserve-on-select semantics of
// the returned balancer are unchanged.
func BuildGroup(rc config.RouteConfig) (balancer.Balancer, error) {
	b, err := balancer.NewByAlgorithm(rc.Algorithm, balancer.Options{
		ConsistentHashReplicas:   rc.ConsistentHash.Replicas,
		ConsistentHashLoadFactor: rc.ConsistentHash.LoadFactor,
	})
	if err != nil {
		return nil, err
	}
	for _, bc := range rc.Backends {
		b.Add(balancer.NewBackend(bc))
	}
	return b, nil
}

// matcher holds the precomputed match criteria for a single route alongside the
// balancer group it selects.
type matcher struct {
	host       string            // lowercased exact Host, "" means no Host criterion
	pathPrefix string            // "" means no PathPrefix criterion
	methods    []string          // uppercased; nil/empty means no Method criterion
	headers    map[string]string // canonical header name -> exact value; nil means no Header criterion
	bal        balancer.Balancer
}

// matches reports whether req satisfies every configured criterion. Absent
// criteria are ignored. A matcher with no criteria at all is a catch-all.
func (m *matcher) matches(req *http.Request) bool {
	if m.host != "" && !strings.EqualFold(req.Host, m.host) {
		return false
	}
	if m.pathPrefix != "" {
		path := ""
		if req.URL != nil {
			path = req.URL.Path
		}
		if !strings.HasPrefix(path, m.pathPrefix) {
			return false
		}
	}
	if len(m.methods) > 0 {
		ok := false
		for _, meth := range m.methods {
			if strings.EqualFold(req.Method, meth) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for name, want := range m.headers {
		if req.Header.Get(name) != want {
			return false
		}
	}
	return true
}

// Router matches requests to a per-route balancer group, falling back to a
// default balancer when no route matches (including the no-routes case).
type Router struct {
	routes []*matcher
	def    balancer.Balancer
}

// NewRouter builds a Router from the given routes and default balancer. Each
// route's balancer group is constructed via BuildGroup; routes are evaluated in
// order (first match wins) at Route time. def is returned when no route matches.
func NewRouter(routes []config.RouteConfig, def balancer.Balancer) (*Router, error) {
	r := &Router{def: def}
	for _, rc := range routes {
		bal, err := BuildGroup(rc)
		if err != nil {
			return nil, err
		}
		m := &matcher{
			host:       strings.ToLower(rc.Host),
			pathPrefix: rc.PathPrefix,
			bal:        bal,
		}
		if len(rc.Methods) > 0 {
			m.methods = make([]string, len(rc.Methods))
			for i, meth := range rc.Methods {
				m.methods[i] = strings.ToUpper(meth)
			}
		}
		if len(rc.Headers) > 0 {
			m.headers = make(map[string]string, len(rc.Headers))
			for name, val := range rc.Headers {
				m.headers[http.CanonicalHeaderKey(name)] = val
			}
		}
		r.routes = append(r.routes, m)
	}
	return r, nil
}

// Route returns the balancer for the first configured route that matches req,
// or the default balancer if none match. It performs matching only: it does not
// call Next / reserve any backend.
func (r *Router) Route(req *http.Request) balancer.Balancer {
	for _, m := range r.routes {
		if m.matches(req) {
			return m.bal
		}
	}
	return r.def
}

// Groups returns every balancer managed by the Router: the default balancer
// followed by each per-route group, in route order. The server uses this to run
// a health checker per group.
func (r *Router) Groups() []balancer.Balancer {
	groups := make([]balancer.Balancer, 0, len(r.routes)+1)
	groups = append(groups, r.def)
	for _, m := range r.routes {
		groups = append(groups, m.bal)
	}
	return groups
}
