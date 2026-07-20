#!/usr/bin/env bash
set -euo pipefail
# Start the proxy with pprof enabled, run a quick load test, collect CPU profile
BINARY=./bin/proxy
CONFIG=configs/config.docker.yaml
PPROF_URL=http://localhost:9090/debug/pprof/profile?seconds=10

echo "Building proxy binary..."
go build -o "$BINARY" ./cmd/proxy/

echo "Starting proxy in background..."
"$BINARY" --config "$CONFIG" &
PROXY_PID=$!
trap "kill $PROXY_PID 2>/dev/null; exit" EXIT
sleep 2

echo "Collecting CPU profile (10s)..."
curl -s -o default.pgo "$PPROF_URL" || { echo "pprof collection failed (proxy may not be listening)"; exit 1; }
echo "Profile saved to default.pgo ($(wc -c < default.pgo) bytes)"

kill $PROXY_PID
echo "Done."
