# Kubernetes Deployment Guide

## Prerequisites

- Kubernetes 1.21+
- Helm 3+
- `kubectl` configured against your target cluster

## RBAC

The chart creates a `ClusterRole` named `<release>-rplb-discovery` with read-only access to:

- `endpoints` (core API group) — used for classic service endpoint discovery
- `endpointslices` (`discovery.k8s.io`) — used for scalable endpoint discovery on Kubernetes 1.21+

This allows rplb to watch backend pod IPs directly from the Kubernetes API without depending on kube-proxy or DNS TTLs. A `ClusterRoleBinding` attaches the role to the chart-managed `ServiceAccount`.

To disable RBAC resource creation (when managing RBAC externally):

```yaml
serviceAccount:
  create: false
  name: my-existing-sa
```

## Installation

```bash
helm install rplb ./helm/rplb -n rplb --create-namespace
```

Verify the rollout:

```bash
kubectl -n rplb rollout status deployment/rplb-rplb
kubectl -n rplb get pods
```

## Configuration

Override values at install time or via a custom `values.yaml`:

```bash
helm install rplb ./helm/rplb -n rplb --create-namespace \
  --set image.tag=v1.2.3 \
  --set replicaCount=3
```

Backend list is controlled by `config.server` and `config.backends` in `values.yaml`:

```yaml
config:
  server:
    host: 0.0.0.0
    port: 8080
  backends:
    - url: "http://my-svc.default.svc.cluster.local:8000"
      weight: 1
  load_balancer:
    algorithm: round_robin
```

## Kubernetes Service Discovery

Enable automatic endpoint discovery so rplb tracks pod IPs without static backend URLs:

```yaml
config:
  discovery:
    kubernetes:
      enabled: true
      namespace: default
      service: my-backend
      port_name: http
```

rplb watches `EndpointSlices` for the named service and adds/removes backends as pods come and go. Ensure `serviceAccount.create: true` (the default) so the RBAC bindings are in place.

## Upgrade

```bash
helm upgrade rplb ./helm/rplb -n rplb
```

To apply a new image tag:

```bash
helm upgrade rplb ./helm/rplb -n rplb --set image.tag=v1.3.0
```

## Autoscaling

Enable HPA to scale between `minReplicas` and `maxReplicas` based on CPU:

```yaml
autoscaling:
  enabled: true
  minReplicas: 2
  maxReplicas: 10
  targetCPUUtilizationPercentage: 80
```

## Monitoring

Pods are annotated for Prometheus scraping out of the box:

```
prometheus.io/scrape: "true"
prometheus.io/port: "9090"
prometheus.io/path: "/metrics"
```

The metrics server also exposes liveness (`/healthz`) and readiness (`/readyz`) on port 9090. The Deployment probes target these endpoints.

## Uninstall

```bash
helm uninstall rplb -n rplb
kubectl delete namespace rplb
```

Note: `ClusterRole` and `ClusterRoleBinding` are cluster-scoped and will be removed with the release. The namespace deletion above removes all remaining namespaced resources.
