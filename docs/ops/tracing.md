# Distributed Tracing

rplb supports OpenTelemetry (OTel) distributed tracing via the `otelhttp` middleware with W3C TraceContext propagation and an OTLP gRPC exporter. When disabled, tracing adds zero overhead — no spans are created and no gRPC connections are opened.

---

## Architecture

```
Client request
  │
  ▼ (incoming W3C traceparent header extracted, or new trace started)
otelhttp middleware  ──── span: "rplb.proxy"
  │                           attributes: http.method, http.url, http.status_code
  │                           span events: retry attempts, hedge issued
  ▼
Upstream backend
  │ (traceparent header injected into upstream request)
  ▼
Response
  │
  ▼ (span finished, exported via OTLP gRPC)
OTLP Collector / Jaeger / Tempo
```

---

## Configuration

```yaml
tracing:
  enabled: true
  endpoint: "http://jaeger:4317"       # OTLP gRPC endpoint (no /v1/traces path needed)
  service_name: "rplb"
  sample_rate: 1.0                     # 1.0 = 100% sampling; 0.1 = 10% sampling
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable OTel tracing |
| `endpoint` | string | — | OTLP gRPC exporter endpoint (`host:port`, no scheme needed for gRPC) |
| `service_name` | string | `rplb` | OTel `service.name` resource attribute |
| `sample_rate` | float | `1.0` | Probability of sampling each trace (0.0–1.0) |

---

## W3C TraceContext propagation

rplb reads the `traceparent` header (W3C TraceContext, RFC 9532) from incoming requests and continues the trace in the upstream call. If no `traceparent` is present, a new root span is started.

The `traceparent` header is injected into all upstream requests:

```
traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
```

This allows end-to-end tracing from client through proxy to backend without any special client-side SDK — any HTTP client that passes through `traceparent` headers gets traced automatically.

If you use `tracestate` for vendor-specific propagation, those headers are also forwarded unchanged.

---

## Span attributes

Each proxied request creates a span with the following attributes:

| Attribute | Value example |
|-----------|--------------|
| `http.method` | `GET` |
| `http.url` | `https://api.example.com/v1/users` |
| `http.status_code` | `200` |
| `http.request_content_length` | `1024` |
| `http.response_content_length` | `2048` |
| `rplb.backend` | `http://backend-1:8000` |
| `rplb.group` | `default` / `canary` / route name |
| `rplb.attempt` | `1` (incremented on retry) |
| `rplb.hedge` | `true` (if this was a hedged request) |
| `net.peer.ip` | `10.0.0.5` |

Span events:
- `retry` — emitted on each retry attempt with the error reason.
- `hedge_issued` — emitted when a hedged request is dispatched.
- `circuit_open` — emitted when a backend circuit was open and failover occurred.

---

## Jaeger setup (development)

Run Jaeger all-in-one for local development:

```bash
docker run -d --name jaeger \
  -e COLLECTOR_OTLP_ENABLED=true \
  -p 16686:16686 \
  -p 4317:4317 \
  jaegertracing/all-in-one:latest
```

Configure rplb to export to it:

```yaml
tracing:
  enabled: true
  endpoint: "localhost:4317"
  service_name: "rplb"
  sample_rate: 1.0
```

Open the Jaeger UI at `http://localhost:16686` and select the `rplb` service.

---

## Grafana Tempo (production)

For production, export to Grafana Tempo via an OTel Collector:

```yaml
# otel-collector-config.yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317

exporters:
  otlp:
    endpoint: "tempo:4317"
    tls:
      insecure: false
      ca_file: "/etc/certs/ca.pem"

service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlp]
```

rplb config:

```yaml
tracing:
  enabled: true
  endpoint: "otel-collector:4317"
  service_name: "rplb"
  sample_rate: 0.1    # 10% in production to reduce volume
```

---

## Sampling strategies

| Strategy | `sample_rate` | When to use |
|---------|--------------|-------------|
| Full sampling | `1.0` | Development, low-traffic |
| 10% | `0.1` | Moderate production traffic |
| 1% | `0.01` | High-traffic production (>1000 RPS) |
| Head-based parent sampling | N/A | Let upstream decide (rplb respects `traceparent` sampling flag) |

If the incoming `traceparent` has the sampling bit set to `1`, rplb always samples that trace regardless of `sample_rate`. This allows upstream services to force-sample specific requests (e.g., debug mode or user-flagged traces).

---

## Request ID correlation

Each request gets a `X-Request-ID` header assigned by the `RequestID` middleware (generated if not present, passed through if already set). The request ID is included in:

- Access log JSON: `{"x-request-id": "abc-123", ...}`
- Span attribute: `rplb.request_id`
- Upstream request headers: `X-Request-ID: abc-123`

This makes it easy to correlate a trace in Jaeger with a specific log line without instrumenting the upstream application:

```bash
# Find all log lines for a specific request
kubectl logs -l app=rplb | grep '"x-request-id":"abc-123"'
```

---

## Disabling tracing (zero overhead)

When `tracing.enabled: false` (the default), the `tracing` package installs a no-op tracer from `go.opentelemetry.io/otel/trace/noop`. No gRPC connections are opened, no spans are allocated, and the hot path adds no overhead beyond a nil-pointer check.
