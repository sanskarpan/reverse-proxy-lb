# Kubernetes Deployment

rplb ships with Kubernetes manifests (`deploy/k8s/`) and a Helm chart (`deploy/helm/rplb/`) for production cluster deployments. It integrates natively with the Kubernetes Endpoints API for zero-TTL backend discovery.

---

## Prerequisites

| Requirement | Minimum version |
|-------------|----------------|
| Kubernetes | 1.21+ |
| Helm | 3+ |
| `kubectl` | configured against target cluster |

---

## Install with Helm

```bash
helm install rplb ./deploy/helm/rplb -n rplb --create-namespace
```

Verify the rollout:

```bash
kubectl -n rplb rollout status deployment/rplb-rplb
kubectl -n rplb get pods
```

Expected output:

```
Waiting for deployment "rplb-rplb" rollout to finish: 0 of 1 updated replicas are available...
deployment "rplb-rplb" successfully rolled out

NAME                        READY   STATUS    RESTARTS   AGE
rplb-rplb-5d89b7f9c-xk2p8  1/1     Running   0          45s
```

---

## RBAC

The chart creates a `ClusterRole` named `<release>-rplb-discovery` with read-only access to:

- `endpoints` (core API group) — classic service endpoint discovery
- `endpointslices` (`discovery.k8s.io`) — scalable endpoint discovery on Kubernetes 1.21+

A `ClusterRoleBinding` attaches the role to the chart-managed `ServiceAccount`. This allows rplb to watch backend pod IPs directly from the Kubernetes API, bypassing kube-proxy and DNS TTL delays. When a pod is evicted or fails, rplb learns within milliseconds (from the watch event) rather than waiting for DNS TTL expiry.

To manage RBAC externally:

```yaml
# values.yaml
serviceAccount:
  create: false
  name: my-existing-sa  # must already have endpoint/endpointslice read access
```

---

## Kubernetes service discovery

Enable automatic endpoint discovery:

```yaml
# values.yaml or --set flags
config:
  discovery:
    kubernetes:
      enabled: true
      namespace: default
      service: my-backend
      port_name: http
```

rplb watches `EndpointSlices` for the named service and dynamically adds/removes backends as pods start and stop. Ensure `serviceAccount.create: true` (the default) so the RBAC bindings are in place.

Manual `backends` entries and discovery-managed backends coexist in the same pool — discovery only manages the backends it created.

---

## Configuration overrides

### Via `--set` flags

```bash
helm install rplb ./deploy/helm/rplb -n rplb --create-namespace \
  --set image.tag=v1.2.3 \
  --set replicaCount=3 \
  --set config.load_balancer.algorithm=least_conn
```

### Via custom values file

```bash
helm install rplb ./deploy/helm/rplb -n rplb --create-namespace \
  -f my-values.yaml
```

Example `my-values.yaml`:

```yaml
replicaCount: 3

image:
  repository: ghcr.io/sanskarpan/rplb
  tag: "v1.2.3"
  pullPolicy: IfNotPresent

config:
  server:
    host: 0.0.0.0
    port: 8080

  backends:
    - url: "http://my-svc.default.svc.cluster.local:8000"
      weight: 1

  load_balancer:
    algorithm: round_robin
    health_check:
      enabled: true
      interval: 10s
      path: "/health"

  discovery:
    kubernetes:
      enabled: true
      namespace: default
      service: my-backend
      port_name: http

  logging:
    level: info
    format: json

  metrics:
    enabled: true
    port: 9090
```

---

## Upgrading

Apply a new image tag:

```bash
helm upgrade rplb ./deploy/helm/rplb -n rplb --set image.tag=v1.3.0
```

Apply a values file change (e.g., add a backend):

```bash
helm upgrade rplb ./deploy/helm/rplb -n rplb -f my-values.yaml
```

Always validate the config before upgrading:

```bash
kubectl -n rplb exec deploy/rplb-rplb -- \
  /bin/proxy --validate --config /etc/rplb/config.yaml
```

---

## Autoscaling (HPA)

Enable Horizontal Pod Autoscaler to scale on CPU utilization:

```yaml
# values.yaml
autoscaling:
  enabled: true
  minReplicas: 2
  maxReplicas: 10
  targetCPUUtilizationPercentage: 80
```

Since rplb is stateless (no per-pod session state beyond the in-memory rate-limit counters), horizontal scaling works seamlessly. For distributed rate limiting across replicas, configure the Redis store — see [Rate Limiting](../resilience/rate-limiting.md).

---

## Health probes

The Deployment configures liveness and readiness probes targeting the admin plane:

```yaml
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
```

| Endpoint | Meaning |
|----------|---------|
| `/healthz` | Liveness — always 200 while the process is running |
| `/readyz` | Readiness — 200 only when at least one backend is healthy; 503 otherwise |

Kubernetes removes a pod from Service endpoints when `/readyz` returns non-200. This means a pod with all backends failing does not receive new traffic, and it drains existing connections during shutdown.

Set `terminationGracePeriodSeconds` to at least `server.shutdown_timeout` (default 30s) to allow in-flight requests to complete:

```yaml
# Deployment spec
terminationGracePeriodSeconds: 35
```

---

## Prometheus monitoring

Pods are annotated for Prometheus scraping out of the box:

```yaml
annotations:
  prometheus.io/scrape: "true"
  prometheus.io/port: "9090"
  prometheus.io/path: "/metrics"
```

If you use a Prometheus Operator `ServiceMonitor` instead:

```yaml
# values.yaml
serviceMonitor:
  enabled: true
  interval: 15s
  path: /metrics
  port: admin
```

---

## Ingress (rplb as the ingress controller)

For running rplb as the cluster ingress (receiving external traffic):

```yaml
# values.yaml
service:
  type: LoadBalancer
  port: 443
  nodePort: null

tls:
  enabled: true
  acme:
    enabled: true
    domains:
      - "api.example.com"
    cache_dir: "/var/cache/rplb/acme"
```

Mount a PersistentVolumeClaim for the ACME cache directory so certificates persist across pod restarts:

```yaml
volumes:
  - name: acme-cache
    persistentVolumeClaim:
      claimName: rplb-acme-cache

volumeMounts:
  - name: acme-cache
    mountPath: /var/cache/rplb/acme
```

---

## Uninstall

```bash
helm uninstall rplb -n rplb
kubectl delete namespace rplb
```

`ClusterRole` and `ClusterRoleBinding` are cluster-scoped and removed with the release. The namespace deletion removes all remaining namespaced resources.
