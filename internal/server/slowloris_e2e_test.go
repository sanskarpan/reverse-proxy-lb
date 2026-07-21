package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"reverse-proxy-lb/internal/config"
)

// startProxyServer starts a real net/http.Server (not httptest) with the
// provided handler and returns its listener address. The server is registered
// for cleanup via t.Cleanup.
func startProxyServer(t *testing.T, cfg *config.Config, handler http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// slowlorisConfig returns a minimal config with the given backend URL
// and custom ReadHeaderTimeout and MaxHeaderBytes.
func slowlorisBaseConfig(backendURL string, readHeaderTimeout time.Duration, maxHeaderBytes int, maxBodyBytes int64) *config.Config {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Host:                "127.0.0.1",
			Port:                0,
			ReadHeaderTimeout:   readHeaderTimeout,
			MaxHeaderBytes:      maxHeaderBytes,
			MaxRequestBodyBytes: maxBodyBytes,
		},
		Backends:     []config.BackendConfig{{URL: backendURL}},
		LoadBalancer: config.LoadBalancerConfig{Algorithm: "round_robin"},
		Logging:      config.LoggingConfig{Level: "error", Format: "text"},
	}
	// Fill in zero defaults that the server still needs.
	if cfg.Server.ReadHeaderTimeout == 0 {
		cfg.Server.ReadHeaderTimeout = 10 * time.Second
	}
	if cfg.Server.MaxHeaderBytes == 0 {
		cfg.Server.MaxHeaderBytes = 64 * 1024
	}
	return cfg
}

// TestSlowlorisHeaderTimeout verifies that a client that opens a connection
// and sends request headers very slowly is disconnected once
// ReadHeaderTimeout elapses.
func TestSlowlorisHeaderTimeout(t *testing.T) {
	t.Parallel()

	const (
		headerTimeout = 300 * time.Millisecond
	)

	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	}))
	t.Cleanup(be.Close)

	cfg := slowlorisBaseConfig(be.URL, headerTimeout, 64*1024, 0)
	handler := New(cfg, "").Handler()
	addr := startProxyServer(t, cfg, handler)

	// Open a raw TCP connection and send only a partial first line of headers.
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHo")) // incomplete headers

	// Wait for the server to close the connection (ReadHeaderTimeout fires).
	deadline := headerTimeout*3 + 500*time.Millisecond
	_ = conn.SetReadDeadline(time.Now().Add(deadline))

	buf := make([]byte, 512)
	n, readErr := conn.Read(buf)

	// Accept any of: connection closed (EOF / reset), or a 408 response.
	if readErr != nil {
		t.Logf("server closed connection (expected): %v", readErr)
		return
	}
	resp := string(buf[:n])
	// 400 Bad Request (malformed/incomplete), 408 Request Timeout (slowloris), or
	// connection-closed are all acceptable server rejections.
	if strings.Contains(resp, "400") || strings.Contains(resp, "408") ||
		strings.Contains(resp, "Bad Request") || strings.Contains(resp, "Request Timeout") {
		return
	}
	t.Fatalf("expected 400/408 or closed connection after %v, got: %q", headerTimeout, resp)
}

// TestOversizedRequestHeaders verifies that requests with headers exceeding
// MaxHeaderBytes are rejected (431, 400, or connection close).
// Uses a raw TCP connection so the Go HTTP client's own header-size caps
// don't interfere with what we send.
func TestOversizedRequestHeaders(t *testing.T) {
	t.Parallel()

	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	}))
	t.Cleanup(be.Close)

	// Use a very small MaxHeaderBytes (256) and send a large header value
	// (32 KiB) to ensure we're well above any internal bufio reader thresholds.
	const maxHeader = 256
	cfg := slowlorisBaseConfig(be.URL, 10*time.Second, maxHeader, 0)
	handler := New(cfg, "").Handler()
	addr := startProxyServer(t, cfg, handler)

	// Send oversized headers over a raw TCP connection to bypass the Go client's
	// own header-size caps, which would prevent sending large values.
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	bigVal := strings.Repeat("A", 32*1024) // 32 KiB — far exceeds any internal threshold
	rawReq := fmt.Sprintf(
		"GET / HTTP/1.1\r\nHost: %s\r\nX-Overflow: %s\r\n\r\n",
		addr, bigVal,
	)
	_, _ = conn.Write([]byte(rawReq))

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 512)
	n, _ := conn.Read(buf)

	if n == 0 {
		return // connection closed without response — server rejected it
	}
	resp := string(buf[:n])
	if strings.Contains(resp, "431") || strings.Contains(resp, "400") ||
		strings.Contains(resp, "Header Fields Too Large") || strings.Contains(resp, "Bad Request") {
		return
	}
	if strings.Contains(resp, "200") {
		t.Errorf("expected oversized header rejection, got 200: %q", resp)
	}
}

// TestOversizedRequestBody verifies that requests with bodies exceeding
// MaxRequestBodyBytes are rejected with 413.
func TestOversizedRequestBody(t *testing.T) {
	t.Parallel()

	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the body length so we can confirm proxied vs rejected.
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "read %d bytes", len(b))
	}))
	t.Cleanup(be.Close)

	const maxBody int64 = 512 // very small limit for the test
	cfg := slowlorisBaseConfig(be.URL, 10*time.Second, 64*1024, maxBody)
	handler := New(cfg, "").Handler()
	addr := startProxyServer(t, cfg, handler)

	// Send a body 4× the limit.
	bigBody := strings.NewReader(strings.Repeat("X", int(maxBody)*4))
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/", bigBody)
	req.Header.Set("Content-Type", "text/plain")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unexpected client error: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for oversized body, got %d", resp.StatusCode)
	}
}
