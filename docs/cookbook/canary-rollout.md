# Canary Rollout Recipe

This recipe walks through a full canary deployment: starting at 5% traffic, automatically promoting to 100% over time, and rolling back automatically on error rate breach.

---

## Overview

```
t=0:    deploy v2 to canary backends, set weight=5%
t=5m:   auto-promoter checks error rate; weight → 15%
t=10m:  weight → 25%
...
t=50m:  weight → 100%; v2 is the new default
t=?:    if error rate exceeds 1% at any step, weight → 0%; alert fires
```

---

## Step 1: Deploy v2 to canary backends

Deploy the new version to dedicated canary backend instances (separate from the current production pool):

```bash
# Kubernetes example
kubectl set image deployment/app-canary app=myapp:v2.0.0
kubectl rollout status deployment/app-canary
```

Verify the canary backends are running:

```bash
curl http://canary-1:8000/health
```

---

## Step 2: Configure the canary in rplb

```yaml
# config.yaml
backends:
  - url: "http://prod-1:8000"
    weight: 1
  - url: "http://prod-2:8000"
    weight: 1

canary:
  enabled: true
  weight_percent: 5       # start with 5% of traffic
  algorithm: round_robin
  backends:
    - url: "http://canary-1:8000"
      weight: 1
    - url: "http://canary-2:8000"
      weight: 1

  auto_promote:
    enabled: true
    step_percent: 10        # increase by 10% per step
    step_interval: 5m       # evaluate every 5 minutes
    max_error_rate: 0.01    # roll back if canary error rate > 1%
    min_requests: 100       # require at least 100 requests before evaluating

load_balancer:
  algorithm: round_robin
  health_check:
    enabled: true
    interval: 10s
    path: "/health"
```

---

## Step 3: Activate the canary

```bash
# Reload config to activate
kill -HUP $(pidof proxy)

# Verify the canary is active
curl http://localhost:9090/admin/backends
```

Expected response:

```json
{
  "groups": {
    "default": {
      "backends": [
        {"url": "http://prod-1:8000", "healthy": true},
        {"url": "http://prod-2:8000", "healthy": true}
      ]
    },
    "canary": {
      "weight_percent": 5,
      "backends": [
        {"url": "http://canary-1:8000", "healthy": true},
        {"url": "http://canary-2:8000", "healthy": true}
      ]
    }
  }
}
```

---

## Step 4: Monitor the canary

Watch canary error rate in real time:

```bash
# Error rate over 5 minutes
promtool query instant http://localhost:9090/metrics \
  'rate(rplb_errors_total{group="canary"}[5m]) / rate(rplb_requests_total{group="canary"}[5m])'
```

Or using curl and simple math:

```bash
#!/bin/bash
while true; do
  METRICS=$(curl -s http://localhost:9090/metrics)
  REQ=$(echo "$METRICS" | grep 'rplb_requests_total{.*group="canary"' | awk '{print $2}')
  ERR=$(echo "$METRICS" | grep 'rplb_errors_total{.*group="canary"' | awk '{print $2}')
  echo "$(date) canary requests=$REQ errors=$ERR"
  sleep 30
done
```

Watch the auto-promoter log output:

```bash
journalctl -u rplb -f | grep -i canary
```

You will see entries like:

```json
{"level":"INFO","msg":"canary promoted","weight_percent":15,"prev":5}
{"level":"INFO","msg":"canary promoted","weight_percent":25,"prev":15}
```

---

## Rollback: automatic

If the canary error rate breaches `max_error_rate`, rplb rolls back automatically:

```json
{"level":"WARN","msg":"canary rolled back","reason":"error_rate_exceeded","error_rate":0.023,"threshold":0.01,"weight_percent":0}
```

All traffic immediately returns to the default backends. The canary remains at `weight_percent: 0` until you manually intervene.

## Rollback: manual

You can also roll back immediately at any time:

```bash
# Edit config to set weight_percent: 0
# Then reload:
kill -HUP $(pidof proxy)
```

Or adjust weight without editing the config file:

```bash
# This drains the canary backends directly
curl -XPOST 'http://localhost:9090/admin/drain?url=http://canary-1:8000'
curl -XPOST 'http://localhost:9090/admin/drain?url=http://canary-2:8000'
```

---

## Step 5: Complete the rollout

When `weight_percent` reaches 100%, all traffic goes to the canary backends. At this point, rename them as the new production pool:

```yaml
# Final config — canary backends become production
backends:
  - url: "http://canary-1:8000"    # renamed to prod-v2-1 in your infra
    weight: 1
  - url: "http://canary-2:8000"    # renamed to prod-v2-2 in your infra
    weight: 1

# Remove the canary block or disable it
canary:
  enabled: false
```

```bash
kill -HUP $(pidof proxy)
```

Then decommission the old production backends (prod-1, prod-2).

---

## Canary with user cohort stickiness

For A/B testing where you want the same user to consistently hit the canary (rather than random per request), combine consistent hash with canary:

```yaml
canary:
  enabled: true
  weight_percent: 20
  algorithm: consistent_hash
  consistent_hash:
    replicas: 100
    load_factor: 1.0
  backends:
    - url: "http://canary-1:8000"
```

Users are assigned to either canary or default based on a hash of their session/user ID. The same user sees the same version on every request until `weight_percent` changes.

---

## Integrating with CI/CD

A full CI/CD pipeline for canary promotion:

```bash
#!/bin/bash
set -e

NEW_IMAGE=$1  # e.g., myapp:v2.0.0

echo "1. Deploy new version to canary backends..."
kubectl set image deployment/app-canary app=$NEW_IMAGE
kubectl rollout status deployment/app-canary --timeout=5m

echo "2. Validate canary health..."
sleep 30
STATUS=$(curl -sf http://canary-1:8000/health -o /dev/null -w "%{http_code}")
[ "$STATUS" = "200" ] || { echo "Canary health check failed"; exit 1; }

echo "3. Enable canary at 5%..."
sed -i 's/weight_percent: 0/weight_percent: 5/' /etc/rplb/config.yaml
kill -HUP $(pidof proxy)

echo "4. Waiting for auto-promote to complete (canary will reach 100% in ~50m)..."
echo "   Monitoring: tail -f /var/log/rplb/access.log | grep canary"
echo "   Dashboard: http://localhost:9090/metrics"
echo ""
echo "   Auto-rollback is active. If error rate > 1%, rollback is automatic."
echo "   Check: journalctl -u rplb | grep -i canary"
```
