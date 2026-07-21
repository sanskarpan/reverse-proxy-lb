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

// TestExpandUnbracedUnknown verifies that an unset $VAR placeholder is preserved.
func TestExpandUnbracedUnknown(t *testing.T) {
	os.Unsetenv("RPLB_UNDEFINED_UNBRACED_XYZ") //nolint:errcheck
	got := Expand("x=$RPLB_UNDEFINED_UNBRACED_XYZ")
	want := "x=$RPLB_UNDEFINED_UNBRACED_XYZ"
	if got != want {
		t.Fatalf("Expand unbraced unknown: got %q, want %q", got, want)
	}
}

// TestNewVaultClient verifies that NewVaultClient succeeds with valid parameters.
func TestNewVaultClient(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	vc, err := NewVaultClient(ts.URL, "test-token", "secret")
	if err != nil {
		t.Fatalf("NewVaultClient: %v", err)
	}
	if vc == nil {
		t.Fatal("NewVaultClient returned nil")
	}
	if vc.mountPath != "secret" {
		t.Fatalf("mountPath: got %q, want %q", vc.mountPath, "secret")
	}
}

// TestNewVaultClientDefaults verifies NewVaultClient with empty addr and token.
func TestNewVaultClientDefaults(t *testing.T) {
	// Use a real default config; we only care the call succeeds.
	vc, err := NewVaultClient("", "", "kv")
	if err != nil {
		t.Fatalf("NewVaultClient with defaults: %v", err)
	}
	if vc == nil {
		t.Fatal("NewVaultClient returned nil with defaults")
	}
}

// TestVaultClientGetNilSecret verifies Get returns an error when vault returns nil.
func TestVaultClientGetNilSecret(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return a valid JSON null body — vault client will parse secret as nil.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`null`))
	}))
	defer ts.Close()

	vc := vaultClientForTest(t, ts, "secret")
	_, err := vc.Get(context.Background(), "myapp/db")
	if err == nil {
		t.Fatal("Get: expected error for nil secret, got nil")
	}
}

// TestVaultClientGetMissingDataKey verifies Get returns an error when the
// vault response is missing the inner "data" key.
func TestVaultClientGetMissingDataKey(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]interface{}{
			"data": map[string]interface{}{
				// No "data" key inside — only metadata-like field.
				"metadata": map[string]interface{}{"version": 1},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer ts.Close()

	vc := vaultClientForTest(t, ts, "secret")
	_, err := vc.Get(context.Background(), "myapp/db")
	if err == nil {
		t.Fatal("Get: expected error for missing 'data' key, got nil")
	}
	if !strings.Contains(err.Error(), "missing 'data' key") {
		t.Fatalf("Get: error %q should mention missing 'data' key", err)
	}
}

// TestVaultClientGetDataNotMap verifies Get returns an error when the inner
// "data" field is not a map[string]interface{}.
func TestVaultClientGetDataNotMap(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]interface{}{
			"data": map[string]interface{}{
				"data": "not-a-map",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer ts.Close()

	vc := vaultClientForTest(t, ts, "secret")
	_, err := vc.Get(context.Background(), "myapp/db")
	if err == nil {
		t.Fatal("Get: expected error for non-map data, got nil")
	}
	if !strings.Contains(err.Error(), "is not a map") {
		t.Fatalf("Get: error %q should mention 'is not a map'", err)
	}
}

// TestVaultClientGetNonStringValue verifies Get converts non-string values via fmt.Sprintf.
func TestVaultClientGetNonStringValue(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Encode an integer value for "port" — JSON numbers decode as float64.
		body := map[string]interface{}{
			"data": map[string]interface{}{
				"data": map[string]interface{}{
					"port": 5432,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer ts.Close()

	vc := vaultClientForTest(t, ts, "secret")
	data, err := vc.Get(context.Background(), "myapp/db")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if data["port"] == "" {
		t.Fatal("Get: 'port' should have been formatted as a string")
	}
}

// TestVaultClientGetHTTPError verifies Get returns an error on a non-200 response.
func TestVaultClientGetHTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()

	vc := vaultClientForTest(t, ts, "secret")
	_, err := vc.Get(context.Background(), "myapp/db")
	if err == nil {
		t.Fatal("Get: expected error for 403 response, got nil")
	}
}

// TestExpandAllVaultCache verifies that ExpandAll fetches a vault path only once
// when multiple fields reference the same path.
func TestExpandAllVaultCache(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		body := map[string]interface{}{
			"data": map[string]interface{}{
				"data": map[string]interface{}{
					"user":     "admin",
					"password": "s3cr3t",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer ts.Close()

	vc := vaultClientForTest(t, ts, "secret")
	fields := []string{
		"${vault:myapp/db#user}",
		"${vault:myapp/db#password}",
	}
	out, err := ExpandAll(fields, vc, context.Background())
	if err != nil {
		t.Fatalf("ExpandAll: %v", err)
	}
	if out[0] != "admin" {
		t.Fatalf("out[0]: got %q, want 'admin'", out[0])
	}
	if out[1] != "s3cr3t" {
		t.Fatalf("out[1]: got %q, want 's3cr3t'", out[1])
	}
	if callCount != 1 {
		t.Fatalf("vault called %d times, want 1 (cache should prevent re-fetch)", callCount)
	}
}

// TestExpandAllVaultKeyNotFound verifies ExpandAll returns an error when a key
// does not exist in the fetched vault secret.
func TestExpandAllVaultKeyNotFound(t *testing.T) {
	ts := httptest.NewServer(mockVaultHandler(map[string]string{
		"password": "s3cr3t",
	}))
	defer ts.Close()

	vc := vaultClientForTest(t, ts, "secret")
	fields := []string{"${vault:myapp/db#nonexistent_key}"}
	_, err := ExpandAll(fields, vc, context.Background())
	if err == nil {
		t.Fatal("ExpandAll: expected error for missing key, got nil")
	}
	if !strings.Contains(err.Error(), "not found in vault path") {
		t.Fatalf("ExpandAll: error %q should mention 'not found in vault path'", err)
	}
}

// TestExpandAllVaultFetchError verifies ExpandAll propagates a vault fetch error.
func TestExpandAllVaultFetchError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()

	vc := vaultClientForTest(t, ts, "secret")
	fields := []string{"${vault:myapp/db#password}"}
	_, err := ExpandAll(fields, vc, context.Background())
	if err == nil {
		t.Fatal("ExpandAll: expected error from vault 403, got nil")
	}
}

// TestExpandAllVaultMultipleErrorsStopsAtFirst verifies that when two vault
// placeholders are in the same field and the first fails, the error is returned
// without attempting the second.
func TestExpandAllVaultMultipleErrorsStopsAtFirst(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()

	vc := vaultClientForTest(t, ts, "secret")
	// Two different vault paths in the same field string.
	fields := []string{"${vault:path1/key#field1} and ${vault:path2/key#field2}"}
	_, err := ExpandAll(fields, vc, context.Background())
	if err == nil {
		t.Fatal("ExpandAll: expected error, got nil")
	}
	// Only one HTTP call should be made; second placeholder skipped due to lastErr.
	if callCount != 1 {
		t.Fatalf("vault called %d times, want 1 (stop-on-first-error)", callCount)
	}
}

// TestExpandAllVaultAndEnvMixed verifies that a field with both a vault placeholder
// and an env-var placeholder is expanded correctly.
func TestExpandAllVaultAndEnvMixed(t *testing.T) {
	ts := httptest.NewServer(mockVaultHandler(map[string]string{
		"password": "vaultpass",
	}))
	defer ts.Close()

	vc := vaultClientForTest(t, ts, "secret")
	t.Setenv("RPLB_TEST_MIXED_HOST", "localhost")
	fields := []string{"${vault:myapp/db#password}@${RPLB_TEST_MIXED_HOST}"}
	out, err := ExpandAll(fields, vc, context.Background())
	if err != nil {
		t.Fatalf("ExpandAll mixed: %v", err)
	}
	if out[0] != "vaultpass@localhost" {
		t.Fatalf("ExpandAll mixed: got %q, want 'vaultpass@localhost'", out[0])
	}
}
