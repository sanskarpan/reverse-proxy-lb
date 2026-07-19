package discovery

import (
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/config"
)

// fakeResolver is a programmable Resolver for tests. Its return values can be
// swapped concurrently while a Discoverer runs.
type fakeResolver struct {
	mu    sync.Mutex
	hosts map[string][]string
	srvs  map[string][]Addr
	err   error
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{
		hosts: make(map[string][]string),
		srvs:  make(map[string][]Addr),
	}
}

func (f *fakeResolver) setHosts(name string, hosts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hosts[name] = hosts
}

func (f *fakeResolver) setSRV(name string, addrs []Addr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.srvs[name] = addrs
}

func (f *fakeResolver) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeResolver) LookupHost(name string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]string(nil), f.hosts[name]...), nil
}

func (f *fakeResolver) LookupSRV(service string) ([]Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]Addr(nil), f.srvs[service]...), nil
}

// testBalancer satisfies balancer.Balancer for tests by embedding BaseBalancer
// (which provides Add/Remove/All/GetHealthy/UpdateWeight) and stubbing Next,
// which the discoverer never calls.
type testBalancer struct {
	balancer.BaseBalancer
}

func (t *testBalancer) Next() (*balancer.Backend, error) { return nil, nil }

// urlSet returns the sorted set of backend URLs currently in the balancer.
func urlSet(b balancer.Balancer) []string {
	all := b.All()
	urls := make([]string, 0, len(all))
	for _, be := range all {
		urls = append(urls, be.URL)
	}
	sort.Strings(urls)
	return urls
}

func equalURLs(a, b []string) bool {
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

// TestSyncInitialA verifies an initial resolve of an A target adds one backend
// per host, using the target port and scheme.
func TestSyncInitialA(t *testing.T) {
	b := &testBalancer{}
	fr := newFakeResolver()
	fr.setHosts("web.svc", []string{"10.0.0.1", "10.0.0.2"})

	target := config.DNSTarget{
		Name:     "web.svc",
		Type:     "a",
		Scheme:   "http",
		Port:     8080,
		Interval: time.Hour,
		Weight:   3,
		MaxConns: 50,
	}
	d := NewDiscoverer(b, []config.DNSTarget{target}, fr)
	d.sync(target)

	want := []string{"http://10.0.0.1:8080", "http://10.0.0.2:8080"}
	if got := urlSet(b); !equalURLs(got, want) {
		t.Fatalf("urls = %v, want %v", got, want)
	}
	// Weight/MaxConns propagate from the target.
	for _, be := range b.All() {
		if be.GetWeight() != 3 || be.MaxConns != 50 {
			t.Fatalf("backend %s: weight=%d maxconns=%d, want 3/50", be.URL, be.GetWeight(), be.MaxConns)
		}
	}
}

// TestSyncInitialSRV verifies SRV mode uses the resolved host and port.
func TestSyncInitialSRV(t *testing.T) {
	b := &testBalancer{}
	fr := newFakeResolver()
	fr.setSRV("api.svc", []Addr{
		{Host: "node1.internal", Port: 9000},
		{Host: "node2.internal", Port: 9001},
	})

	target := config.DNSTarget{
		Name:     "api.svc",
		Type:     "srv",
		Scheme:   "https",
		Interval: time.Hour,
		Weight:   1,
		MaxConns: 100,
	}
	d := NewDiscoverer(b, []config.DNSTarget{target}, fr)
	d.sync(target)

	want := []string{"https://node1.internal:9000", "https://node2.internal:9001"}
	if got := urlSet(b); !equalURLs(got, want) {
		t.Fatalf("urls = %v, want %v", got, want)
	}
}

// TestSyncDiffAddRemove verifies a changed resolution adds new backends and
// removes gone ones.
func TestSyncDiffAddRemove(t *testing.T) {
	b := &testBalancer{}
	fr := newFakeResolver()
	fr.setHosts("web.svc", []string{"10.0.0.1", "10.0.0.2"})

	target := config.DNSTarget{
		Name:     "web.svc",
		Type:     "a",
		Scheme:   "http",
		Port:     80,
		Interval: time.Hour,
		Weight:   1,
		MaxConns: 100,
	}
	d := NewDiscoverer(b, []config.DNSTarget{target}, fr)
	d.sync(target)

	// Keep .2, drop .1, add .3.
	fr.setHosts("web.svc", []string{"10.0.0.2", "10.0.0.3"})
	d.sync(target)

	want := []string{"http://10.0.0.2:80", "http://10.0.0.3:80"}
	if got := urlSet(b); !equalURLs(got, want) {
		t.Fatalf("urls = %v, want %v", got, want)
	}
}

// TestNeverRemovesStatic verifies the discoverer never removes a backend it did
// not create, even when the backend disappears from DNS.
func TestNeverRemovesStatic(t *testing.T) {
	b := &testBalancer{}
	static := balancer.NewBackend(config.BackendConfig{URL: "http://static:80", Weight: 1, MaxConns: 100})
	b.Add(static)

	fr := newFakeResolver()
	fr.setHosts("web.svc", []string{"10.0.0.1"})
	target := config.DNSTarget{
		Name: "web.svc", Type: "a", Scheme: "http", Port: 80,
		Interval: time.Hour, Weight: 1, MaxConns: 100,
	}
	d := NewDiscoverer(b, []config.DNSTarget{target}, fr)
	d.sync(target)

	// DNS now returns nothing; the discovered backend goes away but static
	// must remain.
	fr.setHosts("web.svc", nil)
	d.sync(target)

	want := []string{"http://static:80"}
	if got := urlSet(b); !equalURLs(got, want) {
		t.Fatalf("urls = %v, want %v", got, want)
	}
}

// TestSyncErrorLeavesSetUntouched verifies a resolution error does not tear down
// existing discovered backends.
func TestSyncErrorLeavesSetUntouched(t *testing.T) {
	b := &testBalancer{}
	fr := newFakeResolver()
	fr.setHosts("web.svc", []string{"10.0.0.1"})
	target := config.DNSTarget{
		Name: "web.svc", Type: "a", Scheme: "http", Port: 80,
		Interval: time.Hour, Weight: 1, MaxConns: 100,
	}
	d := NewDiscoverer(b, []config.DNSTarget{target}, fr)
	d.sync(target)

	fr.setErr(errors.New("dns down"))
	d.sync(target)

	want := []string{"http://10.0.0.1:80"}
	if got := urlSet(b); !equalURLs(got, want) {
		t.Fatalf("urls = %v, want %v", got, want)
	}
}

// TestOnlyManagesOwnTargets verifies one target never removes another target's
// backends even if their URLs would collide by host.
func TestOnlyManagesOwnTargets(t *testing.T) {
	b := &testBalancer{}
	fr := newFakeResolver()
	fr.setHosts("a.svc", []string{"10.0.0.1"})
	fr.setHosts("b.svc", []string{"10.0.0.9"})

	ta := config.DNSTarget{Name: "a.svc", Type: "a", Scheme: "http", Port: 80, Interval: time.Hour, Weight: 1, MaxConns: 100}
	tb := config.DNSTarget{Name: "b.svc", Type: "a", Scheme: "http", Port: 80, Interval: time.Hour, Weight: 1, MaxConns: 100}
	d := NewDiscoverer(b, []config.DNSTarget{ta, tb}, fr)
	d.sync(ta)
	d.sync(tb)

	// a.svc empties; b.svc backend must remain.
	fr.setHosts("a.svc", nil)
	d.sync(ta)

	want := []string{"http://10.0.0.9:80"}
	if got := urlSet(b); !equalURLs(got, want) {
		t.Fatalf("urls = %v, want %v", got, want)
	}
}

// TestStartStop exercises the goroutine lifecycle: Start resolves immediately
// and Stop is clean (no leaked goroutines, idempotent).
func TestStartStop(t *testing.T) {
	b := &testBalancer{}
	fr := newFakeResolver()
	fr.setHosts("web.svc", []string{"10.0.0.1"})
	target := config.DNSTarget{
		Name: "web.svc", Type: "a", Scheme: "http", Port: 80,
		Interval: 5 * time.Millisecond, Weight: 1, MaxConns: 100,
	}
	d := NewDiscoverer(b, []config.DNSTarget{target}, fr)
	d.Start()

	// Wait for the immediate resolve to land.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(b.All()) == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := urlSet(b); !equalURLs(got, []string{"http://10.0.0.1:80"}) {
		t.Fatalf("after Start urls = %v", got)
	}

	d.Stop()
	// Stop is idempotent.
	d.Stop()
}

// TestNilResolverUsesDefault verifies passing a nil resolver falls back to the
// stdlib implementation without panicking.
func TestNilResolverUsesDefault(t *testing.T) {
	b := &testBalancer{}
	d := NewDiscoverer(b, nil, nil)
	if d.resolver == nil {
		t.Fatal("expected default resolver, got nil")
	}
}
