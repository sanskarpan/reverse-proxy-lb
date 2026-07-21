package discovery

// KubernetesDiscovery implements Kubernetes Endpoints-based service discovery
// using the k8s REST API directly over net/http (no client-go required).
//
// It performs an initial list of the named Endpoints object, then opens a long-
// poll watch stream to receive incremental ADDED/MODIFIED/DELETED events.  For
// every ready address in a matching port subset the balancer is updated: adds
// when the address appears, removes when it disappears.
//
// Both in-cluster (ServiceAccount token + CA from the mounted secret) and
// out-of-cluster (kubeconfig YAML) auth are supported.  When kubeconfig is
// empty, in-cluster credentials are read from the standard mount path.

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/config"
)

// inClusterTokenFile and inClusterCAFile are the standard paths for a Pod's
// ServiceAccount credentials when running inside Kubernetes.
const (
	inClusterTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	inClusterCAFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	inClusterAPIServer = "https://kubernetes.default.svc"
)

// k8sEndpoints is the subset of the k8s Endpoints JSON we need.
type k8sEndpoints struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Subsets []k8sSubset `json:"subsets"`
}

type k8sSubset struct {
	Addresses         []k8sAddress `json:"addresses"`
	NotReadyAddresses []k8sAddress `json:"notReadyAddresses"`
	Ports             []k8sPort    `json:"ports"`
}

type k8sAddress struct {
	IP string `json:"ip"`
}

type k8sPort struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

// k8sWatchEvent is a single line emitted by the k8s watch stream.
type k8sWatchEvent struct {
	Type   string       `json:"type"` // ADDED, MODIFIED, DELETED
	Object k8sEndpoints `json:"object"`
}

// KubernetesDiscovery watches a Kubernetes Endpoints object via the k8s REST
// API HTTP watch endpoint.  No client-go required: pure stdlib net/http + JSON.
type KubernetesDiscovery struct {
	namespace   string
	service     string
	portName    string
	token       string
	tokenFile   string // path to the token file for refreshing (in-cluster only)
	clientCert  []byte // PEM-encoded client certificate (kubeconfig mTLS)
	clientKey   []byte // PEM-encoded client private key (kubeconfig mTLS)
	apiServer   string
	caCert      []byte
	resync      time.Duration
	balancer    balancer.Balancer
	scheme      string // "http" or "https" for backend URLs
	backendPort int    // override port when portName is empty

	stopCh chan struct{}
	wg     sync.WaitGroup

	mu    sync.Mutex
	owned map[string]*balancer.Backend // ip:port -> *Backend; tracks what we added
}

// NewKubernetesDiscovery creates a KubernetesDiscovery from config.
// The balancer is the target into which discovered backends are added.
func NewKubernetesDiscovery(cfg config.KubernetesDiscoveryConfig, b balancer.Balancer) (*KubernetesDiscovery, error) {
	kd := &KubernetesDiscovery{
		namespace: cfg.Namespace,
		service:   cfg.Service,
		portName:  cfg.PortName,
		resync:    30 * time.Second,
		balancer:  b,
		scheme:    "http",
		stopCh:    make(chan struct{}),
		owned:     make(map[string]*balancer.Backend),
	}

	if cfg.ResyncPeriod != "" {
		d, err := time.ParseDuration(cfg.ResyncPeriod)
		if err != nil {
			return nil, fmt.Errorf("kubernetes discovery: invalid resync_period %q: %w", cfg.ResyncPeriod, err)
		}
		kd.resync = d
	}

	if cfg.Kubeconfig != "" {
		if err := kd.loadKubeconfig(cfg.Kubeconfig); err != nil {
			return nil, fmt.Errorf("kubernetes discovery: load kubeconfig %q: %w", cfg.Kubeconfig, err)
		}
	} else {
		if err := kd.loadInCluster(); err != nil {
			return nil, fmt.Errorf("kubernetes discovery: load in-cluster credentials: %w", err)
		}
	}

	return kd, nil
}

// loadInCluster reads the ServiceAccount token and CA bundle from the standard
// in-cluster mount path.  The token file path is retained in kd.tokenFile so
// that refreshToken() can re-read it when the kubelet rotates it (projected
// service account tokens, k8s 1.21+, default lifetime ~1 h).
func (kd *KubernetesDiscovery) loadInCluster() error {
	tok, err := os.ReadFile(inClusterTokenFile)
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	ca, err := os.ReadFile(inClusterCAFile)
	if err != nil {
		return fmt.Errorf("read ca: %w", err)
	}
	kd.token = strings.TrimSpace(string(tok))
	kd.tokenFile = inClusterTokenFile // remember for rotation
	kd.caCert = ca
	kd.apiServer = inClusterAPIServer
	return nil
}

// refreshToken re-reads the token file (in-cluster only) and updates kd.token.
// It is a no-op when no tokenFile is set (kubeconfig or test path).
// Called on HTTP 401 to pick up a rotated projected service account token.
func (kd *KubernetesDiscovery) refreshToken() {
	if kd.tokenFile == "" {
		return
	}
	tok, err := os.ReadFile(kd.tokenFile)
	if err != nil {
		return
	}
	kd.token = strings.TrimSpace(string(tok))
}

// kubeconfig is a minimal subset of the YAML kubeconfig we parse for
// out-of-cluster auth.
type kubeconfig struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
			CertificateAuthority     string `yaml:"certificate-authority"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token               string `yaml:"token"`
			ClientCertificateData string `yaml:"client-certificate-data"`
			ClientKeyData         string `yaml:"client-key-data"`
		} `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
}

// loadKubeconfig parses a kubeconfig YAML file and extracts the API server URL,
// CA certificate, and bearer token for the current context.
func (kd *KubernetesDiscovery) loadKubeconfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	var kc kubeconfig
	if err := yaml.Unmarshal(data, &kc); err != nil {
		return fmt.Errorf("parse yaml: %w", err)
	}

	// Resolve context -> cluster + user.
	ctx := kc.CurrentContext
	var clusterName, userName string
	for _, c := range kc.Contexts {
		if c.Name == ctx {
			clusterName = c.Context.Cluster
			userName = c.Context.User
			break
		}
	}
	if clusterName == "" {
		return fmt.Errorf("context %q not found", ctx)
	}

	// Find cluster entry.
	for _, cl := range kc.Clusters {
		if cl.Name != clusterName {
			continue
		}
		kd.apiServer = cl.Cluster.Server
		// Prefer base64-embedded CA; fall back to file path.
		if cl.Cluster.CertificateAuthorityData != "" {
			ca, err := base64.StdEncoding.DecodeString(cl.Cluster.CertificateAuthorityData)
			if err != nil {
				return fmt.Errorf("decode ca-data: %w", err)
			}
			kd.caCert = ca
		} else if cl.Cluster.CertificateAuthority != "" {
			ca, err := os.ReadFile(cl.Cluster.CertificateAuthority)
			if err != nil {
				return fmt.Errorf("read ca file: %w", err)
			}
			kd.caCert = ca
		}
		break
	}

	// Find user entry: prefer bearer token; fall back to client certificate.
	for _, u := range kc.Users {
		if u.Name != userName {
			continue
		}
		kd.token = u.User.Token
		// Extract client certificate / key for mTLS auth (e.g. kubeadm, kind, GKE).
		if u.User.ClientCertificateData != "" && u.User.ClientKeyData != "" {
			cert, err := base64.StdEncoding.DecodeString(u.User.ClientCertificateData)
			if err != nil {
				return fmt.Errorf("decode client-certificate-data: %w", err)
			}
			key, err := base64.StdEncoding.DecodeString(u.User.ClientKeyData)
			if err != nil {
				return fmt.Errorf("decode client-key-data: %w", err)
			}
			kd.clientCert = cert
			kd.clientKey = key
		}
		break
	}

	if kd.apiServer == "" {
		return fmt.Errorf("cluster %q not found in kubeconfig", clusterName)
	}
	return nil
}

// buildHTTPClient returns an *http.Client configured for mutual TLS to the k8s
// API server.
//
// CA handling:
//   - When caCert is non-empty the supplied PEM bundle is used as the sole root
//     for verifying the API server's certificate (in-cluster and kubeconfig with
//     an embedded CA).
//   - When caCert is empty the system root pool is used, which is correct for API
//     servers whose certificate is signed by a publicly-trusted CA (e.g. Let's
//     Encrypt).  InsecureSkipVerify is NOT set: that would defeat TLS entirely.
//
// Client certificate:
//   - When clientCert and clientKey are both set (kubeconfig mTLS auth), the
//     keypair is loaded and presented during the TLS handshake.
func (kd *KubernetesDiscovery) buildHTTPClient() (*http.Client, error) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	if len(kd.caCert) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(kd.caCert) {
			return nil, fmt.Errorf("kubernetes discovery: failed to parse CA certificate")
		}
		tlsCfg.RootCAs = pool
	}
	// When caCert is nil, tlsCfg.RootCAs stays nil — Go uses the system pool,
	// which is correct for publicly-trusted API servers.  We deliberately do NOT
	// set InsecureSkipVerify here.

	// Load client certificate for kubeconfig mTLS auth (kubeadm / kind / GKE).
	if len(kd.clientCert) > 0 && len(kd.clientKey) > 0 {
		cert, err := tls.X509KeyPair(kd.clientCert, kd.clientKey)
		if err != nil {
			return nil, fmt.Errorf("kubernetes discovery: load client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	transport := &http.Transport{TLSClientConfig: tlsCfg}
	return &http.Client{Transport: transport}, nil
}

// endpointsURL returns the REST URL for the Endpoints resource.
func (kd *KubernetesDiscovery) endpointsURL() string {
	return fmt.Sprintf("%s/api/v1/namespaces/%s/endpoints/%s",
		kd.apiServer, kd.namespace, kd.service)
}

// watchURL returns the REST URL for the watch stream with a resourceVersion.
func (kd *KubernetesDiscovery) watchURL(rv string) string {
	base := kd.endpointsURL()
	return fmt.Sprintf("%s?watch=true&resourceVersion=%s", base, rv)
}

// doRequest performs an authenticated GET request to the k8s API.
// The provided context controls cancellation (used for graceful stop).
func (kd *KubernetesDiscovery) doRequest(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if kd.token != "" {
		req.Header.Set("Authorization", "Bearer "+kd.token)
	}
	return client.Do(req)
}

// Start launches the watch loop goroutine.
func (kd *KubernetesDiscovery) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel the context when stopCh is closed.
	kd.wg.Add(1)
	go func() {
		defer kd.wg.Done()
		select {
		case <-kd.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	kd.wg.Add(1)
	go kd.run(ctx, cancel)
}

// Stop signals the watch loop to exit and waits for it.
func (kd *KubernetesDiscovery) Stop() {
	select {
	case <-kd.stopCh:
	default:
		close(kd.stopCh)
	}
	kd.wg.Wait()
}

// run is the main goroutine: list+sync, then stream watch events, with
// exponential back-off on errors.  ctx is cancelled when stopCh is closed so
// in-flight HTTP requests are interrupted promptly.
func (kd *KubernetesDiscovery) run(ctx context.Context, cancel context.CancelFunc) {
	defer kd.wg.Done()
	defer cancel()

	backoff := time.Second
	const maxBackoff = 30 * time.Second

	client, err := kd.buildHTTPClient()
	if err != nil {
		// Can't build client at all — no point retrying.
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		rv, err := kd.listAndSync(ctx, client)
		if err != nil {
			kd.sleepOrStop(ctx, backoff)
			backoff = min2(backoff*2, maxBackoff)
			continue
		}
		backoff = time.Second

		// Stream watch events until error or stop.
		rv, err = kd.watchStream(ctx, client, rv)
		if err != nil {
			kd.sleepOrStop(ctx, backoff)
			backoff = min2(backoff*2, maxBackoff)
			continue
		}
		// watchStream returned without error (server closed stream); loop immediately.
		_ = rv
	}
}

// sleepOrStop waits for d or until ctx is Done.
func (kd *KubernetesDiscovery) sleepOrStop(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// listAndSync GETs the current Endpoints object, syncs it into the balancer,
// and returns the resourceVersion for subsequent watching.
func (kd *KubernetesDiscovery) listAndSync(ctx context.Context, client *http.Client) (string, error) {
	resp, err := kd.doRequest(ctx, client, kd.endpointsURL())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		// Token may have been rotated by the kubelet (projected service account
		// tokens, k8s 1.21+). Re-read from disk and let the caller retry.
		kd.refreshToken()
		return "", fmt.Errorf("kubernetes discovery: GET endpoints returned 401 (token refreshed)")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("kubernetes discovery: GET endpoints returned %d", resp.StatusCode)
	}

	var ep k8sEndpoints
	if err := json.NewDecoder(resp.Body).Decode(&ep); err != nil {
		return "", fmt.Errorf("kubernetes discovery: decode endpoints: %w", err)
	}

	desired := kd.addressesFromEndpoints(&ep)
	kd.sync(desired)
	return ep.Metadata.ResourceVersion, nil
}

// watchStream streams NDJSON watch events from the API server, updating the
// balancer on each ADDED/MODIFIED/DELETED event.  It returns when the stream
// ends (the API server closes it), ctx is cancelled, or an error occurs.
func (kd *KubernetesDiscovery) watchStream(ctx context.Context, client *http.Client, rv string) (string, error) {
	resp, err := kd.doRequest(ctx, client, kd.watchURL(rv))
	if err != nil {
		return rv, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		// Token may have been rotated; refresh and let the run-loop retry via
		// listAndSync on the next iteration.
		kd.refreshToken()
		return rv, fmt.Errorf("kubernetes discovery: watch returned 401 (token refreshed)")
	}
	if resp.StatusCode != http.StatusOK {
		return rv, fmt.Errorf("kubernetes discovery: watch returned %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return rv, nil
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event k8sWatchEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}

		rv = event.Object.Metadata.ResourceVersion
		desired := kd.addressesFromEndpoints(&event.Object)

		switch event.Type {
		case "ADDED", "MODIFIED":
			kd.sync(desired)
		case "DELETED":
			// On DELETE the object is gone: remove all backends we own.
			kd.removeAll()
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		return rv, err
	}
	return rv, nil
}

// addressesFromEndpoints extracts the set of "ip:port" strings for ready
// addresses in subsets that match kd.portName.  If portName is empty, the
// first port in each subset is used.
func (kd *KubernetesDiscovery) addressesFromEndpoints(ep *k8sEndpoints) map[string]struct{} {
	result := make(map[string]struct{})
	for _, subset := range ep.Subsets {
		port := kd.resolvePort(subset.Ports)
		if port == 0 {
			continue
		}
		for _, addr := range subset.Addresses {
			key := fmt.Sprintf("%s:%d", addr.IP, port)
			result[key] = struct{}{}
		}
	}
	return result
}

// resolvePort finds the port number in the subset that matches kd.portName.
// If portName is empty, the first port is returned.  Returns 0 when no match.
func (kd *KubernetesDiscovery) resolvePort(ports []k8sPort) int {
	for _, p := range ports {
		if kd.portName == "" || p.Name == kd.portName {
			return p.Port
		}
	}
	return 0
}

// sync diffs desired (ip:port set) against kd.owned and calls Add/Remove on
// the balancer to converge.
func (kd *KubernetesDiscovery) sync(desired map[string]struct{}) {
	kd.mu.Lock()
	defer kd.mu.Unlock()

	// Add newly discovered backends.
	for key := range desired {
		if _, ok := kd.owned[key]; ok {
			continue
		}
		url := fmt.Sprintf("%s://%s", kd.scheme, key)
		be := balancer.NewBackend(config.BackendConfig{
			URL:      url,
			Weight:   1,
			MaxConns: 100,
		})
		kd.balancer.Add(be)
		kd.owned[key] = be
	}

	// Remove backends that are no longer present.
	for key, be := range kd.owned {
		if _, ok := desired[key]; !ok {
			kd.balancer.Remove(be)
			delete(kd.owned, key)
		}
	}
}

// removeAll removes every backend this discovery instance owns.
func (kd *KubernetesDiscovery) removeAll() {
	kd.mu.Lock()
	defer kd.mu.Unlock()
	for key, be := range kd.owned {
		kd.balancer.Remove(be)
		delete(kd.owned, key)
	}
}

// min2 returns the smaller of two durations.
func min2(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
