# WebSocket Proxying

rplb proxies WebSocket connections transparently using HTTP upgrade tunneling. WebSocket connections are long-lived and stateful, so session affinity is essential — the same client must always connect to the same backend.

---

## How WebSocket proxying works

1. Client sends an HTTP Upgrade request (`Connection: Upgrade`, `Upgrade: websocket`).
2. rplb selects a backend and forwards the Upgrade request.
3. The upstream returns `101 Switching Protocols`.
4. rplb tunnels raw TCP bidirectionally between client and backend — it does not inspect WebSocket frames.
5. The connection is held open until either side closes it.

rplb uses `http.Hijacker` to take control of the underlying TCP connection after the 101 response, then performs a bidirectional copy with `io.Copy` in two goroutines.

---

## Configuration

### Basic WebSocket proxying

```yaml
backends:
  - url: "http://ws-server-1:8000"
    max_conns: 1000    # WebSocket connections are long-lived; set appropriately
  - url: "http://ws-server-2:8000"
    max_conns: 1000

load_balancer:
  algorithm: consistent_hash    # critical for session affinity
  consistent_hash:
    replicas: 150
    load_factor: 1.1

server:
  websocket:
    idle_timeout: 0             # 0 = no idle timeout (required for most WS apps)
    max_message_bytes: 0        # 0 = no frame size limit
```

### Route-specific WebSocket config

If only some paths are WebSocket:

```yaml
routes:
  - name: websocket
    path_prefix: "/ws/"
    algorithm: consistent_hash
    consistent_hash:
      replicas: 150
      load_factor: 1.1
    backends:
      - url: "http://ws-1:8000"
        max_conns: 5000
      - url: "http://ws-2:8000"
        max_conns: 5000

backends:
  - url: "http://api-1:8000"
    max_conns: 200

load_balancer:
  algorithm: round_robin    # default algorithm for non-WS routes
```

---

## Session affinity with consistent hash

WebSocket connections break if the client reconnects and lands on a different backend (server-side session state is lost). Use consistent hashing to pin connections:

**By request URL (default):**

The hash key defaults to the full request URL. If your WS clients connect to a stable path (e.g., `/ws/room/42`), this already provides affinity — the room ID in the URL hashes to a consistent backend.

**By cookie (user session):**

Inject a consistent hash key from a cookie using header rewrite:

```yaml
rewrite:
  request_headers_set:
    X-Session-ID: "${cookie:session_id}"

routes:
  - name: websocket
    path_prefix: "/ws/"
    algorithm: consistent_hash
```

The `${cookie:session_id}` placeholder extracts the `session_id` cookie value and sets it as the `X-Session-ID` header, which the consistent hash algorithm uses as its key.

**By IP hash (simpler alternative):**

```yaml
load_balancer:
  algorithm: ip_hash
```

IP hash is simpler but breaks for clients behind NAT (all users at the same office IP go to the same backend) and for mobile clients that change IPs mid-session.

---

## `max_conns` sizing for WebSocket

WebSocket connections are held open for the duration of the session — potentially hours. `max_conns` counts currently open connections, not request rate.

Sizing guidance:

```
max_conns ≈ peak_concurrent_websocket_connections_per_backend + 20% headroom
```

Example: if you expect 2000 concurrent WebSocket clients split across 2 backends:

```yaml
backends:
  - url: "http://ws-1:8000"
    max_conns: 1200    # 1000 expected + 20% headroom
  - url: "http://ws-2:8000"
    max_conns: 1200
```

---

## Idle timeout

For real-time applications (chat, collaboration, live feeds), set `websocket.idle_timeout: 0` to disable the idle timeout entirely. The connection stays open until the client or server closes it.

For batch-oriented WebSocket usage (e.g., file uploads), set an idle timeout to reclaim goroutines from stalled connections:

```yaml
server:
  websocket:
    idle_timeout: 300s    # close connections idle for 5 minutes
```

---

## Health checks for WebSocket backends

Active health checks work normally for WebSocket backends. Use an HTTP health check path (not the WebSocket endpoint):

```yaml
load_balancer:
  health_check:
    enabled: true
    type: http
    path: "/health"
    interval: 10s
```

The health check uses a normal HTTP GET, not a WebSocket handshake. Backends are marked unhealthy based on HTTP response status.

When a backend is marked unhealthy, existing WebSocket connections to it are not interrupted (the connection is already established). New connections are not routed to the unhealthy backend.

---

## Sticky sessions vs consistent hash

For WebSocket, consistent hash (by URL or session key) is preferred over cookie-based sticky sessions because:

1. WebSocket clients do not send cookies on every frame — only on the initial HTTP Upgrade.
2. Consistent hash maps the same key to the same backend even as the pool changes, with `1/n` disruption on backend add/remove.
3. Cookie sticky sessions are disrupted when the cookie expires or the client reconnects from a different process.

Use sticky cookies only if you cannot control the URL structure (e.g., all WebSocket clients connect to the same `/ws` path with no distinguishing information in the URL).

```yaml
load_balancer:
  algorithm: consistent_hash
  sticky:
    enabled: true
    cookie: "rplb_ws_affinity"
    ttl: 86400s    # 24 hours
```

With this config, on first connection the consistent hash selects a backend and sets the affinity cookie. On reconnect (within 24h), the cookie is used directly without hashing — faster and consistent even if the pool changes.

---

## Observability

WebSocket connections appear in the in-flight metrics for their entire duration:

```promql
# Current open WebSocket connections (approximately)
rplb_inflight_requests
```

Access log entries are emitted at connection open and close:

```json
{"msg":"websocket connected","backend":"http://ws-1:8000","client_ip":"203.0.113.5","x-request-id":"ws-abc"}
{"msg":"websocket closed","backend":"http://ws-1:8000","duration_s":3621.2,"bytes_in":1048576,"bytes_out":524288}
```
