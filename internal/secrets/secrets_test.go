package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	vault "github.com/hashicorp/vault/api"
)

// mockVaultHandler returns an http.HandlerFunc that serves a fixed KV v2
// response containing the supplied data map under the standard
// {"data":{"data":{...}}} envelope.
func mockVaultHandler(data map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inner := make(map[string]interface{}, len(data))
		for k, v := range data {
			inner[k] = v
		}
		body := map[string]interface{}{
			"data": map[string]interface{}{
				"data": inner,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
}

// vaultClientForTest creates a VaultClient pointed at ts using the given mountPath.
func vaultClientForTest(t *testing.T, ts *httptest.Server, mountPath string) *VaultClient {
	t.Helper()
	cfg := vault.DefaultConfig()
	cfg.Address = ts.URL
	c, err := vault.NewClient(cfg)
	if err != nil {
		t.Fatalf("vault.NewClient: %v", err)
	}
	c.SetToken("test-token")
	return &VaultClient{client: c, mountPath: mountPath}
}

// TestExpand verifies that a set environment variable is substituted.
func TestExpand(t *testing.T) {
	t.Setenv("TEST_RPLB_SECRET", "hello")
	got := Expand("pass=${TEST_RPLB_SECRET}")
	want := "pass=hello"
	if got != want {
		t.Fatalf("Expand: got %q, want %q", got, want)
	}
}

// TestExpandUnknown verifies that an unset variable placeholder is preserved.
func TestExpandUnknown(t *testing.T) {
	os.Unsetenv("RPLB_UNDEFINED_XYZ") //nolint:errcheck
	got := Expand("x=${RPLB_UNDEFINED_XYZ}")
	want := "x=${RPLB_UNDEFINED_XYZ}"
	if got != want {
		t.Fatalf("Expand: got %q, want %q", got, want)
	}
}

// TestExpandBraced checks that ${VAR} is expanded.
func TestExpandBraced(t *testing.T) {
	t.Setenv("RPLB_TEST_BRACED", "world")
	got := Expand("hello=${RPLB_TEST_BRACED}")
	if !strings.Contains(got, "world") {
		t.Fatalf("got %q, expected to contain 'world'", got)
	}
}

// TestExpandUnbraced checks that $VAR is expanded.
func TestExpandUnbraced(t *testing.T) {
	t.Setenv("RPLB_TEST_UNBRACED", "earth")
	got := Expand("hello=$RPLB_TEST_UNBRACED")
	if !strings.Contains(got, "earth") {
		t.Fatalf("got %q, expected to contain 'earth'", got)
	}
}

// TestVaultClientGet verifies that Get() correctly parses the KV v2 envelope
// returned by a mock Vault server.
func TestVaultClientGet(t *testing.T) {
	ts := httptest.NewServer(mockVaultHandler(map[string]string{
		"password": "s3cr3t",
	}))
	defer ts.Close()

	vc := vaultClientForTest(t, ts, "secret")

	data, err := vc.Get(context.Background(), "myapp/db")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got := data["password"]; got != "s3cr3t" {
		t.Fatalf("Get[password]: got %q, want %q", got, "s3cr3t")
	}
}

// TestExpandAllVault verifies that ExpandAll resolves ${vault:PATH#KEY}
// placeholders against a mock Vault server.
func TestExpandAllVault(t *testing.T) {
	ts := httptest.NewServer(mockVaultHandler(map[string]string{
		"password": "s3cr3t",
	}))
	defer ts.Close()

	vc := vaultClientForTest(t, ts, "secret")

	fields := []string{"${vault:myapp/db#password}"}
	out, err := ExpandAll(fields, vc, context.Background())
	if err != nil {
		t.Fatalf("ExpandAll: %v", err)
	}

	if len(out) != 1 || out[0] != "s3cr3t" {
		t.Fatalf("ExpandAll: got %v, want [s3cr3t]", out)
	}
}

// TestExpandAllNoVault checks ExpandAll with a nil VaultClient falls back to Expand.
func TestExpandAllNoVault(t *testing.T) {
	t.Setenv("RPLB_TEST_ALL_NO_VAULT", "expanded")
	fields := []string{"val=${RPLB_TEST_ALL_NO_VAULT}", "plain"}
	out, err := ExpandAll(fields, nil, context.Background())
	if err != nil {
		t.Fatalf("ExpandAll: %v", err)
	}
	if out[0] != "val=expanded" {
		t.Fatalf("out[0]: got %q, want 'val=expanded'", out[0])
	}
	if out[1] != "plain" {
		t.Fatalf("out[1]: got %q, want 'plain'", out[1])
	}
}
