# Blue/Green Deployment

A blue/green deployment keeps two identical production environments (blue = current, green = next) and switches traffic instantly between them. Unlike a rolling update, all traffic shifts at once — eliminating the period where clients may hit either version.

rplb supports blue/green via weighted backends and the drain API.

---

## Setup

Define both environments in the backend pool with initial weights:

```yaml
backends:
  # Blue (current production) — receives all traffic
  - url: "http://blue-1:8000"
    weight: 100
    max_conns: 200
  - url: "http://blue-2:8000"
    weight: 100
    max_conns: 200

  # Green (next version) — receives no traffic yet
  - url: "http://green-1:8000"
    weight: 0
    max_conns: 200
  - url: "http://green-2:8000"
    weight: 0
    max_conns: 200

load_balancer:
  algorithm: weighted
  health_check:
    enabled: true
    interval: 5s
    path: "/health"
```

Green backends with `weight: 0` are kept in the pool and health-checked, but receive no traffic from the weighted algorithm until their weight is raised.

---

## Promotion procedure

### Step 1: Deploy green and verify it is healthy

```bash
# Deploy your new version to green backends
kubectl set image deployment/green app=myapp:v2.0.0

# Wait for green to become healthy in rplb
watch -n2 "curl -s http://localhost:9090/admin/backends | python3 -m json.tool | grep -A3 green"
```

Expect to see `"healthy": true` for both green backends before proceeding.

### Step 2: Shift all traffic to green

Edit `config.yaml`:

```yaml
backends:
  - url: "http://blue-1:8000"
    weight: 0       # blue gets no new traffic
  - url: "http://blue-2:8000"
    weight: 0
  - url: "http://green-1:8000"
    weight: 100     # green receives all traffic
  - url: "http://green-2:8000"
    weight: 100
```

Reload config:

```bash
kill -HUP $(pidof proxy)
# or
curl -XPOST http://localhost:9090/reload
```

This is an instant, atomic switch. All new connections go to green. In-flight requests to blue complete normally.

### Step 3: Drain blue (wait for in-flight to finish)

```bash
curl -XPOST 'http://localhost:9090/admin/drain?url=http://blue-1:8000'
curl -XPOST 'http://localhost:9090/admin/drain?url=http://blue-2:8000'

# Watch active connections drain to 0
watch -n1 "curl -s http://localhost:9090/admin/backends"
```

### Step 4: Verify green is healthy under full load

```bash
# Check error rate
curl -s http://localhost:9090/metrics | grep rplb_errors_total

# Check latency
curl -s http://localhost:9090/metrics | grep rplb_response_latency
```

Monitor for 5–10 minutes at full traffic before declaring the deployment complete.

### Step 5: Decommission blue

Once green is stable, remove blue from the config and reload again:

```yaml
backends:
  - url: "http://green-1:8000"
    weight: 100
  - url: "http://green-2:8000"
    weight: 100
```

```bash
kill -HUP $(pidof proxy)
```

---

## Rollback procedure

If green shows problems, rollback is identical to promotion but in reverse:

```yaml
backends:
  - url: "http://blue-1:8000"
    weight: 100     # restore blue
  - url: "http://blue-2:8000"
    weight: 100
  - url: "http://green-1:8000"
    weight: 0       # stop green
  - url: "http://green-2:8000"
    weight: 0
```

```bash
kill -HUP $(pidof proxy)
```

Total rollback time: the time for one config reload — typically < 1 second.

---

## Monitoring the switch

Watch the error rate in real time during the traffic shift:

```bash
# Terminal 1: tail error rate
while true; do
  echo -n "$(date) errors/s: "
  curl -s http://localhost:9090/metrics | grep '^rplb_errors_total ' | awk '{print $2}'
  sleep 5
done

# Terminal 2: watch backend status
watch -n2 "curl -s http://localhost:9090/admin/backends"
```

Alert if error rate exceeds your SLO threshold during or after the switch.

---

## Blue/green with health check validation

Add a validation gate before promoting green:

```bash
#!/bin/bash
set -e

GREEN_URL=http://green-1:8000

# Smoke test green directly
echo "Testing green directly..."
STATUS=$(curl -sf -o /dev/null -w "%{http_code}" $GREEN_URL/health)
if [ "$STATUS" != "200" ]; then
  echo "Green health check failed: $STATUS"
  exit 1
fi

# Check rplb sees green as healthy
echo "Checking rplb backend status..."
HEALTHY=$(curl -s http://localhost:9090/admin/backends | python3 -c "
import json, sys
data = json.load(sys.stdin)
greens = [b for b in data.get('backends', []) if 'green' in b['url']]
print('true' if all(b['healthy'] for b in greens) else 'false')
")

if [ "$HEALTHY" != "true" ]; then
  echo "Green backends not all healthy in rplb"
  exit 1
fi

echo "Green is healthy — promoting..."
# ... proceed with config update and reload
```
