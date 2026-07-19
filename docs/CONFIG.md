# Configuration reference

YAML config loaded via `--config` (default `configs/config.yaml`). Environment
variables override file values; `--validate` checks a config without starting.
Every advanced block is **opt-in** and defaults to prior/safe behavior.

## Environment overrides

`RPLB_SERVER_HOST`, `RPLB_SERVER_PORT`, `RPLB_LOG_LEVEL`, `RPLB_METRICS_ENABLED`,
`RPLB_METRICS_PORT`, `RPLB_RATE_LIMIT_ENABLED`, `RPLB_RATE_LIMIT_RPS`,
`RPLB_RATE_LIMIT_BURST`, `RPLB_BACKENDS` (comma-separated URLs, replaces `backends`).

## `server`

| Field | Default | Notes |
|-------|---------|-------|
| `host` / `port` | `0.0.0.0` / `8080` | data-plane listener |
| `read_timeout` / `write_timeout` / `idle_timeout` | — | `http.Server` timeouts |
| `shutdown_timeout` | `30s` | graceful-shutdown drain bound |
| `trusted_proxies` | `[]` | CIDRs allowed to set `X-Forwarded-For`; empty ⇒ headers ignored |
| `zone` | — | this instance's zone (see `load_balancer.prefer_same_zone`) |
| `watch_config` / `watch_interval` | `false` / `5s` | poll the config file and auto-reload on change |
| `upstream.*` | see below | upstream transport tuning |
| `websocket.idle_timeout` / `websocket.max_message_bytes` | `0` (unlimited) | WS guardrails |
| `l4.enabled` / `l4.port` / `l4.dial_timeout` | `false` / — / `5s` | raw TCP (L4) proxy |

`server.upstream`: `dial_timeout` 5s, `tls_handshake_timeout` 5s,
`response_header_timeout` 30s, `expect_continue_timeout` 1s, `idle_conn_timeout` 90s,
`max_idle_conns` 100, `max_idle_conns_per_host` 100, `max_conns_per_host` 0 (unlimited),
`http2` false (h2c/ALPN to backends when true).

## `tls` (data-plane termination)

`enabled`, `cert_file`, `key_file`, `min_version` (`1.2`|`1.3`), `cipher_suites` [],
`certificates` [{cert_file,key_file}] (SNI), `client_auth`
(`none`|`request`|`require_and_verify`), `client_ca_file`, `reload_on_change`.

## `backend_tls` (proxy → https backends)

`insecure_skip_verify`, `ca_file`, `client_cert_file`, `client_key_file` (mTLS to backends).

## `backends`

List of `{url, weight (1), max_conns (100), zone, tier (0), health_check (override)}`.

## `load_balancer`

`algorithm`: `round_robin` (default) | `least_conn` | `weighted` | `swrr` |
`weighted_least_conn` | `weighted_random` | `p2c` | `consistent_hash` | `ewma` | `ip_hash`.
Also: `consistent_hash.{replicas 100, load_factor 1.25}`,
`sticky.{enabled, cookie "rplb_affinity", ttl}`, `slow_start` (0=off),
`prefer_same_zone`, `outlier_detection.{enabled, error_rate_threshold, min_requests,
base_ejection, max_ejection_percent}`, and `health_check` (below).

`load_balancer.health_check`: `enabled`, `type` (`http`|`tcp`), `interval`, `timeout`,
`path`, `method` (GET), `expected_statuses` [] (⇒ 2xx), `expected_body`, `host`,
`headers`, `healthy_threshold` (2), `unhealthy_threshold` (3), `jitter` (0.1),
`startup_grace_period`.

## `routes` (L7 routing)

List of `{name, host, path_prefix, methods[], headers{}, algorithm, consistent_hash,
backends[]}`. First match wins; unmatched ⇒ default group.

## `circuit_breaker`

`enabled`, `mode` (`consecutive`|`rolling`), `failure_threshold`, `success_threshold`,
`timeout`, `rolling_window` (10s), `error_rate_threshold` (0.5), `min_requests` (20),
`trip_on` [`connect`,`timeout`,`5xx`] (default connect+timeout).

## `retry`

`max_attempts`, `backoff`, `max_backoff`, `budget` (0=unlimited), `per_try_timeout`,
`honor_retry_after` (true), `retry_on` [connect,timeout],
`hedge.{enabled, delay, max_extra}`.

## `rate_limiter`

`enabled`, `requests_per_second`, `burst`, `algorithm` (`token_bucket`|`gcra`),
`global_rps`, `global_burst`, `key` (`ip`|`header:<Name>`), `retry_after_seconds`,
`message`, `allowlist` [] (CIDRs), `rules` [{path_prefix, method, rps, burst}].

## `security`

`headers.{enabled, hsts, frame_options, content_type_options, csp, referrer_policy}`,
`cors.{enabled, allow_origins, allow_methods, allow_headers, allow_credentials, max_age}`,
`acl.{allow[], deny[], methods[], blocked_paths[]}`,
`auth.{type (none|basic|apikey|jwt), users{}, api_keys[], header, jwt_secret, jwt_alg}`.

## `canary` / `mirror` / `rewrite` / `fault_injection`

- `canary.{enabled, weight_percent, algorithm, consistent_hash, backends[]}`
- `mirror.{enabled, url, sample_percent, timeout}`
- `rewrite.{request_headers_set{}, request_headers_remove[], response_headers_set{},
  response_headers_remove[], strip_path_prefix, https_redirect}`
- `fault_injection.{enabled, delay_percent, delay, abort_percent, abort_status}`

## `compression` / `logging` / `metrics`

- `compression.{enabled, min_size, content_types[]}`
- `logging.{level (debug|info|warn|error), format (json|text)}`
- `metrics.{enabled, host 127.0.0.1, port 9090, auth_token}` (admin plane: metrics,
  healthz/readyz, reload, pprof)
