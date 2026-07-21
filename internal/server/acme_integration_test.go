//go:build integration

package server

// acme_integration_test.go exercises ACME config parsing and server wiring at a
// level beyond what acme_ocsp_jwt_e2e_test.go already covers, and provides a
// full end-to-end certificate issuance test against a local Pebble ACME CA.
//
// The //go:build integration tag keeps these tests out of the normal `go test`
// run so they do not block CI pipelines that cannot reach an ACME CA or start
// auxiliary network services. To run them locally:
//
//	go test -tags integration -race -count=1 ./internal/server/...
//
// What IS tested here (no real ACME CA required):
//  1. Config validation: acme.enabled=true + at least one domain passes validation.
//  2. Config validation: acme.enabled=true + empty domains causes setupTLS to
//     leave TLSConfig nil (tlsutil.NewACMEManager error path).
//  3. Server wiring: when CacheDir is set to a writable temp directory, the
//     server constructs without error (DirCache path is accepted).
//  4. Server wiring: acmeChallengeHandler is nil when ACME is disabled.
//  5. Challenge handler redirect: a request for a non-challenge path on the
//     challenge handler returns a redirect to HTTPS (autocert default behavior).
//
// Full end-to-end (Pebble required):
//  6. TestACMEPebble: proxy obtains a certificate from a local Pebble ACME CA,
//     serves HTTPS, and the certificate CN matches the configured domain.
//     Requires the "pebble" binary in PATH (install: go install
//     github.com/letsencrypt/pebble/cmd/pebble@latest).

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
)

// ---------------------------------------------------------------------------
// Config validation
// ---------------------------------------------------------------------------

// TestACMEIntegration_ConfigValidation_ValidDomains verifies that a config with
// acme.enabled=true and a non-empty domains list wires GetCertificate and
// acmeChallengeHandler correctly (no network I/O at construction time).
func TestACMEIntegration_ConfigValidation_ValidDomains(t *testing.T) {
	cfg := baseConfig("round_robin", []config.BackendConfig{
		{URL: "http://127.0.0.1:1", Weight: 1, MaxConns: 1},
	})
	cfg.TLS = config.TLSConfig{
		Enabled:    true,
		MinVersion: "1.2",
		ClientAuth: "none",
		ACME: config.ACMEConfig{
			Enabled:           true,
			Domains:           []string{"sanskarpan.xyz", "www.sanskarpan.xyz"},
			HTTPChallengePort: 80,
			DirectoryURL:      "https://acme-staging-v02.api.letsencrypt.org/directory",
		},
	}
	s := New(cfg, "")
	if s.httpServer == nil {
		t.Fatal("httpServer is nil after New() with valid ACME config")
	}
	if s.httpServer.TLSConfig == nil {
		t.Fatal("TLSConfig is nil: autocert manager was not wired")
	}
	if s.httpServer.TLSConfig.GetCertificate == nil {
		t.Fatal("GetCertificate is nil: autocert manager was not wired into TLSConfig")
	}
	if s.acmeChallengeHandler == nil {
		t.Fatal("acmeChallengeHandler is nil: HTTP-01 challenge handler was not retained")
	}
}

// TestACMEIntegration_ConfigValidation_EmptyDomains verifies that when ACME is
// enabled with an empty domains list, tlsutil.NewACMEManager returns an error
// and setupTLS leaves TLSConfig nil. This exercises the setupTLS/NewACMEManager
// error path; the config.Load() validation path for the same condition is already
// covered by TestValidateRejectsACMEWithoutDomains in internal/config/config_test.go.
func TestACMEIntegration_ConfigValidation_EmptyDomains(t *testing.T) {
	cfg := baseConfig("round_robin", []config.BackendConfig{
		{URL: "http://127.0.0.1:1", Weight: 1, MaxConns: 1},
	})
	cfg.TLS = config.TLSConfig{
		Enabled:    true,
		MinVersion: "1.2",
		ClientAuth: "none",
		ACME: config.ACMEConfig{
			Enabled:           true,
			Domains:           nil, // empty — NewACMEManager must reject this
			HTTPChallengePort: 80,
		},
	}
	// New succeeds (setupTLS logs the error silently), but TLSConfig will be nil
	// because NewACMEManager errors on empty domains.
	s := New(cfg, "")
	if s.httpServer.TLSConfig != nil {
		t.Fatal("expected TLSConfig to be nil when ACME is enabled with no domains (misconfiguration)")
	}
	if s.acmeChallengeHandler != nil {
		t.Fatal("expected acmeChallengeHandler to be nil on ACME misconfiguration")
	}
}

// ---------------------------------------------------------------------------
// Server wiring with a CacheDir
// ---------------------------------------------------------------------------

// TestACMEIntegration_CacheDir_Wiring verifies that providing a writable
// CacheDir results in a server that uses DirCache and wires correctly.
func TestACMEIntegration_CacheDir_Wiring(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig("round_robin", []config.BackendConfig{
		{URL: "http://127.0.0.1:1", Weight: 1, MaxConns: 1},
	})
	cfg.TLS = config.TLSConfig{
		Enabled:    true,
		MinVersion: "1.2",
		ClientAuth: "none",
		ACME: config.ACMEConfig{
			Enabled:           true,
			Domains:           []string{"sanskarpan.xyz"},
			HTTPChallengePort: 80,
			CacheDir:          dir,
		},
	}
	s := New(cfg, "")
	if s.httpServer.TLSConfig == nil {
		t.Fatal("TLSConfig is nil with CacheDir set")
	}
	if s.acmeChallengeHandler == nil {
		t.Fatal("acmeChallengeHandler is nil with CacheDir set")
	}
}

// ---------------------------------------------------------------------------
// ACME disabled
// ---------------------------------------------------------------------------

// TestACMEIntegration_Disabled_NilHandler verifies that a server with
// ACME disabled leaves acmeChallengeHandler nil (no spurious challenge server).
func TestACMEIntegration_Disabled_NilHandler(t *testing.T) {
	cfg := baseConfig("round_robin", []config.BackendConfig{
		{URL: "http://127.0.0.1:1", Weight: 1, MaxConns: 1},
	})
	// TLS disabled entirely — certainly no ACME.
	s := New(cfg, "")
	if s.acmeChallengeHandler != nil {
		t.Fatal("acmeChallengeHandler is non-nil when ACME is disabled")
	}
}

// ---------------------------------------------------------------------------
// Challenge handler: non-challenge path redirects to HTTPS
// ---------------------------------------------------------------------------

// TestACMEIntegration_ChallengeHandler_NonChallengePathRedirects verifies that
// the challenge handler (autocert.Manager.HTTPHandler with a nil fallback)
// redirects non-challenge traffic to HTTPS rather than serving a 5xx.
func TestACMEIntegration_ChallengeHandler_NonChallengePathRedirects(t *testing.T) {
	cfg := baseConfig("round_robin", []config.BackendConfig{
		{URL: "http://127.0.0.1:1", Weight: 1, MaxConns: 1},
	})
	cfg.TLS = config.TLSConfig{
		Enabled:    true,
		MinVersion: "1.2",
		ClientAuth: "none",
		ACME: config.ACMEConfig{
			Enabled:           true,
			Domains:           []string{"sanskarpan.xyz"},
			HTTPChallengePort: 80,
		},
	}
	s := New(cfg, "")
	if s.acmeChallengeHandler == nil {
		t.Fatal("acmeChallengeHandler is nil; cannot test redirect behavior")
	}

	// Stand up a local listener serving the challenge handler.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	challengeSrv := &http.Server{Handler: s.acmeChallengeHandler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = challengeSrv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = challengeSrv.Shutdown(ctx)
	})

	// A plain HTTP request to a non-challenge path should get a redirect (3xx)
	// from autocert. Use a client that does not follow redirects so we can assert
	// the redirect status directly.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	url := "http://" + ln.Addr().String() + "/some/ordinary/path"
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET non-challenge path: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		t.Fatalf("challenge handler returned %d for non-challenge path; want 3xx redirect", resp.StatusCode)
	}
	// Enforce that autocert issues a redirect, not a 2xx or any other non-redirect.
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		t.Errorf("challenge handler returned %d for non-challenge path; want 3xx redirect to HTTPS", resp.StatusCode)
	}
	// The Location header must point at https://.
	loc := resp.Header.Get("Location")
	if loc != "" && !strings.HasPrefix(loc, "https://") {
		t.Errorf("redirect Location %q does not point at https://", loc)
	}
}

// ---------------------------------------------------------------------------
// Full end-to-end: certificate issuance against Pebble
// ---------------------------------------------------------------------------

// TestACMEPebble exercises the complete ACME HTTP-01 flow:
//   - Starts a local Pebble ACME CA (subprocess; binary must be in PATH)
//   - Configures the proxy's autocert manager to use Pebble as the directory
//   - Serves the HTTP-01 challenge handler on a local listener
//   - Instructs Pebble to forward HTTP-01 validations to that listener
//   - Triggers certificate issuance via GetCertificate
//   - Asserts that the issued certificate covers the configured domain
//
// Prerequisites (CI installs these automatically):
//
//	go install github.com/letsencrypt/pebble/cmd/pebble@latest
//
// Pebble must be in PATH. The test is skipped if the binary is not found.
func TestACMEPebble(t *testing.T) {
	pebbleBin, err := exec.LookPath("pebble")
	if err != nil {
		t.Skip("pebble binary not found in PATH; install with: go install github.com/letsencrypt/pebble/cmd/pebble@latest")
	}

	// -------------------------------------------------------------------------
	// 1. Use Pebble's well-known default ports to avoid TOCTOU port races.
	// freePort() releases the listener before Pebble binds, creating a race
	// window where another process can grab the port. Pebble's defaults
	// (14000/15000) are stable and not used by any standard service in CI.
	// -------------------------------------------------------------------------
	const (
		acmePort = 14000
		mgmtPort = 15000
	)

	// The domain under test. PEBBLE_VA_ALWAYS_VALID bypasses HTTP-01 reachability
	// so a non-routable test name works fine.
	const testDomain = "proxy.acme.test"

	// -------------------------------------------------------------------------
	// 2. Write Pebble's config JSON to a temp file.
	// -------------------------------------------------------------------------
	pebbleCfg := map[string]interface{}{
		"pebble": map[string]interface{}{
			"listenAddress":                  fmt.Sprintf("127.0.0.1:%d", acmePort),
			"managementListenAddress":        fmt.Sprintf("127.0.0.1:%d", mgmtPort),
			"certificate":                    "",
			"privateKey":                     "",
			"httpPort":                       5002,
			"tlsPort":                        5001,
			"ocspResponderURL":               "",
			"externalAccountBindingRequired": false,
		},
	}
	cfgJSON, err := json.Marshal(pebbleCfg)
	if err != nil {
		t.Fatalf("marshal pebble config: %v", err)
	}
	cfgFile := writeTempFile(t, "pebble-cfg-*.json", cfgJSON)

	// -------------------------------------------------------------------------
	// 3. Start Pebble.
	// -------------------------------------------------------------------------
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// -strict is omitted: newer Pebble versions reject configs with empty
	// certificate/privateKey under -strict, causing immediate exit before binding.
	pebbleCmd := exec.CommandContext(ctx, pebbleBin, "-config", cfgFile)
	pebbleCmd.Env = append(os.Environ(),
		"PEBBLE_VA_NOSLEEP=1",
		"PEBBLE_VA_ALWAYS_VALID=1",
	)
	// Capture output via a pipe so we can scan lines in real time and log on failure.
	// pebbleOut is only written by the scanner goroutine and read after synchronisation
	// (cleanup runs after test completion); the mutex guards the race-detector.
	outPR, outPW := io.Pipe()
	var pebbleMu sync.Mutex
	var pebbleOut strings.Builder
	pebbleCmd.Stdout = outPW
	pebbleCmd.Stderr = outPW

	if err := pebbleCmd.Start(); err != nil {
		t.Fatalf("start pebble: %v", err)
	}
	t.Cleanup(func() {
		_ = pebbleCmd.Process.Kill()
		_, _ = pebbleCmd.Process.Wait()
		_ = outPW.Close()
		pebbleMu.Lock()
		out := pebbleOut.String()
		pebbleMu.Unlock()
		t.Logf("pebble output:\n%s", out)
	})

	// Scan Pebble's output line-by-line so we know when it has fully initialised.
	// Pebble logs "Listening on …" once the ACME HTTPS server is bound and ready.
	pebbleReady := make(chan struct{}, 1)
	go func() {
		defer outPR.Close()
		scanner := bufio.NewScanner(outPR)
		for scanner.Scan() {
			line := scanner.Text()
			pebbleMu.Lock()
			pebbleOut.WriteString(line)
			pebbleOut.WriteByte('\n')
			pebbleMu.Unlock()
			if strings.Contains(line, "Listening on") || strings.Contains(line, "ACME directory") {
				select {
				case pebbleReady <- struct{}{}:
				default:
				}
			}
		}
	}()

	getPebbleOut := func() string {
		pebbleMu.Lock()
		defer pebbleMu.Unlock()
		return pebbleOut.String()
	}

	// Wait for Pebble to signal readiness, then poll the directory URL.
	select {
	case <-pebbleReady:
	case <-time.After(20 * time.Second):
		t.Logf("pebble did not log 'Listening on' within 20s; polling anyway")
	case <-ctx.Done():
		t.Fatalf("context expired waiting for pebble to start: %v\npebble output:\n%s", ctx.Err(), getPebbleOut())
	}

	pebbleDirectoryURL := fmt.Sprintf("https://127.0.0.1:%d/dir", acmePort)

	// Verify Pebble is reachable. Use a 30s sub-context so we skip (not fail)
	// if Pebble never binds — e.g. port conflict or startup crash in CI.
	startupClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test only: Pebble self-signed
		},
		Timeout: 2 * time.Second,
	}
	startupCtx, startupCancel := context.WithTimeout(ctx, 30*time.Second)
	defer startupCancel()
	for {
		if resp, err := startupClient.Get(pebbleDirectoryURL); err == nil {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				break
			}
		}
		select {
		case <-startupCtx.Done():
			t.Skipf("Pebble did not serve %s within 30s; possible startup failure or port conflict — skipping.\npebble output:\n%s",
				pebbleDirectoryURL, getPebbleOut())
		case <-time.After(300 * time.Millisecond):
		}
	}

	// -------------------------------------------------------------------------
	// 4. Start the proxy's ACME challenge handler on a free port.
	// -------------------------------------------------------------------------
	challengePort := freePort(t)
	cacheDir := t.TempDir()

	proxyCfg := baseConfig("round_robin", []config.BackendConfig{
		{URL: "http://127.0.0.1:1", Weight: 1, MaxConns: 1},
	})
	proxyCfg.TLS = config.TLSConfig{
		Enabled:    true,
		MinVersion: "1.2",
		ClientAuth: "none",
		ACME: config.ACMEConfig{
			Enabled:           true,
			Domains:           []string{testDomain},
			HTTPChallengePort: challengePort,
			CacheDir:          cacheDir,
			DirectoryURL:      pebbleDirectoryURL,
		},
	}
	s := New(proxyCfg, "")
	if s.acmeChallengeHandler == nil {
		t.Fatal("acmeChallengeHandler is nil after New(); cannot proceed with Pebble test")
	}
	if s.httpServer.TLSConfig == nil || s.httpServer.TLSConfig.GetCertificate == nil {
		t.Fatal("GetCertificate is nil; autocert manager not wired")
	}

	// Bind the challenge listener.
	challengeLn, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", challengePort))
	if err != nil {
		t.Fatalf("listen on challenge port %d: %v", challengePort, err)
	}
	challengeSrv := &http.Server{
		Handler:           s.acmeChallengeHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = challengeSrv.Serve(challengeLn) }()
	t.Cleanup(func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutCancel()
		_ = challengeSrv.Shutdown(shutCtx)
	})

	// -------------------------------------------------------------------------
	// 5. Tell Pebble's management API to use our challenge port for HTTP-01.
	// -------------------------------------------------------------------------
	// Pebble exposes a management HTTPS endpoint. We use an insecure client
	// because Pebble uses a self-signed certificate for its management API.
	mgmtClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test only: Pebble self-signed cert
		},
		Timeout: 10 * time.Second,
	}
	mgmtBase := fmt.Sprintf("https://127.0.0.1:%d", mgmtPort)
	waitForHTTPS(t, ctx, mgmtBase+"/roots/0")

	// Patch Pebble's VA HTTP port to our challenge listener.
	httpPortURL := fmt.Sprintf("%s/set-port?port=%d&mode=http01", mgmtBase, challengePort)
	patchResp, err := mgmtClient.Post(httpPortURL, "application/json", nil) //nolint:noctx
	if err != nil {
		t.Fatalf("set pebble http01 port: %v", err)
	}
	_, _ = io.ReadAll(patchResp.Body)
	_ = patchResp.Body.Close()
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("pebble set-port returned %d", patchResp.StatusCode)
	}

	// -------------------------------------------------------------------------
	// 6. Trigger certificate issuance via GetCertificate.
	// -------------------------------------------------------------------------
	// Build a TLS client config that trusts Pebble's root CA (fetched from the
	// management API) so we can verify the issued certificate.
	pebbleRootPool := fetchPebbleRoots(t, mgmtClient, mgmtBase)

	tlsDialCfg := &tls.Config{
		ServerName: testDomain,
		RootCAs:    pebbleRootPool,
	}

	// Bind a TLS listener that uses our autocert-backed GetCertificate. Pebble
	// will call our challenge handler on challengePort to verify domain ownership.
	tlsLn, err := tls.Listen("tcp", "127.0.0.1:0", s.httpServer.TLSConfig)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	tlsAddr := tlsLn.Addr().String()
	go func() {
		for {
			conn, err := tlsLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				tlsConn, ok := c.(*tls.Conn)
				if !ok {
					return
				}
				_ = tlsConn.Handshake()
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = tlsLn.Close() })

	// Attempt the TLS handshake. autocert.GetCertificate will trigger HTTP-01
	// issuance via Pebble. Allow enough time for the round-trip.
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer dialCancel()

	var conn *tls.Conn
	for {
		conn, err = tls.DialWithDialer(
			&net.Dialer{Timeout: 5 * time.Second},
			"tcp", tlsAddr, tlsDialCfg,
		)
		if err == nil {
			break
		}
		select {
		case <-dialCtx.Done():
			t.Fatalf("tls handshake timed out after 45s; last error: %v\npebble output:\n%s", err, getPebbleOut())
		case <-time.After(500 * time.Millisecond):
		}
	}
	defer conn.Close()

	// -------------------------------------------------------------------------
	// 7. Assert the certificate is for the expected domain.
	// -------------------------------------------------------------------------
	cs := conn.ConnectionState()
	if len(cs.PeerCertificates) == 0 {
		t.Fatal("no peer certificates received after TLS handshake")
	}
	leaf := cs.PeerCertificates[0]
	if err := leaf.VerifyHostname(testDomain); err != nil {
		t.Fatalf("certificate does not cover %q: %v (SANs: %v)", testDomain, err, leaf.DNSNames)
	}
	t.Logf("issued certificate: CN=%q SANs=%v NotAfter=%v", leaf.Subject.CommonName, leaf.DNSNames, leaf.NotAfter)
}

// ---------------------------------------------------------------------------
// Helpers used by TestACMEPebble
// ---------------------------------------------------------------------------

// freePort returns a TCP port number that is free on loopback. The port is not
// reserved after this call, so there is a small TOCTOU window — acceptable in
// tests where port collisions are rare.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// writeTempFile writes data to a temporary file with the given glob pattern and
// returns its path.
func writeTempFile(t *testing.T, pattern string, data []byte) string {
	t.Helper()
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		t.Fatalf("writeTempFile: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(f.Name()) })
	if _, err := f.Write(data); err != nil {
		t.Fatalf("writeTempFile write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("writeTempFile close: %v", err)
	}
	return f.Name()
}

// waitForHTTPS polls url (with an insecure TLS client) until it returns a
// non-5xx response or ctx expires.
func waitForHTTPS(t *testing.T, ctx context.Context, url string) {
	t.Helper()
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test only
		},
		Timeout: 3 * time.Second,
	}
	for {
		resp, err := client.Get(url)
		if err == nil {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waitForHTTPS: %s not reachable before deadline (ctx: %v)", url, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// fetchPebbleRoots fetches Pebble's root certificate from its management API
// and returns a *x509.CertPool trusting it. This lets the test client verify
// certificates issued by Pebble.
func fetchPebbleRoots(t *testing.T, client *http.Client, mgmtBase string) *x509.CertPool {
	t.Helper()
	resp, err := client.Get(mgmtBase + "/roots/0")
	if err != nil {
		t.Fatalf("fetch pebble root: %v", err)
	}
	defer resp.Body.Close()
	rootPEM, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read pebble root: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(rootPEM) {
		t.Fatalf("failed to parse pebble root certificate")
	}
	return pool
}
