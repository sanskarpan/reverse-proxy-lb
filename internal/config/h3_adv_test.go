package config

import (
	"strings"
	"testing"
)

// TestValidateHTTP3RequiresTLS is an adversarial test: enabling HTTP/3 without
// TLS must be rejected by the validator (QUIC mandates TLS 1.3).
func TestValidateHTTP3RequiresTLS(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://127.0.0.1:9999"
server:
  port: 8080
  http3:
    enabled: true
    port: 8080
tls:
  enabled: false
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error when http3.enabled=true and tls.enabled=false, got nil")
	}
	if !strings.Contains(err.Error(), "http3") && !strings.Contains(err.Error(), "tls") {
		t.Errorf("error message %q should mention http3 or tls", err.Error())
	}
}

// TestValidateHTTP3AllowedWithTLS confirms that HTTP/3 + TLS passes config
// validation (cert loading happens at runtime, not at Load time).
func TestValidateHTTP3AllowedWithTLS(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://127.0.0.1:9999"
server:
  port: 8080
  http3:
    enabled: true
    port: 8080
tls:
  enabled: true
`)
	_, err := Load(path)
	if err != nil {
		t.Fatalf("expected HTTP3+TLS to pass config validation, got: %v", err)
	}
}

// TestValidateHTTP3PortOutOfRange ensures that an HTTP/3 port outside 1-65535
// is caught at validation time.
func TestValidateHTTP3PortOutOfRange(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: "http://127.0.0.1:9999"
server:
  port: 8080
  http3:
    enabled: true
    port: 99999
tls:
  enabled: true
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error for http3.port=99999, got nil")
	}
	if !strings.Contains(err.Error(), "http3") {
		t.Errorf("error message %q should mention http3", err.Error())
	}
}
