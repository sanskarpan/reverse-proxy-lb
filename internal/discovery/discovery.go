// Package discovery implements DNS-based service discovery for the load
// balancer. It periodically resolves configured DNS targets (A or SRV records)
// and syncs the resulting backend set into a balancer.Balancer (the default
// backend group), adding backends that appear and removing ones that disappear.
//
// A Discoverer only ever manages the backends it created: every synced backend
// is tracked per target, so statically-configured backends and backends owned
// by other targets are never touched.
package discovery

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/config"
)

// Addr is a resolved SRV endpoint: a target host plus its port.
type Addr struct {
	Host string
	Port int
}

// Resolver abstracts the DNS lookups the discoverer needs. The default stdlib
// implementation is used in production; tests inject a fake.
type Resolver interface {
	// LookupSRV resolves the SRV records for the given service name, returning
	// the target host/port pairs. Implementations should treat service as a
	// plain SRV name (empty service/proto).
	LookupSRV(service string) ([]Addr, error)
	// LookupHost resolves the A/AAAA records for name, returning the host
	// addresses.
	LookupHost(name string) ([]string, error)
}

// stdResolver is the default Resolver backed by the net package.
type stdResolver struct{}

// NewResolver returns the default stdlib-backed Resolver.
func NewResolver() Resolver { return stdResolver{} }

func (stdResolver) LookupSRV(service string) ([]Addr, error) {
	// Empty service/proto means "name" is treated as a plain SRV record name.
	_, srvs, err := net.LookupSRV("", "", service)
	if err != nil {
		return nil, err
	}
	addrs := make([]Addr, 0, len(srvs))
	for _, s := range srvs {
		host := s.Target
		// SRV targets are typically fully qualified with a trailing dot.
		if n := len(host); n > 0 && host[n-1] == '.' {
			host = host[:n-1]
		}
		addrs = append(addrs, Addr{Host: host, Port: int(s.Port)})
	}
	return addrs, nil
}

func (stdResolver) LookupHost(name string) ([]string, error) {
	return net.LookupHost(name)
}

// Discoverer periodically resolves a set of DNS targets and syncs the resulting
// backends into a balancer. It only manages backends it created.
type Discoverer struct {
	b        balancer.Balancer
	targets  []config.DNSTarget
	resolver Resolver

	stopCh chan struct{}
	wg     sync.WaitGroup

	mu sync.Mutex
	// owned maps target name -> set of URL -> *Backend that this discoverer
	// created for that target. Guards which backends we may remove.
	owned map[string]map[string]*balancer.Backend
}

// NewDiscoverer creates a Discoverer that will sync the given targets into b
// using resolver. If resolver is nil, the default stdlib resolver is used.
func NewDiscoverer(b balancer.Balancer, targets []config.DNSTarget, resolver Resolver) *Discoverer {
	if resolver == nil {
		resolver = NewResolver()
	}
	return &Discoverer{
		b:        b,
		targets:  targets,
		resolver: resolver,
		stopCh:   make(chan struct{}),
		owned:    make(map[string]map[string]*balancer.Backend),
	}
}

// Start launches one goroutine per target. Each performs an immediate resolve,
// then re-resolves every target.Interval until Stop is called.
func (d *Discoverer) Start() {
	for _, t := range d.targets {
		d.wg.Add(1)
		go d.run(t)
	}
}

// Stop ends all goroutines and blocks until they exit. Safe to call once.
func (d *Discoverer) Stop() {
	select {
	case <-d.stopCh:
		// already stopped
	default:
		close(d.stopCh)
	}
	d.wg.Wait()
}

func (d *Discoverer) run(t config.DNSTarget) {
	defer d.wg.Done()

	// Resolve immediately so backends are available without waiting a full
	// interval, then tick.
	d.sync(t)

	ticker := time.NewTicker(t.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopCh:
			return
		case <-ticker.C:
			d.sync(t)
		}
	}
}

// resolve returns the set of "scheme://host:port" URLs for the target.
func (d *Discoverer) resolve(t config.DNSTarget) (map[string]struct{}, error) {
	scheme := t.Scheme
	if scheme == "" {
		scheme = "http"
	}

	urls := make(map[string]struct{})
	switch strings.ToLower(t.Type) {
	case "srv":
		addrs, err := d.resolver.LookupSRV(t.Name)
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			urls[fmt.Sprintf("%s://%s:%d", scheme, a.Host, a.Port)] = struct{}{}
		}
	default: // "a" (and empty, which defaults to A)
		hosts, err := d.resolver.LookupHost(t.Name)
		if err != nil {
			return nil, err
		}
		for _, h := range hosts {
			urls[fmt.Sprintf("%s://%s:%d", scheme, h, t.Port)] = struct{}{}
		}
	}
	return urls, nil
}

// sync resolves the target and diffs the result against the backends this
// discoverer currently owns for that target, adding new ones and removing gone
// ones on the balancer.
func (d *Discoverer) sync(t config.DNSTarget) {
	desired, err := d.resolve(t)
	if err != nil {
		// On resolution failure, leave the current set untouched rather than
		// tearing down healthy backends.
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	current := d.owned[t.Name]
	if current == nil {
		current = make(map[string]*balancer.Backend)
		d.owned[t.Name] = current
	}

	// Add backends that are newly present.
	for url := range desired {
		if _, ok := current[url]; ok {
			continue
		}
		be := balancer.NewBackend(config.BackendConfig{
			URL:      url,
			Weight:   t.Weight,
			MaxConns: t.MaxConns,
		})
		d.b.Add(be)
		current[url] = be
	}

	// Remove backends we own that are no longer present. Iterate over a stable
	// order for deterministic behaviour.
	var gone []string
	for url := range current {
		if _, ok := desired[url]; !ok {
			gone = append(gone, url)
		}
	}
	sort.Strings(gone)
	for _, url := range gone {
		d.b.Remove(current[url])
		delete(current, url)
	}
}
