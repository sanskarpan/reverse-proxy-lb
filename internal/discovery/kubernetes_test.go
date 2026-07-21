package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/config"
)

// mockBalancer records Add/Remove calls for assertions.
type mockBalancer struct {
	mu      sync.Mutex
	added   []string // backend URLs added
	removed []string // backend URLs removed
	backends []*balancer.Backend
}

func (m *mockBalancer) Add(b *balancer.Backend) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.added = append(m.added, b.URL)
	m.backends = append(m.backends, b)
}

func (m *mockBalancer) Remove(b *balancer.Backend) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, b.URL)
	for i, be := range m.backends {
		if be == b {
			m.backends = append(m.backends[:i], m.backends[i+1:]...)
			break
		}
	}
}

func (m *mockBalancer) All() []*balancer.Backend {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*balancer.Backend, len(m.backends))
	copy(out, m.backends)
	return out
}

func (m *mockBalancer) GetHealthy() []*balancer.Backend { return m.All() }

func (m *mockBalancer) Next() (*balancer.Backend, error) { return nil, nil }

func (m *mockBalancer) UpdateWeight(b *balancer.Backend, w int) {}

func (m *mockBalancer) sortedAdded() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.added))
	copy(out, m.added)
	sort.Strings(out)
	return out
}

func (m *mockBalancer) sortedRemoved() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.removed))
	copy(out, m.removed)
	sort.Strings(out)
	return out
}

// waitFor polls f() until it returns true or the deadline is reached.
func waitFor(t *testing.T, timeout time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// makeEndpoints builds the JSON payload the mock server returns.
func makeEndpoints(rv string, subsets []k8sSubset) string {
	ep := k8sEndpoints{Subsets: subsets}
	ep.Metadata.ResourceVersion = rv
	b, _ := json.Marshal(ep)
	return string(b)
}

// makeWatchEvent builds a single NDJSON watch-event line.
func makeWatchEvent(eventType string, rv string, subsets []k8sSubset) string {
	ep := k8sEndpoints{Subsets: subsets}
	ep.Metadata.ResourceVersion = rv
	ev := k8sWatchEvent{Type: eventType, Object: ep}
	b, _ := json.Marshal(ev)
	return string(b)
}

// newKDFromTestServer creates a KubernetesDiscovery that talks to srv.
// caCert is left empty; buildHTTPClient will skip TLS verification for test servers.
func newKDFromTestServer(b balancer.Balancer, srv *httptest.Server, portName string) *KubernetesDiscovery { //nolint:unparam
	return &KubernetesDiscovery{
		namespace: "default",
		service:   "my-svc",
		portName:  portName,
		token:     "test-token",
		apiServer: srv.URL,
		caCert:    nil, // InsecureSkipVerify for plain HTTP test servers
		resync:    30 * time.Second,
		balancer:  b,
		scheme:    "http",
		stopCh:    make(chan struct{}),
		owned:     make(map[string]*balancer.Backend),
	}
}

// TestKubernetesDiscovery_InitialSync: mock returns endpoints with 2 addresses;
// assert balancer.Add called twice.
func TestKubernetesDiscovery_InitialSync(t *testing.T) {
	subsets := []k8sSubset{
		{
			Addresses: []k8sAddress{{IP: "10.0.0.1"}, {IP: "10.0.0.2"}},
			Ports:     []k8sPort{{Name: "http", Port: 8080}},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only serve the initial GET; watch hangs.
		if r.URL.Query().Get("watch") == "true" {
			// Block until the client disconnects.
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, makeEndpoints("100", subsets))
	}))
	defer srv.Close()

	mb := &mockBalancer{}
	kd := newKDFromTestServer(mb, srv, "http")

	// Only test listAndSync directly to avoid the background goroutine complexity.
	client, err := kd.buildHTTPClient()
	if err != nil {
		t.Fatalf("buildHTTPClient: %v", err)
	}
	rv, err := kd.listAndSync(context.Background(), client)
	if err != nil {
		t.Fatalf("listAndSync: %v", err)
	}
	if rv != "100" {
		t.Errorf("resourceVersion = %q, want %q", rv, "100")
	}

	got := mb.sortedAdded()
	want := []string{"http://10.0.0.1:8080", "http://10.0.0.2:8080"}
	if len(got) != len(want) {
		t.Fatalf("Add called %d times, want %d; urls=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("added[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestKubernetesDiscovery_WatchAdded: mock streams an ADDED event; assert balancer.Add called.
func TestKubernetesDiscovery_WatchAdded(t *testing.T) {
	// Start with empty endpoints, then stream an ADDED event with one address.
	addedSubsets := []k8sSubset{
		{
			Addresses: []k8sAddress{{IP: "10.1.0.5"}},
			Ports:     []k8sPort{{Name: "http", Port: 9090}},
		},
	}

	// The watch response: one NDJSON line then close.
	watchLine := makeWatchEvent("ADDED", "101", addedSubsets)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") == "true" {
			w.Header().Set("Content-Type", "application/json")
			// Flush so the scanner reads the line, then end the response.
			fmt.Fprintln(w, watchLine)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		// Initial GET: empty endpoints with resourceVersion.
		fmt.Fprint(w, makeEndpoints("100", nil))
	}))
	defer srv.Close()

	mb := &mockBalancer{}
	kd := newKDFromTestServer(mb, srv, "http")

	client, err := kd.buildHTTPClient()
	if err != nil {
		t.Fatalf("buildHTTPClient: %v", err)
	}

	// Initial sync (empty endpoints).
	rv, err := kd.listAndSync(context.Background(), client)
	if err != nil {
		t.Fatalf("listAndSync: %v", err)
	}

	// Stream watch events.
	_, err = kd.watchStream(context.Background(), client, rv)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	got := mb.sortedAdded()
	want := []string{"http://10.1.0.5:9090"}
	if len(got) != len(want) {
		t.Fatalf("Add called %d times, want %d; urls=%v", len(got), len(want), got)
	}
	if got[0] != want[0] {
		t.Errorf("added[0] = %q, want %q", got[0], want[0])
	}
}

// TestKubernetesDiscovery_WatchDeleted: mock streams DELETED event; assert balancer.Remove called.
func TestKubernetesDiscovery_WatchDeleted(t *testing.T) {
	initialSubsets := []k8sSubset{
		{
			Addresses: []k8sAddress{{IP: "10.2.0.1"}},
			Ports:     []k8sPort{{Name: "http", Port: 8080}},
		},
	}
	deletedSubsets := []k8sSubset{
		{
			Addresses: []k8sAddress{{IP: "10.2.0.1"}},
			Ports:     []k8sPort{{Name: "http", Port: 8080}},
		},
	}

	watchLine := makeWatchEvent("DELETED", "102", deletedSubsets)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") == "true" {
			fmt.Fprintln(w, watchLine)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		fmt.Fprint(w, makeEndpoints("101", initialSubsets))
	}))
	defer srv.Close()

	mb := &mockBalancer{}
	kd := newKDFromTestServer(mb, srv, "http")

	client, err := kd.buildHTTPClient()
	if err != nil {
		t.Fatalf("buildHTTPClient: %v", err)
	}

	// Initial sync: should add the backend.
	rv, err := kd.listAndSync(context.Background(), client)
	if err != nil {
		t.Fatalf("listAndSync: %v", err)
	}
	if len(mb.sortedAdded()) != 1 {
		t.Fatalf("expected 1 Add after initial sync, got %v", mb.sortedAdded())
	}

	// Watch: DELETED event should trigger removeAll -> Remove.
	_, err = kd.watchStream(context.Background(), client, rv)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	got := mb.sortedRemoved()
	if len(got) != 1 {
		t.Fatalf("Remove called %d times, want 1; urls=%v", len(got), got)
	}
	if got[0] != "http://10.2.0.1:8080" {
		t.Errorf("removed[0] = %q, want %q", got[0], "http://10.2.0.1:8080")
	}
}

// TestKubernetesDiscovery_ReadinessFilter: NotReadyAddresses are NOT added to balancer.
func TestKubernetesDiscovery_ReadinessFilter(t *testing.T) {
	// One address in Addresses (ready), two in NotReadyAddresses (not ready).
	subsets := []k8sSubset{
		{
			Addresses:         []k8sAddress{{IP: "10.3.0.1"}},
			NotReadyAddresses: []k8sAddress{{IP: "10.3.0.2"}, {IP: "10.3.0.3"}},
			Ports:             []k8sPort{{Name: "http", Port: 8080}},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") == "true" {
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, makeEndpoints("200", subsets))
	}))
	defer srv.Close()

	mb := &mockBalancer{}
	kd := newKDFromTestServer(mb, srv, "http")

	client, err := kd.buildHTTPClient()
	if err != nil {
		t.Fatalf("buildHTTPClient: %v", err)
	}

	_, err = kd.listAndSync(context.Background(), client)
	if err != nil {
		t.Fatalf("listAndSync: %v", err)
	}

	got := mb.sortedAdded()
	// Only the one ready address should be added.
	if len(got) != 1 {
		t.Fatalf("Add called %d times, want 1 (only ready address); urls=%v", len(got), got)
	}
	if got[0] != "http://10.3.0.1:8080" {
		t.Errorf("added[0] = %q, want %q", got[0], "http://10.3.0.1:8080")
	}
}

// TestKubernetesDiscovery_PortNameFilter: only ports matching portName are added.
func TestKubernetesDiscovery_PortNameFilter(t *testing.T) {
	subsets := []k8sSubset{
		{
			Addresses: []k8sAddress{{IP: "10.4.0.1"}},
			Ports: []k8sPort{
				{Name: "http", Port: 8080},
				{Name: "metrics", Port: 9090},
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") == "true" {
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, makeEndpoints("300", subsets))
	}))
	defer srv.Close()

	mb := &mockBalancer{}
	// Only want "metrics" port.
	kd := newKDFromTestServer(mb, srv, "metrics")

	client, err := kd.buildHTTPClient()
	if err != nil {
		t.Fatalf("buildHTTPClient: %v", err)
	}

	_, err = kd.listAndSync(context.Background(), client)
	if err != nil {
		t.Fatalf("listAndSync: %v", err)
	}

	got := mb.sortedAdded()
	if len(got) != 1 {
		t.Fatalf("Add called %d times, want 1; urls=%v", len(got), got)
	}
	if got[0] != "http://10.4.0.1:9090" {
		t.Errorf("added[0] = %q, want %q", got[0], "http://10.4.0.1:9090")
	}
}

// TestKubernetesDiscovery_StartStop: goroutine lifecycle is clean.
func TestKubernetesDiscovery_StartStop(t *testing.T) {
	subsets := []k8sSubset{
		{
			Addresses: []k8sAddress{{IP: "10.5.0.1"}},
			Ports:     []k8sPort{{Port: 8080}},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") == "true" {
			// Keep stream open until client cancels.
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, makeEndpoints("400", subsets))
	}))
	defer srv.Close()

	mb := &mockBalancer{}
	kd := newKDFromTestServer(mb, srv, "")

	kd.Start()

	// Wait for initial sync to land.
	waitFor(t, 2*time.Second, func() bool {
		return len(mb.sortedAdded()) >= 1
	})

	kd.Stop()
	// Stop is idempotent.
	kd.Stop()

	if got := mb.sortedAdded(); len(got) == 0 {
		t.Error("expected at least one Add before Stop")
	}
}

// TestKubernetesDiscovery_NewFromConfig_MissingCredentials: NewKubernetesDiscovery
// returns an error when in-cluster credentials are absent and no kubeconfig is set.
func TestKubernetesDiscovery_NewFromConfig_MissingCredentials(t *testing.T) {
	mb := &mockBalancer{}
	cfg := config.KubernetesDiscoveryConfig{
		Enabled:   true,
		Namespace: "default",
		Service:   "my-svc",
		// No Kubeconfig and no in-cluster environment.
	}
	_, err := NewKubernetesDiscovery(cfg, mb)
	// In CI/test environments, there are no ServiceAccount files; we expect an error.
	if err == nil {
		t.Skip("in-cluster credentials found in test environment; skipping absence test")
	}
}

// TestKubernetesDiscovery_WatchModified: MODIFIED event re-syncs the endpoint set.
func TestKubernetesDiscovery_WatchModified(t *testing.T) {
	initialSubsets := []k8sSubset{
		{
			Addresses: []k8sAddress{{IP: "10.6.0.1"}},
			Ports:     []k8sPort{{Port: 8080}},
		},
	}
	// MODIFIED event: address .1 replaced by .2
	modifiedSubsets := []k8sSubset{
		{
			Addresses: []k8sAddress{{IP: "10.6.0.2"}},
			Ports:     []k8sPort{{Port: 8080}},
		},
	}

	watchLine := makeWatchEvent("MODIFIED", "501", modifiedSubsets)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") == "true" {
			fmt.Fprintln(w, watchLine)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		fmt.Fprint(w, makeEndpoints("500", initialSubsets))
	}))
	defer srv.Close()

	mb := &mockBalancer{}
	kd := newKDFromTestServer(mb, srv, "")

	client, err := kd.buildHTTPClient()
	if err != nil {
		t.Fatalf("buildHTTPClient: %v", err)
	}

	rv, err := kd.listAndSync(context.Background(), client)
	if err != nil {
		t.Fatalf("listAndSync: %v", err)
	}

	_, err = kd.watchStream(context.Background(), client, rv)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	added := mb.sortedAdded()
	removed := mb.sortedRemoved()

	// .1 added initially, .2 added on MODIFIED, .1 removed on MODIFIED.
	if len(added) < 2 {
		t.Errorf("expected >=2 Add calls (initial + modified), got %v", added)
	}
	if len(removed) < 1 {
		t.Errorf("expected >=1 Remove call (stale backend), got %v", removed)
	}
}
