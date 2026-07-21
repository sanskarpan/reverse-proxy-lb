# Kubernetes Sidecar Deployment

Running rplb as a sidecar container gives each application pod its own local proxy with no shared state and no single point of failure. This pattern is useful for:

- **Service mesh lite:** give each pod outbound load balancing with circuit breaking without the overhead of a full service mesh control plane.
- **Protocol translation:** terminate TLS from callers before passing plain HTTP to the app container.
- **Per-pod rate limiting and auth:** enforce rate limits and JWT validation at the pod level.

---

## Sidecar architecture

```
┌─────────────────────────── Pod ─────────────────────────────┐
│                                                              │
│  ┌──────────────┐   localhost   ┌──────────────────────────┐ │
│  │   app        │──────────────►│  rplb sidecar            │ │
│  │  :8080       │               │  :9000 (outbound proxy)  │ │
│  └──────────────┘               │  :9090 (admin)           │ │
│                                 └──────────┬───────────────┘ │
│                                            │                  │
└────────────────────────────────────────────┼──────────────────┘
                                             │ (to upstream backends in cluster)
                                             ▼
                                    http://backend-svc:8000
```

The application forwards all outbound traffic to `localhost:9000` (rplb sidecar). rplb handles backend discovery, load balancing, retries, circuit breaking, and TLS termination.

---

## Deployment manifest

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app: myapp
  template:
    metadata:
      labels:
        app: myapp
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9090"
        prometheus.io/path: "/metrics"
    spec:
      serviceAccountName: rplb-sidecar-sa  # needs endpoint read access

      containers:
        - name: app
          image: myapp:latest
          ports:
            - containerPort: 8080
          env:
            # Tell the app to use the sidecar as its outbound proxy
            - name: UPSTREAM_BASE_URL
              value: "http://localhost:9000"

        - name: rplb
          image: ghcr.io/sanskarpan/rplb:latest
          args: ["--config", "/etc/rplb/config.yaml"]
          ports:
            - name: proxy
              containerPort: 9000
            - name: admin
              containerPort: 9090
          volumeMounts:
            - name: rplb-config
              mountPath: /etc/rplb
          resources:
            requests:
              cpu: "50m"
              memory: "32Mi"
            limits:
              cpu: "200m"
              memory: "128Mi"
          livenessProbe:
            httpGet:
              path: /healthz
              port: 9090
            initialDelaySeconds: 5
            periodSeconds: 10
          readinessProbe:
            httpGet:
              path: /readyz
              port: 9090
            initialDelaySeconds: 5
            periodSeconds: 5

      volumes:
        - name: rplb-config
          configMap:
            name: rplb-sidecar-config
```

---

## ConfigMap

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: rplb-sidecar-config
  namespace: default
data:
  config.yaml: |
    server:
      host: "0.0.0.0"
      port: 9000
      read_timeout: 30s
      write_timeout: 30s

    discovery:
      kubernetes:
        enabled: true
        namespace: default
        service: backend-svc
        port_name: http

    load_balancer:
      algorithm: least_conn
      health_check:
        enabled: true
        interval: 10s
        path: "/health"

    circuit_breaker:
      enabled: true
      mode: consecutive
      failure_threshold: 5
      timeout: 30s

    retry:
      max_attempts: 3
      backoff: exponential
      max_backoff: 5s

    metrics:
      enabled: true
      host: "0.0.0.0"   # allow Prometheus scraping from outside the pod
      port: 9090

    logging:
      level: info
      format: json
```

---

## RBAC for sidecar

The sidecar needs read access to Endpoints and EndpointSlices for the Kubernetes discovery feature:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: rplb-sidecar-sa
  namespace: default

---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: rplb-sidecar-discovery
rules:
  - apiGroups: [""]
    resources: ["endpoints"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["get", "list", "watch"]

---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: rplb-sidecar-discovery-binding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: rplb-sidecar-discovery
subjects:
  - kind: ServiceAccount
    name: rplb-sidecar-sa
    namespace: default
```

---

## Inbound proxy (mTLS termination)

For the reverse direction — terminating mTLS from callers before the app container — configure the sidecar on the inbound path:

```
Caller ──(mTLS)──► rplb sidecar :8443 ──(HTTP)──► app :8080
```

```yaml
# config.yaml for inbound sidecar
server:
  host: "0.0.0.0"
  port: 8443

tls:
  enabled: true
  cert_file: "/etc/certs/tls.crt"
  key_file: "/etc/certs/tls.key"
  client_auth: "require_and_verify"
  client_ca_file: "/etc/certs/ca.crt"

backends:
  - url: "http://localhost:8080"    # forward to app container
    max_conns: 1000
```

Mount the TLS certificates from a Kubernetes Secret:

```yaml
volumeMounts:
  - name: tls-certs
    mountPath: /etc/certs
    readOnly: true

volumes:
  - name: tls-certs
    secret:
      secretName: myapp-tls
```

---

## Resource sizing

rplb is designed to be lightweight. Typical sidecar resource usage:

| Traffic | CPU | Memory |
|---------|-----|--------|
| < 100 RPS | 10–20m CPU | 16–32 MiB |
| 100–1000 RPS | 50–100m CPU | 32–64 MiB |
| > 1000 RPS | 100–200m CPU | 64–128 MiB |

The hot path (healthy backend list cache, zero-alloc) keeps CPU usage low even at high throughput. PGO (`make pgo-build`) provides an additional 2–7% CPU reduction on the hottest paths.

---

## Versus a full service mesh

| Feature | rplb sidecar | Envoy/Istio |
|---------|-------------|-------------|
| Control plane | None — config file | Pilot/xDS |
| mTLS between pods | Manual cert distribution | Automatic SPIFFE/SVID |
| Traffic policies | Config file | CRDs (VirtualService, DestinationRule) |
| Observability | Prometheus + OTel | Mixer/Telemetry v2 |
| Resource overhead | ~16 MiB / 20m CPU | ~50–200 MiB / 100–500m CPU |
| Complexity | Low | High |

rplb sidecar is appropriate when you want load balancing, circuit breaking, and observability without the operational complexity of a full service mesh control plane.
