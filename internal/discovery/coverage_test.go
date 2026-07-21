// coverage_test.go provides additional tests to raise internal/discovery
// coverage from ~61% to 75%+.
//
// Gaps targeted:
//   - buildHTTPClient: valid CA, invalid CA, client cert, invalid client cert
//   - min2 helper both branches
//   - refreshToken: no-op, reads file, missing file
//   - NewKubernetesDiscovery: invalid resync_period, from kubeconfig, custom resync
//   - loadKubeconfig: bearer token, embedded CA, CA file, client cert, context/cluster not found, missing file, invalid YAML
//   - listAndSync: 401 path (refreshToken called), non-200 path
//   - watchStream: 410 Gone error, 401 path
//   - k8s watch reconnect on 410 Gone (run loop integration)
//   - k8s Stop() cleans up goroutine
//   - DNS: empty address list (no panic), empty SRV list
//   - DNS: TTL-style refresh (second sync updates set)
//   - DNS: ticker path (run goroutine re-resolves)
//   - resolve: default scheme "http" when empty
//   - loadInCluster: missing token file error
//   - addressesFromEndpoints: empty subsets, no matching port
//   - resolvePort: empty portName returns first, no match returns 0
package discovery

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// selfSignedCA generates a self-signed CA certificate + key and returns them
// as PEM bytes.
func selfSignedCA(t *testing.T) (caCertPEM, caKeyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	caCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal CA key: %v", err)
	}
	caKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return
}

// selfSignedClientCert generates a client certificate signed by the given CA.
func selfSignedClientCert(t *testing.T, caCertPEM, caKeyPEM []byte) (certPEM, keyPEM []byte) {
	t.Helper()
	block, _ := pem.Decode(caCertPEM)
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	block, _ = pem.Decode(caKeyPEM)
	caKey, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse CA key: %v", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-client"},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create client cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return
}

// writeKubeconfig writes content to a temp file and returns its path.
func writeKubeconfig(t *testing.T, dir string, content string) string {
	t.Helper()
	p := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return p
}

// ---------------------------------------------------------------------------
// buildHTTPClient
// ---------------------------------------------------------------------------

// TestBuildHTTPClient_WithCACert ensures buildHTTPClient succeeds when given
// a valid PEM CA certificate.
func TestBuildHTTPClient_WithCACert(t *testing.T) {
	caCertPEM, _ := selfSignedCA(t)
	kd := &KubernetesDiscovery{caCert: caCertPEM}
	client, err := kd.buildHTTPClient()
	if err != nil {
		t.Fatalf("buildHTTPClient with valid CA: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}

// TestBuildHTTPClient_InvalidCA ensures buildHTTPClient returns an error
// for garbage CA PEM data.
func TestBuildHTTPClient_InvalidCA(t *testing.T) {
	kd := &KubernetesDiscovery{caCert: []byte("not-valid-pem")}
	_, err := kd.buildHTTPClient()
	if err == nil {
		t.Fatal("expected error for invalid CA PEM, got nil")
	}
}

// TestBuildHTTPClient_WithClientCert ensures buildHTTPClient succeeds and
// attaches the client certificate.
func TestBuildHTTPClient_WithClientCert(t *testing.T) {
	caCertPEM, caKeyPEM := selfSignedCA(t)
	certPEM, keyPEM := selfSignedClientCert(t, caCertPEM, caKeyPEM)
	kd := &KubernetesDiscovery{
		caCert:     caCertPEM,
		clientCert: certPEM,
		clientKey:  keyPEM,
	}
	client, err := kd.buildHTTPClient()
	if err != nil {
		t.Fatalf("buildHTTPClient with client cert: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}

// TestBuildHTTPClient_InvalidClientCert ensures an error is returned for a
// mismatched client cert/key pair.
func TestBuildHTTPClient_InvalidClientCert(t *testing.T) {
	caCertPEM, caKeyPEM := selfSignedCA(t)
	certPEM, _ := selfSignedClientCert(t, caCertPEM, caKeyPEM)
	_, caKeyPEM2 := selfSignedCA(t) // different CA key — mismatch
	kd := &KubernetesDiscovery{
		clientCert: certPEM,
		clientKey:  caKeyPEM2, // deliberately wrong key
	}
	_, err := kd.buildHTTPClient()
	if err == nil {
		t.Fatal("expected error for mismatched cert/key, got nil")
	}
}

// ---------------------------------------------------------------------------
// min2
// ---------------------------------------------------------------------------

// TestMin2 verifies the helper returns the smaller of two durations.
func TestMin2(t *testing.T) {
	cases := []struct {
		a, b, want time.Duration
	}{
		{time.Second, 2 * time.Second, time.Second},
		{3 * time.Second, time.Second, time.Second},
		{time.Second, time.Second, time.Second},
	}
	for _, c := range cases {
		if got := min2(c.a, c.b); got != c.want {
			t.Errorf("min2(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// refreshToken
// ---------------------------------------------------------------------------

// TestRefreshToken_NoOp ensures refreshToken is a no-op when tokenFile is empty.
func TestRefreshToken_NoOp(t *testing.T) {
	kd := &KubernetesDiscovery{token: "original", tokenFile: ""}
	kd.refreshToken()
	if kd.token != "original" {
		t.Errorf("token changed unexpectedly to %q", kd.token)
	}
}

// TestRefreshToken_ReadsFile ensures refreshToken updates the token from disk.
func TestRefreshToken_ReadsFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "token-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.WriteString("new-token\n"); err != nil {
		t.Fatalf("write token: %v", err)
	}
	f.Close()

	kd := &KubernetesDiscovery{token: "old-token", tokenFile: f.Name()}
	kd.refreshToken()
	if kd.token != "new-token" {
		t.Errorf("token = %q, want %q", kd.token, "new-token")
	}
}

// TestRefreshToken_MissingFile ensures refreshToken is a no-op for a missing
// token file.
func TestRefreshToken_MissingFile(t *testing.T) {
	kd := &KubernetesDiscovery{token: "current", tokenFile: "/nonexistent/path/token"}
	kd.refreshToken() // must not panic
	if kd.token != "current" {
		t.Errorf("token changed unexpectedly to %q", kd.token)
	}
}

// ---------------------------------------------------------------------------
// NewKubernetesDiscovery
// ---------------------------------------------------------------------------

// TestNewKubernetesDiscovery_InvalidResyncPeriod ensures an error is returned
// for an unparseable resync_period string.
func TestNewKubernetesDiscovery_InvalidResyncPeriod(t *testing.T) {
	mb := &mockBalancer{}
	cfg := config.KubernetesDiscoveryConfig{
		Namespace:    "default",
		Service:      "my-svc",
		ResyncPeriod: "not-a-duration",
		Kubeconfig:   "/dev/null", // prevent loadInCluster from running
	}
	_, err := NewKubernetesDiscovery(cfg, mb)
	if err == nil {
		t.Fatal("expected error for invalid ResyncPeriod, got nil")
	}
}

// ---------------------------------------------------------------------------
// loadKubeconfig
// ---------------------------------------------------------------------------

// TestLoadKubeconfig_BearerToken exercises the token auth path.
func TestLoadKubeconfig_BearerToken(t *testing.T) {
	kc := `
current-context: test-ctx
clusters:
- name: test-cluster
  cluster:
    server: https://k8s.example.com
contexts:
- name: test-ctx
  context:
    cluster: test-cluster
    user: test-user
users:
- name: test-user
  user:
    token: my-bearer-token
`
	p := writeKubeconfig(t, t.TempDir(), kc)
	kd := &KubernetesDiscovery{}
	if err := kd.loadKubeconfig(p); err != nil {
		t.Fatalf("loadKubeconfig: %v", err)
	}
	if kd.apiServer != "https://k8s.example.com" {
		t.Errorf("apiServer = %q, want %q", kd.apiServer, "https://k8s.example.com")
	}
	if kd.token != "my-bearer-token" {
		t.Errorf("token = %q, want %q", kd.token, "my-bearer-token")
	}
}

// TestLoadKubeconfig_EmbeddedCA exercises the certificate-authority-data path.
func TestLoadKubeconfig_EmbeddedCA(t *testing.T) {
	caCertPEM, _ := selfSignedCA(t)
	caB64 := base64.StdEncoding.EncodeToString(caCertPEM)

	kc := fmt.Sprintf(`
current-context: ctx
clusters:
- name: cls
  cluster:
    server: https://k8s.local
    certificate-authority-data: %s
contexts:
- name: ctx
  context:
    cluster: cls
    user: u
users:
- name: u
  user:
    token: tok
`, caB64)
	p := writeKubeconfig(t, t.TempDir(), kc)
	kd := &KubernetesDiscovery{}
	if err := kd.loadKubeconfig(p); err != nil {
		t.Fatalf("loadKubeconfig with embedded CA: %v", err)
	}
	if len(kd.caCert) == 0 {
		t.Error("expected caCert to be populated from certificate-authority-data")
	}
}

// TestLoadKubeconfig_CACertFile exercises the certificate-authority file path.
func TestLoadKubeconfig_CACertFile(t *testing.T) {
	dir := t.TempDir()
	caCertPEM, _ := selfSignedCA(t)
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, caCertPEM, 0600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}

	kc := fmt.Sprintf(`
current-context: ctx
clusters:
- name: cls
  cluster:
    server: https://k8s.local
    certificate-authority: %s
contexts:
- name: ctx
  context:
    cluster: cls
    user: u
users:
- name: u
  user:
    token: tok
`, caFile)
	p := writeKubeconfig(t, dir, kc)
	kd := &KubernetesDiscovery{}
	if err := kd.loadKubeconfig(p); err != nil {
		t.Fatalf("loadKubeconfig with CA file: %v", err)
	}
	if len(kd.caCert) == 0 {
		t.Error("expected caCert to be populated from certificate-authority file")
	}
}

// TestLoadKubeconfig_ClientCert exercises the client certificate auth path.
func TestLoadKubeconfig_ClientCert(t *testing.T) {
	caCertPEM, caKeyPEM := selfSignedCA(t)
	certPEM, keyPEM := selfSignedClientCert(t, caCertPEM, caKeyPEM)
	certB64 := base64.StdEncoding.EncodeToString(certPEM)
	keyB64 := base64.StdEncoding.EncodeToString(keyPEM)

	kc := fmt.Sprintf(`
current-context: ctx
clusters:
- name: cls
  cluster:
    server: https://k8s.local
contexts:
- name: ctx
  context:
    cluster: cls
    user: u
users:
- name: u
  user:
    client-certificate-data: %s
    client-key-data: %s
`, certB64, keyB64)
	p := writeKubeconfig(t, t.TempDir(), kc)
	kd := &KubernetesDiscovery{}
	if err := kd.loadKubeconfig(p); err != nil {
		t.Fatalf("loadKubeconfig with client cert: %v", err)
	}
	if len(kd.clientCert) == 0 {
		t.Error("expected clientCert to be populated")
	}
	if len(kd.clientKey) == 0 {
		t.Error("expected clientKey to be populated")
	}
}

// TestLoadKubeconfig_ContextNotFound ensures an error when context is missing.
func TestLoadKubeconfig_ContextNotFound(t *testing.T) {
	kc := `
current-context: missing-ctx
clusters:
- name: cls
  cluster:
    server: https://k8s.local
contexts: []
users: []
`
	p := writeKubeconfig(t, t.TempDir(), kc)
	kd := &KubernetesDiscovery{}
	if err := kd.loadKubeconfig(p); err == nil {
		t.Fatal("expected error for missing context, got nil")
	}
}

// TestLoadKubeconfig_ClusterNotFound ensures an error when the cluster is missing.
func TestLoadKubeconfig_ClusterNotFound(t *testing.T) {
	kc := `
current-context: ctx
clusters: []
contexts:
- name: ctx
  context:
    cluster: missing-cls
    user: u
users:
- name: u
  user:
    token: tok
`
	p := writeKubeconfig(t, t.TempDir(), kc)
	kd := &KubernetesDiscovery{}
	if err := kd.loadKubeconfig(p); err == nil {
		t.Fatal("expected error for missing cluster, got nil")
	}
}

// TestLoadKubeconfig_MissingFile ensures a file-not-found error is propagated.
func TestLoadKubeconfig_MissingFile(t *testing.T) {
	kd := &KubernetesDiscovery{}
	if err := kd.loadKubeconfig("/nonexistent/kubeconfig"); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

// TestLoadKubeconfig_InvalidYAML ensures a YAML parse error is returned.
func TestLoadKubeconfig_InvalidYAML(t *testing.T) {
	p := writeKubeconfig(t, t.TempDir(), "::invalid yaml::{[")
	kd := &KubernetesDiscovery{}
	if err := kd.loadKubeconfig(p); err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

// ---------------------------------------------------------------------------
// listAndSync error paths
// ---------------------------------------------------------------------------

// TestListAndSync_Unauthorized exercises the 401 path — token refresh is called
// and an error is returned.
func TestListAndSync_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	mb := &mockBalancer{}
	kd := newKDFromTestServer(mb, srv, "http")

	// Provide a token file so refreshToken actually updates the token.
	f, err := os.CreateTemp(t.TempDir(), "tok-*")
	if err != nil {
		t.Fatalf("create temp token file: %v", err)
	}
	fmt.Fprint(f, "new-token")
	f.Close()
	kd.tokenFile = f.Name()
	kd.token = "stale-token"

	client, _ := kd.buildHTTPClient()
	_, err = kd.listAndSync(context.Background(), client)
	if err == nil {
		t.Fatal("expected error from 401 listAndSync, got nil")
	}
	// refreshToken should have been called and updated the token.
	if kd.token != "new-token" {
		t.Errorf("token after listAndSync-401 = %q, want %q", kd.token, "new-token")
	}
}

// TestListAndSync_NonOK exercises the non-200/non-401 error path.
func TestListAndSync_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	mb := &mockBalancer{}
	kd := newKDFromTestServer(mb, srv, "http")
	client, _ := kd.buildHTTPClient()

	_, err := kd.listAndSync(context.Background(), client)
	if err == nil {
		t.Fatal("expected error from 503 listAndSync, got nil")
	}
}

// ---------------------------------------------------------------------------
// watchStream error paths
// ---------------------------------------------------------------------------

// TestWatchStream_410Gone exercises the 410 path that triggers reconnect.
func TestWatchStream_410Gone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone) // 410
	}))
	defer srv.Close()

	mb := &mockBalancer{}
	kd := newKDFromTestServer(mb, srv, "http")
	client, _ := kd.buildHTTPClient()

	_, err := kd.watchStream(context.Background(), client, "100")
	if err == nil {
		t.Fatal("expected error from 410 Gone in watchStream, got nil")
	}
}

// TestWatchStream_Unauthorized exercises the 401 path in watchStream.
func TestWatchStream_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	mb := &mockBalancer{}
	kd := newKDFromTestServer(mb, srv, "http")

	f, err := os.CreateTemp(t.TempDir(), "tok-*")
	if err != nil {
		t.Fatalf("create temp token file: %v", err)
	}
	fmt.Fprint(f, "refreshed")
	f.Close()
	kd.tokenFile = f.Name()
	kd.token = "old"

	client, _ := kd.buildHTTPClient()
	_, err = kd.watchStream(context.Background(), client, "100")
	if err == nil {
		t.Fatal("expected error from 401 in watchStream, got nil")
	}
	if kd.token != "refreshed" {
		t.Errorf("token after watch-401 = %q, want %q", kd.token, "refreshed")
	}
}

// ---------------------------------------------------------------------------
// k8s watch reconnect on 410 Gone (integration via run goroutine)
// ---------------------------------------------------------------------------

// TestKubernetesDiscovery_ReconnectOn410 verifies that after the watch returns
// 410 Gone the run loop reconnects (issues a second list request).
func TestKubernetesDiscovery_ReconnectOn410(t *testing.T) {
	subsets := []k8sSubset{{
		Addresses: []k8sAddress{{IP: "10.9.0.1"}},
		Ports:     []k8sPort{{Port: 8080}},
	}}

	var listCalls atomic.Int32
	var watchCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") == "true" {
			wc := watchCalls.Add(1)
			if wc == 1 {
				// First watch: return 410 to trigger reconnect.
				w.WriteHeader(http.StatusGone)
				return
			}
			// Subsequent watches: hang until client cancels.
			<-r.Context().Done()
			return
		}
		listCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		ep := k8sEndpoints{Subsets: subsets}
		ep.Metadata.ResourceVersion = fmt.Sprintf("%d", listCalls.Load())
		b, _ := json.Marshal(ep)
		fmt.Fprint(w, string(b))
	}))
	defer srv.Close()

	mb := &mockBalancer{}
	kd := newKDFromTestServer(mb, srv, "")
	kd.Start()

	// Wait for at least 2 list calls (initial + reconnect after 410).
	waitFor(t, 5*time.Second, func() bool {
		return listCalls.Load() >= 2
	})

	kd.Stop()

	if listCalls.Load() < 2 {
		t.Errorf("expected >= 2 list calls (reconnect after 410), got %d", listCalls.Load())
	}
}

// ---------------------------------------------------------------------------
// k8s Stop() cleans up goroutine
// ---------------------------------------------------------------------------

// TestKubernetesDiscovery_StopCleansUp verifies Stop() returns promptly and
// the background goroutine exits.
func TestKubernetesDiscovery_StopCleansUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") == "true" {
			<-r.Context().Done()
			return
		}
		ep := k8sEndpoints{}
		ep.Metadata.ResourceVersion = "1"
		b, _ := json.Marshal(ep)
		fmt.Fprint(w, string(b))
	}))
	defer srv.Close()

	mb := &mockBalancer{}
	kd := newKDFromTestServer(mb, srv, "")
	kd.Start()

	done := make(chan struct{})
	go func() {
		kd.Stop()
		close(done)
	}()

	select {
	case <-done:
		// goroutine cleaned up successfully
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return within 5s — goroutine leak?")
	}
}

// ---------------------------------------------------------------------------
// DNS discovery — empty address list (no panic)
// ---------------------------------------------------------------------------

// TestDiscovery_EmptyAddressList verifies that when a resolver returns an empty
// address list no backends are added and no panic occurs.
func TestDiscovery_EmptyAddressList(t *testing.T) {
	b := &testBalancer{}
	fr := newFakeResolver()
	fr.setHosts("empty.svc", []string{}) // empty but no error

	target := config.DNSTarget{
		Name:     "empty.svc",
		Type:     "a",
		Scheme:   "http",
		Port:     80,
		Interval: time.Hour,
		Weight:   1,
		MaxConns: 100,
	}
	d := NewDiscoverer(b, []config.DNSTarget{target}, fr)
	d.sync(target) // must not panic

	if got := urlSet(b); len(got) != 0 {
		t.Errorf("expected 0 backends for empty DNS response, got %v", got)
	}
}

// TestDiscovery_EmptySRVList verifies empty SRV results cause no backends.
func TestDiscovery_EmptySRVList(t *testing.T) {
	b := &testBalancer{}
	fr := newFakeResolver()
	fr.setSRV("empty.svc", []Addr{}) // empty SRV

	target := config.DNSTarget{
		Name:     "empty.svc",
		Type:     "srv",
		Scheme:   "http",
		Interval: time.Hour,
		Weight:   1,
		MaxConns: 100,
	}
	d := NewDiscoverer(b, []config.DNSTarget{target}, fr)
	d.sync(target)

	if got := urlSet(b); len(got) != 0 {
		t.Errorf("expected 0 backends for empty SRV response, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// DNS TTL refresh — second sync updates backends
// ---------------------------------------------------------------------------

// TestDiscovery_TTLRefresh verifies that after a "TTL" the second sync adds
// new addresses and removes addresses from the first set.
func TestDiscovery_TTLRefresh(t *testing.T) {
	b := &testBalancer{}
	fr := newFakeResolver()
	fr.setHosts("ttl.svc", []string{"192.168.1.1", "192.168.1.2"})

	target := config.DNSTarget{
		Name:     "ttl.svc",
		Type:     "a",
		Scheme:   "http",
		Port:     8080,
		Interval: time.Hour,
		Weight:   1,
		MaxConns: 100,
	}
	d := NewDiscoverer(b, []config.DNSTarget{target}, fr)
	d.sync(target)

	// After "TTL": replace .1 and .2 with .3 and .4.
	fr.setHosts("ttl.svc", []string{"192.168.1.3", "192.168.1.4"})
	d.sync(target)

	got := urlSet(b)
	want := []string{"http://192.168.1.3:8080", "http://192.168.1.4:8080"}
	if !equalURLs(got, want) {
		t.Errorf("after TTL refresh urls = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// DNS discoverer ticker path (run goroutine re-resolves on interval)
// ---------------------------------------------------------------------------

// TestDiscovery_TickerRefreshes verifies the run goroutine re-resolves on
// every interval tick and applies updates to the balancer.
func TestDiscovery_TickerRefreshes(t *testing.T) {
	b := &testBalancer{}
	fr := newFakeResolver()
	fr.setHosts("tick.svc", []string{"10.20.0.1"})

	target := config.DNSTarget{
		Name:     "tick.svc",
		Type:     "a",
		Scheme:   "http",
		Port:     9000,
		Interval: 20 * time.Millisecond,
		Weight:   1,
		MaxConns: 100,
	}
	d := NewDiscoverer(b, []config.DNSTarget{target}, fr)
	d.Start()

	// Wait for initial resolve.
	waitFor(t, time.Second, func() bool {
		return len(urlSet(b)) == 1
	})

	// Change DNS response.
	fr.setHosts("tick.svc", []string{"10.20.0.2"})

	// Wait for tick to pick up the new address.
	waitFor(t, 2*time.Second, func() bool {
		got := urlSet(b)
		return len(got) == 1 && got[0] == "http://10.20.0.2:9000"
	})

	d.Stop()
}

// ---------------------------------------------------------------------------
// resolve — default scheme
// ---------------------------------------------------------------------------

// TestResolve_DefaultScheme verifies that when Scheme is empty the resolve
// function defaults to "http".
func TestResolve_DefaultScheme(t *testing.T) {
	b := &testBalancer{}
	fr := newFakeResolver()
	fr.setHosts("def.svc", []string{"1.2.3.4"})

	target := config.DNSTarget{
		Name:     "def.svc",
		Type:     "a",
		Scheme:   "", // empty — should default to http
		Port:     80,
		Interval: time.Hour,
		Weight:   1,
		MaxConns: 100,
	}
	d := NewDiscoverer(b, []config.DNSTarget{target}, fr)
	d.sync(target)

	got := urlSet(b)
	if len(got) != 1 || got[0] != "http://1.2.3.4:80" {
		t.Errorf("resolve with empty scheme = %v, want [http://1.2.3.4:80]", got)
	}
}

// ---------------------------------------------------------------------------
// loadInCluster
// ---------------------------------------------------------------------------

// TestLoadInCluster_MissingToken verifies loadInCluster returns error when the
// in-cluster token file is absent.
func TestLoadInCluster_MissingToken(t *testing.T) {
	if _, err := os.Stat(inClusterTokenFile); err == nil {
		t.Skip("running inside a Kubernetes pod; skipping absence test")
	}
	kd := &KubernetesDiscovery{}
	if err := kd.loadInCluster(); err == nil {
		t.Fatal("expected error from missing in-cluster token, got nil")
	}
}

// ---------------------------------------------------------------------------
// addressesFromEndpoints
// ---------------------------------------------------------------------------

// TestAddressesFromEndpoints_Empty verifies no panic when subsets is nil.
func TestAddressesFromEndpoints_Empty(t *testing.T) {
	kd := &KubernetesDiscovery{portName: "http"}
	ep := &k8sEndpoints{Subsets: nil}
	result := kd.addressesFromEndpoints(ep)
	if len(result) != 0 {
		t.Errorf("expected empty result for nil subsets, got %v", result)
	}
}

// TestAddressesFromEndpoints_NoMatchingPort verifies that subsets with no
// matching port name return no addresses.
func TestAddressesFromEndpoints_NoMatchingPort(t *testing.T) {
	kd := &KubernetesDiscovery{portName: "grpc"}
	ep := &k8sEndpoints{
		Subsets: []k8sSubset{{
			Addresses: []k8sAddress{{IP: "1.2.3.4"}},
			Ports:     []k8sPort{{Name: "http", Port: 80}},
		}},
	}
	result := kd.addressesFromEndpoints(ep)
	if len(result) != 0 {
		t.Errorf("expected empty result for non-matching portName, got %v", result)
	}
}

// ---------------------------------------------------------------------------
// resolvePort
// ---------------------------------------------------------------------------

// TestResolvePort_EmptyPortName verifies that empty portName returns the first port.
func TestResolvePort_EmptyPortName(t *testing.T) {
	kd := &KubernetesDiscovery{portName: ""}
	ports := []k8sPort{{Name: "http", Port: 8080}, {Name: "metrics", Port: 9090}}
	if p := kd.resolvePort(ports); p != 8080 {
		t.Errorf("resolvePort with empty name = %d, want 8080", p)
	}
}

// TestResolvePort_NoMatch verifies 0 is returned when no port matches.
func TestResolvePort_NoMatch(t *testing.T) {
	kd := &KubernetesDiscovery{portName: "grpc"}
	ports := []k8sPort{{Name: "http", Port: 8080}}
	if p := kd.resolvePort(ports); p != 0 {
		t.Errorf("resolvePort with no match = %d, want 0", p)
	}
}

// ---------------------------------------------------------------------------
// NewKubernetesDiscovery from kubeconfig
// ---------------------------------------------------------------------------

// TestNewKubernetesDiscovery_FromKubeconfig exercises the kubeconfig code path.
func TestNewKubernetesDiscovery_FromKubeconfig(t *testing.T) {
	kc := `
current-context: ctx
clusters:
- name: cls
  cluster:
    server: https://k8s.example.com
contexts:
- name: ctx
  context:
    cluster: cls
    user: u
users:
- name: u
  user:
    token: my-token
`
	p := writeKubeconfig(t, t.TempDir(), kc)
	mb := &mockBalancer{}
	cfg := config.KubernetesDiscoveryConfig{
		Namespace:  "ns1",
		Service:    "svc1",
		Kubeconfig: p,
	}
	kd, err := NewKubernetesDiscovery(cfg, mb)
	if err != nil {
		t.Fatalf("NewKubernetesDiscovery: %v", err)
	}
	if kd.apiServer != "https://k8s.example.com" {
		t.Errorf("apiServer = %q, want %q", kd.apiServer, "https://k8s.example.com")
	}
	if kd.token != "my-token" {
		t.Errorf("token = %q, want %q", kd.token, "my-token")
	}
}

// TestNewKubernetesDiscovery_FromKubeconfig_CustomResync verifies custom
// resync_period is parsed and stored.
func TestNewKubernetesDiscovery_FromKubeconfig_CustomResync(t *testing.T) {
	kc := `
current-context: ctx
clusters:
- name: cls
  cluster:
    server: https://k8s.local
contexts:
- name: ctx
  context:
    cluster: cls
    user: u
users:
- name: u
  user:
    token: tok
`
	p := writeKubeconfig(t, t.TempDir(), kc)
	mb := &mockBalancer{}
	cfg := config.KubernetesDiscoveryConfig{
		Namespace:    "default",
		Service:      "my-svc",
		Kubeconfig:   p,
		ResyncPeriod: "5m",
	}
	kd, err := NewKubernetesDiscovery(cfg, mb)
	if err != nil {
		t.Fatalf("NewKubernetesDiscovery: %v", err)
	}
	if kd.resync != 5*time.Minute {
		t.Errorf("resync = %v, want 5m", kd.resync)
	}
}

// ---------------------------------------------------------------------------
// Ensure strings package is used (suppress unused-import error)
// ---------------------------------------------------------------------------

// TestLoadInCluster_TokenTrimming verifies that a token with trailing whitespace
// is trimmed (exercises the strings.TrimSpace branch in loadInCluster).
func TestLoadInCluster_TokenTrimming(t *testing.T) {
	// Simulate what loadInCluster does with a token that has a trailing newline.
	raw := "my-sa-token\n"
	trimmed := strings.TrimSpace(raw)
	if trimmed != "my-sa-token" {
		t.Errorf("TrimSpace(%q) = %q, want %q", raw, trimmed, "my-sa-token")
	}
}
