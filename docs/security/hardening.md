# Security Hardening

rplb includes a multi-layered hardening approach implemented across the middleware stack, HTTP server configuration, and transport settings. This page describes each protection and its configuration.

---

## Slowloris protection

The Slowloris attack opens many connections and sends HTTP headers one byte at a time, never completing the request. This exhausts file descriptors and goroutines without ever sending a full request.

**Mitigation:** `read_header_timeout` closes connections that do not complete their headers within the specified duration.

```yaml
server:
  read_header_timeout: 10s    # close if headers not complete within 10s
```

Any connection that does not finish sending its request headers within `read_header_timeout` receives a TCP RST. Legitimate clients sending headers in one write (virtually all HTTP clients) are unaffected.

**Recommended value:** 5–15 seconds. Lower values are more aggressive against slow attackers but may affect clients on very high-latency networks.

---

## Oversized headers (431 protection)

Attackers may send headers with hundreds of kilobytes of data to probe for parsing bugs or exhaust memory.

**Mitigation:** `max_header_bytes` causes `net/http` to reject requests with an oversized header section with `431 Request Header Fields Too Large` before any middleware runs.

```yaml
server:
  max_header_bytes: 1048576    # 1 MiB (Go's default)
```

For APIs where you know clients send only small headers (e.g., `Authorization` + `Content-Type`), reducing this to `65536` (64 KiB) is reasonable.

---

## Oversized bodies (413 protection)

Large request bodies can exhaust memory if buffered, or saturate disk if spooled. The `MaxBytes` middleware wraps the request body reader with a byte count:

```yaml
server:
  max_request_body_bytes: 10485760    # 10 MiB default
```

When a body exceeds the limit, the connection is closed with `413 Payload Too Large` and the remaining body bytes are discarded. The proxy never buffers the full body — the limit is enforced streaming.

Set to `0` to disable (not recommended for public-facing deployments without another body-size enforcement layer).

For per-path overrides (e.g., allowing larger bodies on `/api/upload`), use a route-specific configuration or an upstream reverse proxy that handles multipart uploads directly.

---

## Connection and request timeouts

```yaml
server:
  read_timeout: 30s           # time to read the complete request (headers + body)
  write_timeout: 30s          # time to write the complete response
  idle_timeout: 120s          # keep-alive idle connection lifetime
  read_header_timeout: 10s    # time to read just the headers
  shutdown_timeout: 30s       # graceful shutdown drain window
```

Together these timeouts bound the lifetime of every connection:

- A connection that reads slowly is killed by `read_timeout`.
- A connection that reads headers slowly is killed earlier by `read_header_timeout`.
- A response that writes slowly is killed by `write_timeout`.
- An idle keep-alive connection is cleaned up by `idle_timeout`.

All four should be set for public-facing deployments. Omitting any one of them allows the corresponding attack vector.

---

## Security response headers middleware

The `SecurityHeaders` middleware adds HTTP security headers to every response. These instruct browsers to apply additional protections.

```yaml
security:
  headers:
    enabled: true
    hsts: "max-age=31536000; includeSubDomains; preload"
    frame_options: "DENY"
    content_type_options: "nosniff"
    csp: "default-src 'self'; script-src 'self'"
    referrer_policy: "strict-origin-when-cross-origin"
    permissions_policy: "geolocation=(), microphone=(), camera=()"
```

| Header | Effect |
|--------|--------|
| `Strict-Transport-Security` | Forces HTTPS for the `max-age` duration; `preload` enables browser preload list inclusion |
| `X-Frame-Options: DENY` | Prevents the page from being embedded in an iframe (clickjacking protection) |
| `X-Content-Type-Options: nosniff` | Prevents browsers from MIME-sniffing a response away from the declared content type |
| `Content-Security-Policy` | Controls which resources the browser is allowed to load |
| `Referrer-Policy` | Controls how much referrer information is sent |
| `Permissions-Policy` | Restricts access to browser APIs (geolocation, camera, etc.) |

These headers are added to every proxied response. Headers already set by the upstream backend are not overwritten — the middleware only adds headers that are absent.

---

## CORS

CORS (Cross-Origin Resource Sharing) controls which origins can make cross-site requests:

```yaml
security:
  cors:
    enabled: true
    allow_origins:
      - "https://app.example.com"
      - "https://admin.example.com"
    allow_methods: ["GET", "POST", "PUT", "DELETE", "OPTIONS"]
    allow_headers: ["Authorization", "Content-Type", "X-Request-ID"]
    allow_credentials: true
    max_age: 86400    # preflight cache duration in seconds
```

**Do not use `allow_origins: ["*"]` with `allow_credentials: true`** — this is a CORS security misconfiguration that browsers reject and that would expose session cookies to any origin.

---

## ACL (IP/method/path filtering)

The ACL middleware runs before authentication and provides coarse-grained access control:

```yaml
security:
  acl:
    # IP allowlist — only these CIDRs can access the proxy
    allow:
      - "10.0.0.0/8"
      - "172.16.0.0/12"

    # IP denylist — these are blocked even if in the allowlist
    deny:
      - "10.0.100.0/24"

    # Restrict to specific HTTP methods
    methods: ["GET", "POST", "HEAD", "OPTIONS"]

    # Block specific paths entirely
    blocked_paths:
      - "/.git/"
      - "/wp-admin/"
      - "/phpMyAdmin/"
      - "/.env"
```

ACL evaluation order:
1. If `deny` list is non-empty and client IP matches, reject 403.
2. If `allow` list is non-empty and client IP does not match, reject 403.
3. If `methods` list is non-empty and request method is not in it, reject 405.
4. If `blocked_paths` list is non-empty and path prefix matches, reject 403.

---

## Admin plane security

The admin plane (metrics, health, reload, drain API) is hardened by default:

- **Loopback binding:** `metrics.host: 127.0.0.1` — admin plane does not listen on public interfaces.
- **Bearer token auth:** Set `metrics.auth_token` to require a secret token on all admin endpoints.
- **No sensitive data in metrics:** The Prometheus exposition format does not include request bodies, headers, or personally identifiable information.

```yaml
metrics:
  enabled: true
  host: "127.0.0.1"    # loopback only
  port: 9090
  auth_token: "${ADMIN_TOKEN}"
```

Admin endpoints protected by the bearer token:

```bash
curl -H "Authorization: Bearer $ADMIN_TOKEN" http://localhost:9090/metrics
curl -H "Authorization: Bearer $ADMIN_TOKEN" -XPOST http://localhost:9090/reload
```

---

## Upstream TLS verification

Never disable TLS verification for production backends:

```yaml
backend_tls:
  insecure_skip_verify: false    # NEVER set true in production
  ca_file: "/etc/tls/backend-ca.crt"
```

`insecure_skip_verify: true` disables certificate chain and hostname validation — a man-in-the-middle attacker can present any certificate and the proxy will accept it. This is acceptable only in local development with self-signed certificates when there is no network attacker.

---

## Summary: minimum hardening checklist

| Protection | Config | Recommended value |
|-----------|--------|-------------------|
| Slowloris | `server.read_header_timeout` | `10s` |
| Large headers | `server.max_header_bytes` | `65536`–`1048576` |
| Large bodies | `server.max_request_body_bytes` | `10485760` (10 MiB) |
| Idle connections | `server.idle_timeout` | `120s` |
| Security headers | `security.headers.enabled` | `true` |
| Admin plane binding | `metrics.host` | `127.0.0.1` |
| Admin auth | `metrics.auth_token` | set a strong secret |
| Backend TLS | `backend_tls.insecure_skip_verify` | `false` |
