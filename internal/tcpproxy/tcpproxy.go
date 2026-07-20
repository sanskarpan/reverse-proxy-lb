// Package tcpproxy implements an L4 (raw TCP) reverse proxy. Each accepted
// client connection is routed to a backend chosen by a balancer.Balancer and
// bytes are shuttled bidirectionally between client and backend until either
// side closes.
package tcpproxy

import (
	"errors"
	"io"
	"net"
	"net/url"
	"sync"
	"time"

	"reverse-proxy-lb/internal/balancer"
)

// Proxy is an L4 TCP reverse proxy. It accepts client connections on a listener
// and forwards them to backends selected by the balancer.
type Proxy struct {
	balancer    balancer.Balancer
	dialTimeout time.Duration

	mu       sync.Mutex
	ln       net.Listener
	conns    map[net.Conn]struct{}
	closed   bool
	closedCh chan struct{}

	wg sync.WaitGroup
}

// NewProxy constructs a Proxy that selects backends via b and dials each backend
// with the given timeout. A non-positive dialTimeout means net.Dial has no
// explicit timeout beyond the OS default.
func NewProxy(b balancer.Balancer, dialTimeout time.Duration) *Proxy {
	return &Proxy{
		balancer:    b,
		dialTimeout: dialTimeout,
		conns:       make(map[net.Conn]struct{}),
		closedCh:    make(chan struct{}),
	}
}

// Start binds a listener on addr and serves in the background. Serve runs on its
// own goroutine; use Stop to shut down. Returns an error if the listener cannot
// be created.
func (p *Proxy) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		_ = p.Serve(ln)
	}()
	return nil
}

// Serve accepts connections on ln until the listener is closed (via Stop) and
// proxies each to a backend. It takes ownership of ln and closes it on return.
// Serve returns nil on a clean shutdown triggered by Stop.
func (p *Proxy) Serve(ln net.Listener) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = ln.Close()
		return errors.New("tcpproxy: closed")
	}
	p.ln = ln
	p.mu.Unlock()

	for {
		client, err := ln.Accept()
		if err != nil {
			// If we've been asked to stop, Accept fails because the listener is
			// closed; report that as a clean shutdown.
			if p.isClosed() {
				return nil
			}
			// Retry on temporary/transient accept errors; bail otherwise.
			var ne net.Error
			//lint:ignore SA1019 ne.Temporary() is deprecated but retained for accept-loop backoff; timeouts are the common case
			if errors.As(err, &ne) && ne.Temporary() {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			return err
		}

		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handle(client)
		}()
	}
}

// handle proxies a single client connection to a balancer-selected backend.
func (p *Proxy) handle(client net.Conn) {
	defer client.Close()

	backend, err := p.balancer.Next()
	if err != nil {
		return
	}
	// Next reserved the backend via IncrConn; always release it.
	defer backend.DecrConn()

	addr, err := hostPort(backend.URL)
	if err != nil {
		return
	}

	upstream, err := net.DialTimeout("tcp", addr, p.dialTimeout)
	if err != nil {
		return
	}
	defer upstream.Close()

	// Track both conns so Stop can force them closed.
	p.track(client)
	p.track(upstream)
	defer p.untrack(client)
	defer p.untrack(upstream)

	// Pipe both directions. When either side closes, unblock the other by
	// closing both connections so both copies return.
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, client)
		closeBoth()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, upstream)
		closeBoth()
	}()
	wg.Wait()
}

// Stop shuts the proxy down: it closes the listener (unblocking Serve), forces
// all active connections closed, and waits for Serve and all in-flight handlers
// to finish. Stop is idempotent.
func (p *Proxy) Stop() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	close(p.closedCh)
	ln := p.ln
	// Snapshot and close all tracked conns.
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}

	p.wg.Wait()
	return nil
}

func (p *Proxy) isClosed() bool {
	select {
	case <-p.closedCh:
		return true
	default:
		return false
	}
}

func (p *Proxy) track(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		// Already shutting down: close immediately so we don't leak.
		_ = c.Close()
		return
	}
	p.conns[c] = struct{}{}
}

func (p *Proxy) untrack(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, c)
}

// hostPort derives a dialable host:port from a backend URL. If the URL omits a
// port, the scheme's default port is used (http/ws -> 80, https/wss -> 443).
// A bare host:port (no scheme) is accepted as-is.
func hostPort(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("tcpproxy: empty backend URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		// A bare "host:port" (no scheme) makes url.Parse choke on the colon
		// ("first path segment cannot contain colon"). Treat it as a literal
		// dial address.
		return raw, nil
	}
	// url.Parse on some bare "host:port" inputs treats the host as a scheme,
	// leaving an empty Host. Fall back to the raw string in that case.
	if u.Host == "" {
		return raw, nil
	}
	if u.Port() != "" {
		return u.Host, nil
	}
	port := defaultPort(u.Scheme)
	if port == "" {
		return "", errors.New("tcpproxy: no port in backend URL and unknown scheme")
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

func defaultPort(scheme string) string {
	switch scheme {
	case "http", "ws":
		return "80"
	case "https", "wss":
		return "443"
	default:
		return ""
	}
}
