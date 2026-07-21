//go:build integration

package server_test

import (
	"crypto/tls"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// startPebble locates the pebble binary (checks PATH and PEBBLE_PATH env).
// Skips the test if pebble is not found — it is not installed in normal CI.
func startPebble(t *testing.T) (directoryURL string) {
	t.Helper()

	pebbleBin := os.Getenv("PEBBLE_PATH")
	if pebbleBin == "" {
		pebbleBin, _ = exec.LookPath("pebble")
	}
	if pebbleBin == "" {
		t.Skip("pebble binary not found; set PEBBLE_PATH or install pebble to run ACME integration tests")
	}

	// Write a minimal pebble config
	pebbleCfg := `{"pebble":{"listenAddress":"127.0.0.1:14000","managementListenAddress":"127.0.0.1:15000","certificate":"","privateKey":"","httpPort":5002,"tlsPort":5001,"ocspResponderURL":"","externalAccountBindingRequired":false}}`
	cfgFile := filepath.Join(t.TempDir(), "pebble-config.json")
	if err := os.WriteFile(cfgFile, []byte(pebbleCfg), 0600); err != nil {
		t.Fatalf("write pebble config: %v", err)
	}

	cmd := exec.Command(pebbleBin, "-config", cfgFile, "-strict")
	cmd.Env = append(os.Environ(), "PEBBLE_VA_NOSLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start pebble: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// Wait for Pebble to be ready
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("https://127.0.0.1:14000/dir")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return "https://127.0.0.1:14000/dir"
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("pebble did not become ready within 10s")
	return ""
}

// TestACMECertIssuanceWithPebble verifies that the ACME manager can obtain
// a certificate from a Pebble ACME server.
// Run with: go test -tags=integration -run TestACMECertIssuanceWithPebble ./internal/server/...
func TestACMECertIssuanceWithPebble(t *testing.T) {
	directoryURL := startPebble(t)
	t.Logf("Pebble directory URL: %s", directoryURL)
	// TODO: wire autocert.Manager to directoryURL and assert cert is returned
	// This is intentionally a skeleton — full implementation requires
	// HTTP-01 challenge solver wiring which needs a real listener on port 80.
	// The test validates that Pebble starts and responds correctly.
	t.Logf("Pebble is running at %s — full issuance test requires HTTP-01 port 80", directoryURL)
}
