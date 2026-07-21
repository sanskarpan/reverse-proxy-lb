# Authentication

rplb supports four authentication modes at the middleware layer: none, HTTP Basic, API key, and JWT (with optional OIDC token introspection). Authentication runs after ACL and before any proxied request reaches an upstream backend.

---

## Authentication modes

Configure the mode via `security.auth.type`:

| Mode | Value | Use case |
|------|-------|---------|
| None | `none` (default) | No authentication — the proxy passes all requests through |
| Basic | `basic` | Simple username/password for internal tools |
| API Key | `apikey` | Stateless token-based access for service-to-service calls |
| JWT | `jwt` | Bearer token validation (RS256 or HS256); optional OIDC introspection |

---

## Basic authentication

```yaml
security:
  auth:
    type: basic
    users:
      alice: "$2a$12$..."    # bcrypt hash of the password
      bob: "$2a$12$..."
```

Passwords are stored as bcrypt hashes. Generate a hash:

```bash
htpasswd -nBC 12 alice
```

Requests without a valid `Authorization: Basic ...` header are rejected with 401 and a `WWW-Authenticate: Basic realm="rplb"` challenge.

---

## API key authentication

```yaml
security:
  auth:
    type: apikey
    api_keys:
      - "sk-prod-abc123def456"
      - "sk-staging-xyz789"
    header: "X-API-Key"    # default header name
```

The middleware checks the named header for an exact match against the configured key list. A missing or invalid key returns 401.

To use the `Authorization` header with a `Bearer` prefix instead:

```yaml
security:
  auth:
    type: apikey
    api_keys:
      - "my-secret-token"
    header: "Authorization"
```

---

## JWT authentication

rplb validates JWT bearer tokens in the `Authorization: Bearer <token>` header.

### HS256 (symmetric)

```yaml
security:
  auth:
    type: jwt
    jwt_alg: HS256
    jwt_secret: "${JWT_SECRET}"    # HMAC-SHA256 shared secret
```

### RS256 (asymmetric)

```yaml
security:
  auth:
    type: jwt
    jwt_alg: RS256
    jwt_public_key_file: "/etc/rplb/keys/jwt-public.pem"
```

Or fetch public keys from a JWKS endpoint (recommended for OIDC):

```yaml
security:
  auth:
    type: jwt
    jwt_alg: RS256
    jwks_url: "https://auth.example.com/.well-known/jwks.json"
    jwks_cache_ttl: 300s    # how long to cache keys before re-fetching
```

### Validation

The JWT middleware validates:
- Signature (using the configured key or JWKS-fetched key).
- `exp` (expiry) claim — expired tokens are rejected.
- `nbf` (not before) claim — tokens not yet valid are rejected.
- `iss` (issuer) claim — optional, only if `jwt_issuer` is set.
- `aud` (audience) claim — optional, only if `jwt_audience` is set.

```yaml
security:
  auth:
    type: jwt
    jwt_alg: RS256
    jwks_url: "https://auth.example.com/.well-known/jwks.json"
    jwt_issuer: "https://auth.example.com/"
    jwt_audience: "https://api.example.com"
```

Validated claims are forwarded to the upstream backend as `X-JWT-Subject`, `X-JWT-Email`, and `X-JWT-Claims` (JSON-encoded) headers, so backends can use them without re-validating the token.

---

## OIDC token introspection (RFC 7662)

For opaque tokens (tokens that are not self-contained JWTs), rplb can validate them against an OIDC provider's introspection endpoint:

```yaml
security:
  auth:
    type: jwt
    oidc:
      enabled: true
      introspection_url: "https://auth.example.com/oauth/introspect"
      client_id: "rplb-proxy"
      client_secret: "${OIDC_CLIENT_SECRET}"
      cache_ttl: 60s          # how long to cache introspection results
      cache_size: 10000       # max cached tokens (LRU eviction)
```

### Introspection flow

1. Client sends `Authorization: Bearer <opaque-token>`.
2. Proxy checks LRU cache for the token hash.
3. Cache miss: proxy calls `POST /oauth/introspect` with `token=<opaque-token>` and Basic auth (`client_id:client_secret`).
4. If the response `active` field is `true`, the token is valid and the result is cached.
5. Cache hit: the cached result is used directly without an upstream call.

The LRU cache significantly reduces load on the introspection endpoint. Cache TTL should be set shorter than the token's expected lifetime to avoid serving stale `active: false` results after token revocation.

### Introspection response example

```json
{
  "active": true,
  "sub": "user-12345",
  "email": "user@example.com",
  "scope": "openid profile email",
  "exp": 1753096800,
  "iat": 1753093200
}
```

---

## Combining authentication with ACL

ACL and authentication run in sequence: ACL first (IP/method/path filtering), then authentication. This order ensures unauthenticated requests from blocked IPs are rejected at the ACL layer without touching the auth middleware.

```yaml
security:
  acl:
    allow:
      - "10.0.0.0/8"

  auth:
    type: jwt
    jwks_url: "https://auth.example.com/.well-known/jwks.json"
```

---

## Security headers

Authentication works alongside the security headers middleware:

```yaml
security:
  headers:
    enabled: true
    hsts: "max-age=31536000; includeSubDomains; preload"
    frame_options: "DENY"
    content_type_options: "nosniff"
    csp: "default-src 'self'; script-src 'self' 'nonce-{RANDOM}'"
    referrer_policy: "strict-origin-when-cross-origin"
    permissions_policy: "geolocation=(), microphone=(), camera=()"
```

---

## Error responses

| Condition | Status | Response |
|-----------|--------|---------|
| Missing token/credentials | `401 Unauthorized` | `WWW-Authenticate` header set |
| Invalid token signature | `401 Unauthorized` | — |
| Expired token | `401 Unauthorized` | — |
| Valid token, insufficient scope | `403 Forbidden` | — |
| Introspection endpoint unreachable | `503 Service Unavailable` | Fail-closed behavior |

The proxy fails **closed** on auth errors: if the introspection endpoint is unreachable, requests are rejected rather than allowed through. This is the safe default for a security boundary.
