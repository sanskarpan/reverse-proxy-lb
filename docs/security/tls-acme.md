# TLS and Automatic Certificates (ACME)

rplb terminates TLS for the data-plane listener and supports automatic certificate provisioning and renewal via the ACME protocol (RFC 8555) using `golang.org/x/crypto/acme/autocert`. The HTTP-01 challenge type is used exclusively.

---

## Manual TLS (static certificates)

For certificates managed by an external system (cert-manager, Vault PKI, manual renewal):

```yaml
tls:
  enabled: true
  cert_file: "/etc/rplb/tls/tls.crt"
  key_file: "/etc/rplb/tls/tls.key"
  min_version: "1.2"
  reload_on_change: true    # hot-reload on mtime change, no restart needed
```

### SNI multi-cert

Serve different certificates per domain using SNI:

```yaml
tls:
  enabled: true
  # Default certificate (catch-all)
  cert_file: "/etc/tls/default.crt"
  key_file: "/etc/tls/default.key"
  # Domain-specific certificates
  certificates:
    - cert_file: "/etc/tls/api.example.com.crt"
      key_file: "/etc/tls/api.example.com.key"
    - cert_file: "/etc/tls/app.example.com.crt"
      key_file: "/etc/tls/app.example.com.key"
```

The `GetCertificate` callback selects the correct leaf certificate based on the TLS ClientHello's ServerName extension.

### Mutual TLS (mTLS)

Require clients to present a certificate signed by a trusted CA:

```yaml
tls:
  enabled: true
  cert_file: "/etc/tls/server.crt"
  key_file: "/etc/tls/server.key"
  client_auth: "require_and_verify"
  client_ca_file: "/etc/tls/client-ca.crt"
```

`client_auth` values:

| Value | Effect |
|-------|--------|
| `none` | No client certificate requested |
| `request` | Client certificate requested but not required |
| `require_and_verify` | Client must present a valid certificate signed by `client_ca_file` |

### TLS version and cipher policy

```yaml
tls:
  min_version: "1.3"    # Enforce TLS 1.3 only
  cipher_suites:
    - "TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384"
    - "TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384"
    - "TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256"
```

Cipher suites are only effective for TLS 1.2. TLS 1.3 cipher negotiation is handled by the Go runtime and cannot be overridden per the `crypto/tls` specification.

---

## ACME (Let's Encrypt / automatic certificates)

### Prerequisites

**DNS** — Add an `A` record (and `AAAA` for IPv6) for each domain pointing at the server's public IP. The record must propagate before the first TLS handshake, or the HTTP-01 challenge will fail.

Example for `sanskarpan.xyz`:
```
sanskarpan.xyz.     60  IN  A   203.0.113.10
www.sanskarpan.xyz. 60  IN  A   203.0.113.10
```

**Port 80** — Must be open and reachable from the internet. Let's Encrypt validates domain ownership by fetching a token over plain HTTP on port 80. The proxy starts a dedicated `http_challenge_port` listener (default 80) on startup.

**Port 443** — Must be open for the HTTPS data-plane listener.

### Configuration

```yaml
tls:
  enabled: true
  acme:
    enabled: true
    domains:
      - "sanskarpan.xyz"
      - "www.sanskarpan.xyz"
    email: "admin@sanskarpan.xyz"
    cache_dir: "/var/cache/rplb/acme"
    directory_url: "https://acme-staging-v02.api.letsencrypt.org/directory"
    http_challenge_port: 80
```

### Staging vs production

Always use staging first — staging certificates are not browser-trusted but exercise the full ACME flow without consuming production rate-limit quota.

| Environment | `directory_url` |
|-------------|----------------|
| Staging | `https://acme-staging-v02.api.letsencrypt.org/directory` |
| Production | `https://acme-v02.api.letsencrypt.org/directory` (or omit the field) |

### Cache directory setup

```bash
sudo mkdir -p /var/cache/rplb/acme
sudo chown rplb: /var/cache/rplb/acme
sudo chmod 700 /var/cache/rplb/acme
```

Without a cache directory, certificates are re-issued on every restart and you will hit Let's Encrypt rate limits (50 certificates/domain/week in production).

The `ACME_CACHE_DIR` environment variable overrides `cache_dir` — useful for containers where the cache path is injected at runtime:

```bash
ACME_CACHE_DIR=/run/secrets/acme-cache ./bin/proxy --config configs/config.acme.yaml
```

### Step-by-step: first certificate

1. Configure DNS and open ports 80 and 443.
2. Create the cache directory (above).
3. Start with staging:
   ```bash
   ./bin/proxy --config configs/config.acme.yaml
   ```
4. Make a test HTTPS request:
   ```bash
   curl --insecure https://sanskarpan.xyz/   # --insecure because staging cert is not trusted
   ```
5. Verify the certificate was issued and cached:
   ```bash
   ls -la /var/cache/rplb/acme/
   ```
6. Switch to production — update `directory_url`, clear staging cache, restart:
   ```bash
   # In config.acme.yaml:
   directory_url: https://acme-v02.api.letsencrypt.org/directory

   rm -rf /var/cache/rplb/acme/*
   ./bin/proxy --config configs/config.acme.yaml
   ```

### Automatic renewal

Renewal is automatic. `autocert` checks the certificate expiry on every TLS handshake; when the certificate is within 30 days of expiry, it triggers a renewal in the background. No cron job or manual intervention is needed.

---

## OCSP Stapling

When TLS is enabled with a static certificate, rplb automatically fetches and caches OCSP (Online Certificate Status Protocol) responses, attaching them to TLS handshakes. This eliminates the client's need to contact the OCSP responder, reducing handshake latency and improving privacy.

The OCSP response is:
- Fetched from the responder URL embedded in the certificate.
- Refreshed before the response's `NextUpdate` time.
- Never stapled if the response status is `Revoked` or `Unknown`.
- Cached in memory (not on disk).

OCSP stapling is automatic when `tls.enabled: true` and a static cert/key is configured. No additional configuration is needed.

---

## HTTP/3 (QUIC)

When TLS is enabled, rplb also starts an HTTP/3 server on the same port using the QUIC transport. Browsers and HTTP/3-capable clients will use QUIC automatically after the first connection.

The proxy adds the `Alt-Svc` header to responses to advertise HTTP/3 support:

```
Alt-Svc: h3=":443"; ma=2592000
```

No additional configuration is required — HTTP/3 is enabled automatically when `tls.enabled: true`.

---

## mTLS to backends

Configure mTLS for the proxy-to-backend connection separately from the client-facing TLS:

```yaml
backend_tls:
  ca_file: "/etc/tls/backend-ca.crt"           # Verify backend certificates
  client_cert_file: "/etc/tls/proxy-client.crt" # Present client cert to backends
  client_key_file: "/etc/tls/proxy-client.key"
```

---

## Integration testing with Pebble

[Pebble](https://github.com/letsencrypt/pebble) is a lightweight ACME test server that exercises the complete HTTP-01 flow locally without hitting Let's Encrypt:

```bash
go install github.com/letsencrypt/pebble/cmd/pebble@latest
make integration-test
# or:
PEBBLE_PATH=$(which pebble) go test -tags=integration ./internal/server/...
```

The integration test skips automatically if Pebble is not installed.
