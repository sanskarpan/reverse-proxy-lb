package proxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reverse-proxy-lb/internal/balancer"
	"reverse-proxy-lb/internal/config"
	"reverse-proxy-lb/internal/tcpproxy"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// This file drives §5 (upstream transport, HTTP/2, L4, WebSocket, client-cancel)
// features end-to-end through a REAL network stack: real backends bound to real
// listeners, the proxy fronted by a real httptest.NewServer listener, and clients
// speaking real TCP/HTTP. Nothing on the request path is stubbed. It reuses the
// helpers in resilience_e2e_test.go (newHitBackend, frontFor, eventually).
//
// Scenarios:
//   - Config-driven upstream ResponseHeaderTimeout: a short header timeout with a
//     stalling backend abandons the attempt and fails over to a healthy backend.
//   - HTTP/2 (h2c) upstream: with Upstream.HTTP2=true the h2c backend reports
//     "HTTP/2.0"; with HTTP2 off the same backend reports "HTTP/1.1".
//   - L4 TCP proxy: a TCP echo backend echoes bytes end-to-end through the L4
//     proxy, and two backends both receive traffic.
//   - WebSocket idle timeout: a raw upgraded connection that goes idle beyond the
//     configured WebSocket.IdleTimeout is closed by the proxy.
//   - Client-disconnect cancellation: a backend blocking on its request context is
//     unblocked promptly when the client closes the connection.

// ----------------------------------------------------------------------------
// Config-driven upstream ResponseHeaderTimeout + failover
// ----------------------------------------------------------------------------

// A backend that never sends response headers (stalls before WriteHeader) must be
// abandoned once the config-driven ResponseHeaderTimeout elapses. Because a second
// healthy backend exists and GET is idempotent, the request still succeeds via
// failover. This exercises the config path (config.UpstreamConfig.ResponseHeaderTimeout
// -> proxy.buildTransport -> http.Transport.ResponseHeaderTimeout), NOT the retry
// PerTryTimeout knob.
func TestE2E_UpstreamResponseHeaderTimeout_AbandonsAndFailsOver(t *testing.T) {
	stallObservedCancel := make(chan struct{})
	var once sync.Once
	// This backend accepts the connection and reads the request but never writes
	// any response header, so the proxy's ResponseHeaderTimeout must fire.
	stall, stallHits := newHitBackend(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done(): // proxy abandons the attempt; server cancels ctx
			once.Do(func() { close(stallObservedCancel) })
		case <-time.After(5 * time.Second):
		}
	})
	defer stall.Close()

	fast, fastHits := newHitBackend(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "fast-ok")
	})
	defer fast.Close()

	rr := balancer.NewRoundRobin()
	// Stall backend added first so round-robin makes it the primary attempt.
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: stall.URL, MaxConns: 100}))
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: fast.URL, MaxConns: 100}))

	// Drive the abandonment purely via the config-level upstream header timeout.
	up := config.UpstreamConfig{ResponseHeaderTimeout: 150 * time.Millisecond}
	p := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, up)

	front, client, closeFront := frontFor(t, p)
	defer closeFront()

	start := time.Now()
	resp, err := client.Get(front + "/")
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 via failover after response-header timeout, got %d", resp.StatusCode)
	}
	if string(body) != "fast-ok" {
		t.Errorf("expected fast backend body after failover, got %q", body)
	}
	if elapsed > 3*time.Second {
		t.Errorf("response-header timeout did not abandon the stalling backend promptly: took %v", elapsed)
	}
	if atomic.LoadInt64(stallHits) < 1 {
		t.Error("expected the stall backend to have been attempted first")
	}
	if atomic.LoadInt64(fastHits) < 1 {
		t.Error("expected failover to reach the fast backend")
	}
	// The stalling backend's handler must observe the attempt being abandoned.
	if !eventually(t, 2*time.Second, func() bool {
		select {
		case <-stallObservedCancel:
			return true
		default:
			return false
		}
	}) {
		t.Error("stall backend did not observe the abandoned attempt (context cancellation)")
	}
}

// ----------------------------------------------------------------------------
// HTTP/2 (h2c) upstream
// ----------------------------------------------------------------------------

// newH2CBackend stands up a cleartext HTTP/2 (h2c) backend that reports r.Proto so
// the test can observe whether the proxy reached it over HTTP/2 or HTTP/1.1. The
// same handler serves both protocols (h2c.NewHandler upgrades HTTP/2 prior-knowledge
// connections while still serving HTTP/1.1).
func newH2CBackend(t *testing.T) (urlStr string, protoSeen func() string, stop func()) {
	t.Helper()
	var mu sync.Mutex
	var lastProto string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastProto = r.Proto
		mu.Unlock()
		io.WriteString(w, r.Proto)
	})
	h2s := &http2.Server{}
	srv := &http.Server{Handler: h2c.NewHandler(h, h2s)}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("h2c listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()

	stop = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	protoSeen = func() string {
		mu.Lock()
		defer mu.Unlock()
		return lastProto
	}
	return "http://" + ln.Addr().String(), protoSeen, stop
}

// With Upstream.HTTP2=true and an http:// (cleartext) backend, the proxy must use an
// h2c transport (§5.3) and the backend must observe "HTTP/2.0". The response body,
// which echoes r.Proto, confirms the end-to-end protocol as seen by the backend.
func TestE2E_H2CUpstream_HTTP2Enabled_BackendSeesHTTP2(t *testing.T) {
	backendURL, protoSeen, stop := newH2CBackend(t)
	defer stop()

	rr := balancer.NewRoundRobin()
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: backendURL, MaxConns: 100}))

	p := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{HTTP2: true})

	front, client, closeFront := frontFor(t, p)
	defer closeFront()

	resp, err := client.Get(front + "/")
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from h2c backend, got %d", resp.StatusCode)
	}
	if got := strings.TrimSpace(string(body)); got != "HTTP/2.0" {
		t.Errorf("with HTTP2 enabled the backend must see HTTP/2.0, response body reported %q", got)
	}
	if p := protoSeen(); p != "HTTP/2.0" {
		t.Errorf("with HTTP2 enabled the backend must observe HTTP/2.0, saw %q", p)
	}
}

// With Upstream.HTTP2=false (default) against the SAME h2c-capable backend, the
// proxy uses the standard HTTP/1.1 transport and the backend must observe HTTP/1.1.
// This proves the config knob actually changes the negotiated protocol rather than
// the backend always answering h2.
func TestE2E_H2CUpstream_HTTP2Disabled_BackendSeesHTTP11(t *testing.T) {
	backendURL, protoSeen, stop := newH2CBackend(t)
	defer stop()

	rr := balancer.NewRoundRobin()
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: backendURL, MaxConns: 100}))

	p := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{HTTP2: false})

	front, client, closeFront := frontFor(t, p)
	defer closeFront()

	resp, err := client.Get(front + "/")
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from backend over HTTP/1.1, got %d", resp.StatusCode)
	}
	if got := strings.TrimSpace(string(body)); got != "HTTP/1.1" {
		t.Errorf("with HTTP2 disabled the backend must see HTTP/1.1, response body reported %q", got)
	}
	if p := protoSeen(); p != "HTTP/1.1" {
		t.Errorf("with HTTP2 disabled the backend must observe HTTP/1.1, saw %q", p)
	}
}

// ----------------------------------------------------------------------------
// L4 (raw TCP) proxy
// ----------------------------------------------------------------------------

// tcpEchoBackend stands up a TCP listener that echoes everything it receives and
// counts accepted connections. It returns a bare host:port so tcpproxy.hostPort
// passes it through as a literal dial address.
func tcpEchoBackend(t *testing.T) (addr string, conns *int32, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp echo listen: %v", err)
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

// Enable the L4 proxy on an ephemeral port with two TCP echo backends. Bytes must
// echo end-to-end through the L4 proxy, and across many connections both backends
// must receive traffic (round-robin distribution over the raw TCP layer).
func TestE2E_L4TCPProxy_EchoAndBothBackendsUsed(t *testing.T) {
	addr1, conns1, stop1 := tcpEchoBackend(t)
	defer stop1()
	addr2, conns2, stop2 := tcpEchoBackend(t)
	defer stop2()

	rr := balancer.NewRoundRobin()
	// Bare host:port so tcpproxy dials them as literal addresses.
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: addr1, MaxConns: 100}))
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: addr2, MaxConns: 100}))

	// Bind an explicit ephemeral listener so the chosen port is known, then Serve on
	// it (tcpproxy.Start binds internally and does not surface the :0 port).
	l4 := tcpproxy.NewProxy(rr, 2*time.Second)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("l4 listener: %v", err)
	}
	go func() { _ = l4.Serve(ln) }()
	defer l4.Stop()
	proxyAddr := ln.Addr().String()

	// Echo end-to-end: each connection sends a unique payload and must read it back.
	const n = 20
	for i := 0; i < n; i++ {
		payload := make([]byte, 32)
		if _, err := rand.Read(payload); err != nil {
			t.Fatalf("rand: %v", err)
		}
		c, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial l4 proxy: %v", err)
		}
		if err := c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("set deadline: %v", err)
		}
		if _, err := c.Write(payload); err != nil {
			t.Fatalf("write: %v", err)
		}
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(c, got); err != nil {
			t.Fatalf("read echo: %v", err)
		}
		if string(got) != string(payload) {
			t.Fatalf("echo mismatch on conn %d: sent %x got %x", i, payload, got)
		}
		c.Close()
	}

	// Both backends must have received at least one connection (traffic spread).
	got1 := atomic.LoadInt32(conns1)
	got2 := atomic.LoadInt32(conns2)
	if got1 == 0 || got2 == 0 {
		t.Errorf("L4 proxy did not spread traffic across both backends: backend1=%d backend2=%d", got1, got2)
	}
	if int(got1+got2) != n {
		t.Errorf("expected %d total backend connections, got %d (b1=%d b2=%d)", n, got1+got2, got1, got2)
	}
}

// ----------------------------------------------------------------------------
// WebSocket idle timeout
// ----------------------------------------------------------------------------

// wsEchoBackend is a backend that performs a raw HTTP Upgrade (101) hijack and then
// echoes bytes on the upgraded connection until it closes. It intentionally does NOT
// speak the full RFC6455 framing: the proxy's idle-timeout enforcement operates below
// the framing layer (on the raw net.Conn), so a byte-echo upgrade is sufficient to
// drive the read-deadline path in wsConn.Read.
func wsEchoBackend(t *testing.T) (urlStr string, stop func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "expected upgrade", http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, brw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		// Complete the upgrade handshake.
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = brw.Flush()
		// Echo raw bytes until the connection is torn down.
		buf := make([]byte, 1024)
		for {
			n, err := brw.Read(buf)
			if n > 0 {
				_, _ = conn.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}))
	return srv.URL, srv.Close
}

// With WebSocket.IdleTimeout configured, a raw upgraded connection that goes idle
// (sends no bytes) beyond the timeout must be torn down by the proxy: the client's
// read on the upgraded connection returns EOF/error once the proxy closes it. This
// drives the real hijack path (captureWriter.Hijack -> newWSConn -> wsConn.Read
// read-deadline enforcement, §5.7).
func TestE2E_WebSocketIdleTimeout_ClosesIdleConnection(t *testing.T) {
	backendURL, stopBackend := wsEchoBackend(t)
	defer stopBackend()

	rr := balancer.NewRoundRobin()
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: backendURL, MaxConns: 100}))

	p := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})
	const idle = 200 * time.Millisecond
	p.SetWebSocket(config.WebSocketConfig{IdleTimeout: idle})

	front, _, closeFront := frontFor(t, p)
	defer closeFront()

	// Dial the front proxy directly and perform the Upgrade handshake by hand so we
	// own the raw connection and can observe when the proxy closes it.
	host := strings.TrimPrefix(front, "http://")
	conn, err := net.DialTimeout("tcp", host, 2*time.Second)
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer conn.Close()

	req := "GET / HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write upgrade: %v", err)
	}

	br := bufio.NewReader(conn)
	// Read the 101 status line to confirm the upgrade succeeded through the proxy.
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("expected 101 Switching Protocols through proxy, got %q", statusLine)
	}
	// Drain the rest of the response headers (until the blank line).
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read upgrade headers: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	// Now go idle: send nothing. The proxy must close the connection after the idle
	// timeout elapses. A blocked Read must return (EOF/error) well within a bound
	// comfortably larger than the idle timeout.
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	start := time.Now()
	buf := make([]byte, 64)
	_, readErr := br.Read(buf)
	elapsed := time.Since(start)

	if readErr == nil {
		t.Fatalf("expected the idle WebSocket connection to be closed by the proxy, but Read returned data with no error")
	}
	// If our own read deadline fired (3s) rather than the proxy closing (~200ms),
	// the idle timeout did not take effect.
	if ne, ok := readErr.(net.Error); ok && ne.Timeout() && elapsed >= 3*time.Second {
		t.Fatalf("idle timeout did not close the connection; our read deadline fired after %v", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("proxy took too long (%v) to close an idle WebSocket connection with a %v idle timeout", elapsed, idle)
	}
	t.Logf("idle WebSocket connection closed by proxy after %v (idle timeout %v), err=%v", elapsed, idle, readErr)
}

// ----------------------------------------------------------------------------
// Client-disconnect cancellation
// ----------------------------------------------------------------------------

// A backend blocking on its request context must be unblocked promptly when the
// client cancels/closes the connection. The proxy propagates the front-connection
// cancellation to the upstream request context (r.Context()), so the backend's
// <-r.Context().Done() fires shortly after the client goes away.
func TestE2E_ClientDisconnect_CancelsBackend(t *testing.T) {
	backendUnblocked := make(chan time.Time, 1)
	backendEntered := make(chan struct{}, 1)
	var once sync.Once
	backend, _ := newHitBackend(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(backendEntered) })
		// Block until the client's cancellation propagates through the proxy, or a
		// generous ceiling so a broken propagation fails the test rather than hangs.
		select {
		case <-r.Context().Done():
			backendUnblocked <- time.Now()
		case <-time.After(5 * time.Second):
			backendUnblocked <- time.Now()
		}
	})
	defer backend.Close()

	rr := balancer.NewRoundRobin()
	rr.Add(balancer.NewBackend(config.BackendConfig{URL: backend.URL, MaxConns: 100}))

	p := New(rr, nil, config.RetryConfig{}, "round_robin", nil, nil, config.UpstreamConfig{})

	front, _, closeFront := frontFor(t, p)
	defer closeFront()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, front+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	// Fire the request on its own goroutine; it will hang in the backend handler.
	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		client := &http.Client{}
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	// Wait until the backend is actually handling the request, then disconnect.
	select {
	case <-backendEntered:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatalf("backend never received the request")
	}

	cancelAt := time.Now()
	cancel() // client disconnects

	select {
	case unblockedAt := <-backendUnblocked:
		delay := unblockedAt.Sub(cancelAt)
		if delay > 2*time.Second {
			t.Errorf("backend was not unblocked promptly after client disconnect: took %v", delay)
		}
		t.Logf("backend observed client-disconnect cancellation %v after the client went away", delay)
	case <-time.After(4 * time.Second):
		t.Fatalf("backend was not unblocked by the client disconnect (context did not propagate)")
	}

	<-reqDone
}
