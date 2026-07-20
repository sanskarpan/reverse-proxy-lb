package secrets_test

import (
	"os"
	"testing"

	"reverse-proxy-lb/internal/config"
)

// TestConfigEnvExpansion verifies that config.Load() expands ${VAR} placeholders
// in the YAML file using secrets.Expand for TLS and backend fields.
func TestConfigEnvExpansion(t *testing.T) {
	t.Setenv("TEST_RPLB_TLS_CERT", "/tmp/cert.pem")

	yamlContent := `
backends:
  - url: http://127.0.0.1:9090
tls:
  cert_file: "${TEST_RPLB_TLS_CERT}"
`
	f, err := os.CreateTemp("", "rplb-config-*.yaml")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(f.Name())

	if _, err := f.WriteString(yamlContent); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	f.Close()

	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	if cfg.TLS.CertFile != "/tmp/cert.pem" {
		t.Fatalf("TLS.CertFile: got %q, want %q", cfg.TLS.CertFile, "/tmp/cert.pem")
	}
}
