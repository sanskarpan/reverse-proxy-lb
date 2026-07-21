//go:build !integration

package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBinary compiles the proxy binary into a temp directory and returns its
// path.  The build is cached by the Go toolchain so repeated calls within a
// test run are fast.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "proxy")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

// minimalValidConfig returns a minimal YAML config accepted by config.Load.
const minimalValidConfig = `
server:
  host: 127.0.0.1
  port: 18080
backends:
  - url: http://localhost:8080
load_balancer:
  algorithm: round_robin
logging:
  level: info
  format: json
metrics:
  enabled: false
  port: 19090
`

// writeTempConfig writes cfg to a temporary file and returns its path.
func writeTempConfig(t *testing.T, cfg string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(f, []byte(cfg), 0600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return f
}

// TestValidateFlag_ValidConfig exercises the --validate flag with a correct
// config.  The binary must exit 0 and print "config OK".
func TestValidateFlag_ValidConfig(t *testing.T) {
	bin := buildBinary(t)
	cfgFile := writeTempConfig(t, minimalValidConfig)

	cmd := exec.Command(bin, "--config", cfgFile, "--validate")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--validate on valid config exited non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "config OK") {
		t.Errorf("expected 'config OK' in output, got: %s", out)
	}
}

// TestValidateFlag_InvalidConfig confirms that --validate exits non-zero when
// the YAML is malformed.
func TestValidateFlag_InvalidConfig(t *testing.T) {
	bin := buildBinary(t)
	cfgFile := writeTempConfig(t, ":::not valid yaml:::")

	cmd := exec.Command(bin, "--config", cfgFile, "--validate")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit for invalid config YAML, got success\n%s", out)
	}
}

// TestMissingConfigFile confirms the binary exits non-zero when the config
// path does not exist (both in validate and normal mode).
func TestMissingConfigFile(t *testing.T) {
	bin := buildBinary(t)

	cmd := exec.Command(bin, "--config", "/nonexistent/path/config.yaml", "--validate")
	if err := cmd.Run(); err == nil {
		t.Fatal("expected non-zero exit for missing config file")
	}
}

// TestConfigFlagDefault exercises that the binary respects the --config flag
// by pointing it at a valid file and running --validate (avoids starting the
// server).
func TestConfigFlagDefault(t *testing.T) {
	bin := buildBinary(t)
	cfgFile := writeTempConfig(t, minimalValidConfig)

	cmd := exec.Command(bin, "--config", cfgFile, "--validate")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("unexpected error with explicit --config flag: %v\n%s", err, out)
	}
}

// TestOverrideFlags_HostPort verifies that --host and --port override flags
// are accepted without error when combined with --validate.
func TestOverrideFlags_HostPort(t *testing.T) {
	bin := buildBinary(t)
	cfgFile := writeTempConfig(t, minimalValidConfig)

	cmd := exec.Command(bin, "--config", cfgFile, "--validate", "--host", "127.0.0.1", "--port", "19999")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("override flags rejected during --validate: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "config OK") {
		t.Errorf("expected 'config OK' in output, got: %s", out)
	}
}

// TestOverrideFlag_LogLevel verifies the --log-level override flag is accepted.
func TestOverrideFlag_LogLevel(t *testing.T) {
	bin := buildBinary(t)
	cfgFile := writeTempConfig(t, minimalValidConfig)

	cmd := exec.Command(bin, "--config", cfgFile, "--validate", "--log-level", "debug")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--log-level flag rejected during --validate: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "config OK") {
		t.Errorf("expected 'config OK' in output, got: %s", out)
	}
}

// TestOverrideFlag_MetricsPort verifies the --metrics-port override flag is accepted.
func TestOverrideFlag_MetricsPort(t *testing.T) {
	bin := buildBinary(t)
	cfgFile := writeTempConfig(t, minimalValidConfig)

	cmd := exec.Command(bin, "--config", cfgFile, "--validate", "--metrics-port", "19091")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--metrics-port flag rejected during --validate: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "config OK") {
		t.Errorf("expected 'config OK' in output, got: %s", out)
	}
}

// TestHelpFlag verifies the binary prints usage information for -help / --help.
func TestHelpFlag(t *testing.T) {
	bin := buildBinary(t)

	for _, flag := range []string{"-help", "--help"} {
		cmd := exec.Command(bin, flag)
		out, _ := cmd.CombinedOutput() // -help exits with code 2 in Go's flag package
		combined := string(out)
		if !strings.Contains(combined, "config") && !strings.Contains(combined, "Usage") && !strings.Contains(combined, "flag") {
			t.Errorf("help output for %s does not mention flags: %s", flag, combined)
		}
	}
}

// TestUnknownFlag verifies that an unrecognised flag causes a non-zero exit.
func TestUnknownFlag(t *testing.T) {
	bin := buildBinary(t)

	cmd := exec.Command(bin, "--thisflagneverexists")
	if err := cmd.Run(); err == nil {
		t.Fatal("expected non-zero exit for unknown flag")
	}
}
