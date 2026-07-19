package tcpproxy

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/config"
)

// echoServer stands up a TCP listener that echoes back everything it receives.
// It reports how many client connections it accepted via the returned counter.
func echoServer(t *testing.T) (addr string, hits *int32, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	var count int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&count, 1)
			wg.Add(1)
			go func(conn net.Conn) {
				defer wg.Done()
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()
	stop = func() {
		_ = ln.Close()
		wg.Wait()
	}
	return ln.Addr().String(), &count, stop
}

// newProxyWithBackends builds a round-robin balancer over the given host:port
// addrs and starts a tcpproxy on an ephemeral port. Returns the proxy address.
func newProxyWithBackends(t *testing.T, addrs ...string) (*Proxy, string) {
	t.Helper()
	rr := balancer.NewRoundRobin()
	for _, a := range addrs {
		// Give a bare host:port so hostPort passes it through as-is.
		rr.Add(balancer.NewBackend(backendCfg(a)))
	}
	p := NewProxy(rr, 2*time.Second)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	go func() { _ = p.Serve(ln) }()
	return p, ln.Addr().String()
}

// backendCfg builds a BackendConfig with just a URL. Backends are given bare
// host:port strings so hostPort passes them through as-is.
func backendCfg(url string) config.BackendConfig {
	return config.BackendConfig{URL: url}
}

func roundTrip(t *testing.T, proxyAddr string, payload []byte) []byte {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	return buf
}

func TestProxyEchoSingleBackend(t *testing.T) {
	addr, hits, stopEcho := echoServer(t)
	defer stopEcho()

	p, proxyAddr := newProxyWithBackends(t, addr)
	defer p.Stop()

	payload := []byte("hello l4 proxy")
	got := roundTrip(t, proxyAddr, payload)
	if string(got) != string(payload) {
		t.Fatalf("echo mismatch: got %q want %q", got, payload)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("expected 1 backend hit, got %d", n)
	}
}

func TestProxyTwoBackendsBothReceive(t *testing.T) {
	addr1, hits1, stop1 := echoServer(t)
	defer stop1()
	addr2, hits2, stop2 := echoServer(t)
	defer stop2()

	p, proxyAddr := newProxyWithBackends(t, addr1, addr2)
	defer p.Stop()

	// Round-robin over two backends: 4 requests -> 2 each.
	for i := 0; i < 4; i++ {
		payload := []byte("ping")
		got := roundTrip(t, proxyAddr, payload)
		if string(got) != "ping" {
			t.Fatalf("request %d: got %q", i, got)
		}
	}
	if n := atomic.LoadInt32(hits1); n == 0 {
		t.Fatalf("backend 1 received no traffic")
	}
	if n := atomic.LoadInt32(hits2); n == 0 {
		t.Fatalf("backend 2 received no traffic")
	}
}

func TestProxyBackendReleasesConn(t *testing.T) {
	addr, _, stopEcho := echoServer(t)
	defer stopEcho()

	rr := balancer.NewRoundRobin()
	be := balancer.NewBackend(backendCfg(addr))
	rr.Add(be)
	p := NewProxy(rr, 2*time.Second)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = p.Serve(ln) }()
	defer p.Stop()

	got := roundTrip(t, ln.Addr().String(), []byte("x"))
	if string(got) != "x" {
		t.Fatalf("echo mismatch: %q", got)
	}
	// After the client closes, the handler should DecrConn. Poll briefly.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if be.GetActiveConns() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("backend ActiveConns did not return to 0, got %d", be.GetActiveConns())
}

func TestStopUnblocksServe(t *testing.T) {
	rr := balancer.NewRoundRobin()
	p := NewProxy(rr, time.Second)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- p.Serve(ln) }()

	// Give Serve a moment to enter its accept loop.
	time.Sleep(20 * time.Millisecond)
	if err := p.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve returned error on clean stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after Stop")
	}
}

func TestStopClosesActiveConns(t *testing.T) {
	// Backend that accepts but never responds, so the pipe stays open until Stop.
	backLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	defer backLn.Close()
	go func() {
		for {
			c, err := backLn.Accept()
			if err != nil {
				return
			}
			// Hold the conn without echoing.
			go func(conn net.Conn) { io.Copy(io.Discard, conn) }(c)
		}
	}()

	p, proxyAddr := newProxyWithBackends(t, backLn.Addr().String())

	c, err := net.DialTimeout("tcp", proxyAddr, time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("data")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// Stop must force the active client conn closed and return.
	done := make(chan struct{})
	go func() { _ = p.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return with active conns")
	}

	// The client read should now observe EOF/closed.
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1)
	// Any error (EOF or connection reset) is acceptable; the conn was torn down.
	if _, err := c.Read(buf); err == nil {
		t.Fatal("expected client conn to be closed after Stop")
	}
}

func TestHostPort(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"http://example.com", "example.com:80", false},
		{"https://example.com", "example.com:443", false},
		{"http://example.com:8080", "example.com:8080", false},
		{"ws://h:9000", "h:9000", false},
		{"127.0.0.1:5000", "127.0.0.1:5000", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got, err := hostPort(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("hostPort(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("hostPort(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("hostPort(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
