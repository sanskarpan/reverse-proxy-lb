# Configuration Reference

Configuration is loaded from a YAML file passed via `--config` (default `configs/config.yaml`). Run `--validate` to check a config without starting.

Every advanced block is **opt-in** — omitting it is equivalent to disabling it with safe defaults.

---

## Environment overrides

Environment variables take precedence over file values. Useful for injecting secrets in containers without editing config files.

| Variable | Overrides |
|----------|-----------|
| `RPLB_SERVER_HOST` | `server.host` |
| `RPLB_SERVER_PORT` | `server.port` |
| `RPLB_LOG_LEVEL` | `logging.level` |
| `RPLB_METRICS_ENABLED` | `metrics.enabled` |
| `RPLB_METRICS_PORT` | `metrics.port` |
| `RPLB_RATE_LIMIT_ENABLED` | `rate_limiter.enabled` |
| `RPLB_RATE_LIMIT_RPS` | `rate_limiter.requests_per_second` |
| `RPLB_RATE_LIMIT_BURST` | `rate_limiter.burst` |
| `RPLB_BACKENDS` | Comma-separated list of backend URLs — replaces the entire `backends` list |
| `ACME_CACHE_DIR` | `tls.acme.cache_dir` |

---

## Secret placeholders

Two placeholder forms are resolved at config load time before validation:

- `${VAR}` — environment variable expansion; undefined vars are left unexpanded.
- `${vault:PATH#KEY}` — HashiCorp Vault KV v2 lookup at mount path `PATH`, key `KEY`.

Vault authentication supports token (env `VAULT_TOKEN`) and AppRole (`VAULT_ROLE_ID` + `VAULT_SECRET_ID`).

Example:

```yaml
auth:
  type: jwt
  jwt_secret: "${vault:secret/rplb#jwt_signing_key}"
```

---

## `server`

```yaml
server:
  host: "0.0.0.0"
  port: 8080
  read_timeout: 30s
  write_timeout: 30s
  idle_timeout: 120s
  shutdown_timeout: 30s
  trusted_proxies:
    - "10.0.0.0/8"
    - "172.16.0.0/12"
  zone: "us-east-1a"
  watch_config: false
  watch_interval: 5s

  upstream:
    dial_timeout: 5s
    tls_handshake_timeout: 5s
    response_header_timeout: 30s
    expect_continue_timeout: 1s
    idle_conn_timeout: 90s
    max_idle_conns: 100
    max_idle_conns_per_host: 100
    max_conns_per_host: 0
    http2: false

  websocket:
    idle_timeout: 0
    max_message_bytes: 0

  l4:
    enabled: false
    port: 8443
    dial_timeout: 5s
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `host` | string | `0.0.0.0` | Data-plane bind address |
| `port` | int | `8080` | Data-plane port |
| `read_timeout` | duration | — | Maximum time to read an entire request |
| `write_timeout` | duration | — | Maximum time to write a response |
| `idle_timeout` | duration | — | Keep-alive idle timeout |
| `shutdown_timeout` | duration | `30s` | Time to wait for in-flight requests during graceful shutdown |
| `trusted_proxies` | []CIDR | `[]` | CIDRs whose `X-Forwarded-For` headers are trusted; empty means headers are ignored |
| `zone` | string | — | This instance's availability zone for zone-aware routing |
| `watch_config` | bool | `false` | Poll the config file and reload automatically on change |
| `watch_interval` | duration | `5s` | How often to poll for config changes |
| `upstream.dial_timeout` | duration | `5s` | TCP connect timeout to upstream |
| `upstream.tls_handshake_timeout` | duration | `5s` | TLS handshake timeout to upstream |
| `upstream.response_header_timeout` | duration | `30s` | Time to wait for upstream response headers |
| `upstream.http2` | bool | `false` | Enable h2c / ALPN HTTP/2 to upstream backends |
| `upstream.max_idle_conns_per_host` | int | `100` | Connection pool size per backend |
| `websocket.idle_timeout` | duration | `0` | WebSocket idle timeout (0 = unlimited) |
| `websocket.max_message_bytes` | int64 | `0` | Maximum WebSocket frame size in bytes (0 = unlimited) |
| `l4.enabled` | bool | `false` | Enable raw TCP (L4) proxy |
| `l4.port` | int | — | L4 listener port |

---

## `tls`

TLS termination for the data-plane listener.

```yaml
tls:
  enabled: true
  cert_file: "/etc/rplb/tls/tls.crt"
  key_file: "/etc/rplb/tls/tls.key"
  min_version: "1.2"
  cipher_suites:
    - "TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384"
    - "TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384"
  # SNI multi-cert
  certificates:
    - cert_file: "/etc/rplb/tls/api.crt"
      key_file: "/etc/rplb/tls/api.key"
  # mTLS
  client_auth: "require_and_verify"
  client_ca_file: "/etc/rplb/tls/client-ca.crt"
  reload_on_change: true

  # ACME / Let's Encrypt
  acme:
    enabled: true
    domains:
      - "sanskarpan.xyz"
      - "www.sanskarpan.xyz"
    cache_dir: "/var/cache/rplb/acme"
    directory_url: "https://acme-staging-v02.api.letsencrypt.org/directory"
    email: "admin@sanskarpan.xyz"
    http_challenge_port: 80
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable TLS termination |
| `cert_file` | string | — | PEM certificate path |
| `key_file` | string | — | PEM private key path |
| `min_version` | string | `1.2` | Minimum TLS version (`1.2` or `1.3`) |
| `cipher_suites` | []string | Go defaults | Explicit cipher suite list (TLS 1.2 only) |
| `certificates` | []object | — | Additional certs for SNI-based multi-cert |
| `client_auth` | string | `none` | mTLS mode: `none`, `request`, `require_and_verify` |
| `client_ca_file` | string | — | CA cert for verifying client certificates |
| `reload_on_change` | bool | `false` | Hot-reload cert/key when mtime changes (no fsnotify, no restart) |
| `acme.enabled` | bool | `false` | Enable ACME auto-cert via Let's Encrypt |
| `acme.domains` | []string | — | Domains to obtain certificates for |
| `acme.cache_dir` | string | — | Directory to cache certs and account key |
| `acme.directory_url` | string | production | ACME directory URL (omit for production) |
| `acme.http_challenge_port` | int | `80` | Port for HTTP-01 challenge listener |

See [TLS/ACME guide](security/tls-acme.md) for full setup instructions.

---

## `backend_tls`

TLS configuration for the proxy-to-backend connection.

```yaml
backend_tls:
  insecure_skip_verify: false
  ca_file: "/etc/rplb/tls/backend-ca.crt"
  client_cert_file: "/etc/rplb/tls/proxy.crt"
  client_key_file: "/etc/rplb/tls/proxy.key"
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `insecure_skip_verify` | bool | `false` | Skip backend TLS certificate verification (development only) |
| `ca_file` | string | — | CA cert to verify backend certificates |
| `client_cert_file` | string | — | Client certificate for mTLS to backends |
| `client_key_file` | string | — | Client key for mTLS to backends |

---

## `backends`

The static backend pool. Each backend is a weighted, zone-aware, priority-tiered upstream.

```yaml
backends:
  - url: "http://backend-1:8000"
    weight: 2
    max_conns: 200
    zone: "us-east-1a"
    tier: 0
    health_check:
      path: "/health"
      interval: 5s

  - url: "http://backend-2:8000"
    weight: 1
    max_conns: 100
    zone: "us-east-1b"
    tier: 0

  - url: "http://fallback:8000"
    weight: 1
    tier: 1          # only used when tier-0 backends are unavailable
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `url` | string | required | Backend URL (scheme + host + optional path prefix) |
| `weight` | int | `1` | Relative weight for weighted algorithms |
| `max_conns` | int | `100` | Maximum concurrent in-flight requests (bulkhead) |
| `zone` | string | — | Availability zone for zone-aware routing |
| `tier` | int | `0` | Priority tier (lower is higher priority) |
| `health_check` | object | — | Per-backend health check override |

---

## `load_balancer`

```yaml
load_balancer:
  algorithm: "round_robin"

  consistent_hash:
    replicas: 100
    load_factor: 1.25

  sticky:
    enabled: true
    cookie: "rplb_affinity"
    ttl: 3600s

  slow_start: 30s
  prefer_same_zone: true

  outlier_detection:
    enabled: true
    error_rate_threshold: 0.5
    min_requests: 20
    base_ejection: 30s
    max_ejection_percent: 50

  health_check:
    enabled: true
    type: http
    interval: 10s
    timeout: 5s
    path: "/health"
    method: GET
    expected_statuses: [200, 204]
    expected_body: ""
    healthy_threshold: 2
    unhealthy_threshold: 3
    jitter: 0.1
    startup_grace_period: 10s
```

### Algorithms

| Value | Description |
|-------|-------------|
| `round_robin` | Uniform distribution in order |
| `least_conn` | Fewest active connections — actually P2C under the hood |
| `weighted` | Weighted round-robin |
| `swrr` | Smooth weighted round-robin (Nginx-style, no burst) |
| `weighted_least_conn` | Weight × connection count |
| `weighted_random` | Random selection weighted by `weight` |
| `p2c` | Power of Two Choices — pick 2, select the less loaded |
| `consistent_hash` | Bounded-load consistent hashing for session affinity |
| `ewma` | Peak-EWMA latency-aware selection |
| `ip_hash` | Hash client IP for sticky routing |

See [Algorithms](algorithms/index.md) for a full comparison and deep dives.

### Health check fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `type` | string | `http` | Probe type: `http` or `tcp` |
| `interval` | duration | `10s` | Time between probes |
| `timeout` | duration | `5s` | Probe timeout |
| `path` | string | `/health` | HTTP probe path |
| `healthy_threshold` | int | `2` | Consecutive successes to mark healthy |
| `unhealthy_threshold` | int | `3` | Consecutive failures to mark unhealthy |
| `jitter` | float | `0.1` | Random jitter fraction added to interval (prevents thundering herd) |
| `startup_grace_period` | duration | — | Skip health checks for this long after startup |

---

## `routes`

L7 routing rules evaluated before the default backend group. First match wins; unmatched requests use the default group.

```yaml
routes:
  - name: "api-v2"
    host: "api.example.com"
    path_prefix: "/v2/"
    methods: ["GET", "POST"]
    headers:
      X-Client-Version: "2"
    algorithm: least_conn
    backends:
      - url: "http://api-v2-1:8000"
        weight: 1
      - url: "http://api-v2-2:8000"
        weight: 1

  - name: "static"
    path_prefix: "/static/"
    algorithm: round_robin
    backends:
      - url: "http://cdn-proxy:8000"
```

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Route identifier (for logs and metrics) |
| `host` | string | Exact `Host` header match |
| `path_prefix` | string | URL path prefix match |
| `methods` | []string | HTTP method allowlist |
| `headers` | map[string]string | Exact request header value matches |
| `algorithm` | string | Override LB algorithm for this route |
| `backends` | []backend | Route-specific backend pool |

---

## `circuit_breaker`

```yaml
circuit_breaker:
  enabled: true
  mode: "consecutive"       # or "rolling"
  failure_threshold: 5
  success_threshold: 2
  timeout: 30s

  # Rolling window mode only
  rolling_window: 10s
  error_rate_threshold: 0.5
  min_requests: 20

  trip_on:
    - connect
    - timeout
    - 5xx
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `mode` | string | `consecutive` | `consecutive` (N failures in a row) or `rolling` (error rate in window) |
| `failure_threshold` | int | `5` | Consecutive failures to trip (consecutive mode) |
| `success_threshold` | int | `2` | Consecutive successes to close (half-open → closed) |
| `timeout` | duration | `30s` | How long to stay open before probing |
| `rolling_window` | duration | `10s` | Window duration for rolling mode |
| `error_rate_threshold` | float | `0.5` | Error rate (0–1) to trip in rolling mode |
| `min_requests` | int | `20` | Minimum requests in window before tripping |
| `trip_on` | []string | `[connect, timeout]` | Error classes that count as failures |

See [Circuit Breaker](resilience/circuit-breaker.md) for state diagram and distributed sync.

---

## `retry`

```yaml
retry:
  max_attempts: 3
  backoff: exponential
  max_backoff: 10s
  budget: 0
  per_try_timeout: 5s
  honor_retry_after: true
  retry_on:
    - connect
    - timeout

  hedge:
    enabled: true
    delay: 50ms
    max_extra: 2
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `max_attempts` | int | `3` | Maximum total attempts (including the first) |
| `backoff` | string | `exponential` | Backoff strategy: `exponential` or `linear` |
| `max_backoff` | duration | `10s` | Backoff cap |
| `budget` | int | `0` | Maximum concurrent retries across all requests (0 = unlimited) |
| `per_try_timeout` | duration | — | Per-attempt timeout independent of overall request timeout |
| `honor_retry_after` | bool | `true` | Respect `Retry-After` header from upstream |
| `retry_on` | []string | `[connect, timeout]` | Error conditions that trigger retry |
| `hedge.enabled` | bool | `false` | Enable request hedging |
| `hedge.delay` | duration | — | Delay before issuing a hedged request |
| `hedge.max_extra` | int | `1` | Maximum extra hedged requests |

See [Retry and Hedging](resilience/retry-hedging.md) for details.

---

## `rate_limiter`

```yaml
rate_limiter:
  enabled: true
  algorithm: token_bucket     # or gcra
  requests_per_second: 100
  burst: 200

  global_rps: 1000
  global_burst: 2000

  key: ip                     # or "header:X-API-Key"
  retry_after_seconds: 1
  message: "Rate limit exceeded"

  allowlist:
    - "10.0.0.0/8"
    - "127.0.0.1/32"

  rules:
    - path_prefix: "/api/v1/upload"
      method: POST
      rps: 10
      burst: 20
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `algorithm` | string | `token_bucket` | `token_bucket` or `gcra` (Generic Cell Rate Algorithm) |
| `requests_per_second` | float | — | Per-key rate |
| `burst` | int | — | Per-key burst size |
| `global_rps` / `global_burst` | float/int | — | Global (across all keys) rate and burst |
| `key` | string | `ip` | Key source: `ip` or `header:<Name>` |
| `allowlist` | []CIDR | — | IPs exempt from rate limiting |
| `rules` | []object | — | Per-path/method rate limit overrides |

See [Rate Limiting](resilience/rate-limiting.md) for GCRA mechanics and Redis distributed mode.

---

## `security`

### Headers

```yaml
security:
  headers:
    enabled: true
    hsts: "max-age=31536000; includeSubDomains"
    frame_options: "DENY"
    content_type_options: "nosniff"
    csp: "default-src 'self'"
    referrer_policy: "strict-origin-when-cross-origin"
```

### CORS

```yaml
security:
  cors:
    enabled: true
    allow_origins:
      - "https://app.example.com"
    allow_methods: ["GET", "POST", "PUT", "DELETE"]
    allow_headers: ["Authorization", "Content-Type"]
    allow_credentials: true
    max_age: 86400
```

### ACL

```yaml
security:
  acl:
    allow:
      - "10.0.0.0/8"
    deny:
      - "192.168.100.0/24"
    methods: ["GET", "POST"]
    blocked_paths:
      - "/internal/"
      - "/.git/"
```

### Authentication

```yaml
security:
  auth:
    type: jwt          # none | basic | apikey | jwt
    jwt_secret: "${JWT_SECRET}"
    jwt_alg: RS256     # RS256 or HS256
    # For basic auth
    users:
      alice: "${ALICE_PASSWORD_HASH}"
    # For API key
    api_keys:
      - "sk-prod-abc123"
    header: "X-API-Key"
```

See [Auth](security/auth.md) for JWT/OIDC configuration.

---

## `canary`

```yaml
canary:
  enabled: true
  weight_percent: 10
  algorithm: round_robin
  backends:
    - url: "http://canary-1:8000"
      weight: 1

  auto_promote:
    enabled: true
    step_percent: 10
    step_interval: 5m
    max_error_rate: 0.01
    min_requests: 100
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `weight_percent` | int | — | Percentage of traffic to route to canary backends |
| `algorithm` | string | `round_robin` | Algorithm for canary backend selection |
| `auto_promote.step_percent` | int | — | Traffic percentage to add at each step |
| `auto_promote.step_interval` | duration | — | Time between promotion steps |
| `auto_promote.max_error_rate` | float | — | Canary error rate threshold that triggers rollback |
| `auto_promote.min_requests` | int | — | Minimum requests before evaluating error rate |

See [Canary Deployments](ops/canary.md) for the auto-promote workflow.

---

## `mirror`

Shadow-copy a percentage of requests to a mirror endpoint without affecting clients.

```yaml
mirror:
  enabled: true
  url: "http://mirror-backend:8000"
  sample_percent: 10
  timeout: 1s
```

---

## `rewrite`

```yaml
rewrite:
  request_headers_set:
    X-Forwarded-Proto: "https"
    X-Internal-Source: "rplb"
  request_headers_remove:
    - "X-Debug-Token"
  response_headers_set:
    X-Proxy: "rplb"
  response_headers_remove:
    - "Server"
    - "X-Powered-By"
  strip_path_prefix: "/api"
  https_redirect: true
```

---

## `fault_injection`

Inject artificial failures for chaos testing.

```yaml
fault_injection:
  enabled: true
  delay_percent: 5
  delay: 200ms
  abort_percent: 1
  abort_status: 503
```

---

## `compression`

```yaml
compression:
  enabled: true
  min_size: 1024
  content_types:
    - "application/json"
    - "text/html"
    - "text/plain"
```

---

## `logging`

```yaml
logging:
  level: info       # debug | info | warn | error
  format: json      # json | text
```

Log level is hot-reloadable without restart.

---

## `metrics`

```yaml
metrics:
  enabled: true
  host: "127.0.0.1"
  port: 9090
  auth_token: "${METRICS_TOKEN}"
```

The admin plane at `host:port` exposes:

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/metrics` | GET | Prometheus text format |
| `/metrics.json` | GET | JSON format metrics |
| `/healthz` | GET | Liveness — always 200 when process is running |
| `/readyz` | GET | Readiness — 200 when at least one backend is healthy |
| `/reload` | POST | Trigger live config reload |
| `/admin/backends` | GET | List backends with health, connections, circuit state |
| `/admin/drain` | POST | Stop new traffic to a backend (`?url=`) |
| `/admin/undrain` | POST | Resume traffic to a backend (`?url=`) |
| `/admin/weight` | POST | Adjust backend weight at runtime (`?url=&weight=`) |
| `/admin/circuit/reset` | POST | Force-close circuit breaker (`?url=`) |
| `/debug/pprof/` | GET | Go pprof profiles |

---

## `discovery`

Dynamic backend discovery supplements or replaces the static `backends` list.

```yaml
discovery:
  dns:
    enabled: true
    host: "backend-service.default.svc.cluster.local"
    port: 8000
    type: A          # A or SRV
    interval: 30s

  kubernetes:
    enabled: true
    namespace: "default"
    service: "backend"
    port_name: "http"
```

---

## `tracing`

```yaml
tracing:
  enabled: true
  endpoint: "http://jaeger:4317"
  service_name: "rplb"
  sample_rate: 1.0
```

See [Tracing](ops/tracing.md) for Jaeger/OTLP setup.
