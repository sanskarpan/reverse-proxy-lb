# Rate Limiting

rplb provides a flexible rate-limiting layer supporting two algorithms (token bucket and GCRA), global and per-key limits, per-path rules, an IP allowlist, and a pluggable store backend with Redis support for distributed rate limiting across multiple proxy instances.

---

## Algorithms

### Token Bucket

The classic leaky-bucket variant: a bucket of capacity `burst` refills at rate `requests_per_second`. A request is allowed when a token is available; otherwise it is rejected with 429.

Token bucket allows short bursts up to `burst` size above the steady-state rate. This is appropriate for APIs where clients legitimately accumulate quota while idle and then consume it in a burst.

### GCRA (Generic Cell Rate Algorithm)

GCRA (also known as virtual scheduling) is a stricter algorithm that enforces a minimum inter-arrival time between requests at the configured rate. Unlike token bucket, GCRA does not allow accumulation — a client cannot save up quota and spend it all at once.

```
theoretical_arrival_time = max(now, last_arrival + 1/rate)
if theoretical_arrival_time - now > burst_tolerance:
    reject with 429
else:
    allow; update last_arrival = theoretical_arrival_time
```

GCRA is preferred for API-key rate limiting where clients must not burst above the agreed rate, and for Redis-backed distributed limiting where atomic Lua scripts make GCRA trivially correct.

---

## Configuration

```yaml
rate_limiter:
  enabled: true
  algorithm: gcra          # token_bucket | gcra
  requests_per_second: 100
  burst: 200

  global_rps: 5000
  global_burst: 10000

  key: ip                  # ip | header:<Name>
  retry_after_seconds: 1
  message: "Rate limit exceeded. Please slow down."

  allowlist:
    - "10.0.0.0/8"
    - "127.0.0.1/32"
    - "::1/128"

  rules:
    - path_prefix: "/api/v1/upload"
      method: POST
      rps: 5
      burst: 10

    - path_prefix: "/api/v1/search"
      rps: 20
      burst: 40
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `algorithm` | string | `token_bucket` | Rate-limiting algorithm |
| `requests_per_second` | float | — | Per-key rate limit |
| `burst` | int | — | Per-key burst allowance |
| `global_rps` | float | — | Rate limit across all keys combined |
| `global_burst` | int | — | Global burst allowance |
| `key` | string | `ip` | Key source: `ip` extracts client IP; `header:X-API-Key` uses a header value |
| `retry_after_seconds` | int | `1` | Value for the `Retry-After` response header on 429 |
| `message` | string | — | Response body on 429 |
| `allowlist` | []CIDR | `[]` | IPs exempt from all rate limiting |
| `rules` | []object | — | Per-path/method overrides, evaluated before the global limit |

### Per-path rules

Rules are evaluated in order. The first matching rule's limits apply. If no rule matches, the global `requests_per_second`/`burst` applies.

---

## Key extraction

### By IP (`key: ip`)

The client IP is extracted after trusted-proxy header processing. If `server.trusted_proxies` is configured, the rightmost non-trusted IP in `X-Forwarded-For` is used as the key. This prevents clients from spoofing their IP by adding fake `X-Forwarded-For` entries.

### By header (`key: header:X-API-Key`)

The raw header value is used as the rate-limit key. Useful for API key rate limiting:

```yaml
rate_limiter:
  key: "header:X-API-Key"
  requests_per_second: 1000
  burst: 2000
```

Requests without the header are grouped under a sentinel key and subject to the same limit. To reject keyless requests entirely, combine with an ACL or auth middleware.

---

## Distributed rate limiting (Redis)

For multi-instance deployments where a single proxy cannot enforce a global budget alone, the Redis store aggregates counts across instances:

```yaml
rate_limiter:
  enabled: true
  algorithm: gcra
  requests_per_second: 100
  burst: 200

  redis:
    addr: "redis:6379"
    password: "${REDIS_PASSWORD}"
    db: 0
    key_prefix: "rplb:rl:"
```

The Redis-backed GCRA uses a Lua script for atomic compare-and-set:

```lua
local key    = KEYS[1]
local now    = tonumber(ARGV[1])
local rate   = tonumber(ARGV[2])
local burst  = tonumber(ARGV[3])
local tat    = tonumber(redis.call("GET", key) or now)

local new_tat = math.max(tat, now) + (1 / rate)
local diff    = new_tat - now - burst

if diff > 0 then
  return {0, diff}  -- rejected, retry_after = diff seconds
end

redis.call("SET", key, new_tat, "EX", math.ceil(burst / rate + 1))
return {1, 0}  -- allowed
```

The Lua script executes atomically in Redis, eliminating the TOCTOU race that would exist with separate GET/SET operations.

When Redis is unavailable, the rate limiter falls back to the in-memory store automatically. The fallback is logged at WARN level.

### In-memory store

The default in-memory store uses a thread-safe map with LRU eviction. The map is bounded to prevent unbounded memory growth under key-diversity attacks. Eviction removes the least-recently-seen key when the map is full.

---

## Response headers

On a rejected request (429), the proxy sets:

```
HTTP/1.1 429 Too Many Requests
Retry-After: 1
X-RateLimit-Limit: 100
X-RateLimit-Remaining: 0
X-RateLimit-Reset: 1753093261
Content-Type: application/json

{"error": "Rate limit exceeded. Please slow down."}
```

On allowed requests, `X-RateLimit-Remaining` and `X-RateLimit-Reset` are included in the response headers so well-behaved clients can self-throttle before hitting the limit.

---

## Metrics

```
rplb_rate_limited_total  Total number of requests rejected by the rate limiter
```

Suggested alert:

```promql
rate(rplb_rate_limited_total[5m]) > 10
```

A sustained 429 rate suggests the limit is too low for the current traffic pattern, or a misbehaving client is hammering the API.
