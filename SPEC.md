# Production-Grade Reverse Proxy with Load Balancing

## Project Overview
- **Project Name**: Reverse Proxy Load Balancer
- **Type**: Network Application / Infrastructure
- **Core Functionality**: A production-ready reverse proxy server with intelligent load balancing, health monitoring, circuit breaking, rate limiting, and comprehensive observability
- **Target Users**: DevOps engineers, backend developers, system architects

## Architecture

### Components
1. **Reverse Proxy Server** - Handles incoming HTTP/HTTPS requests
2. **Load Balancer** - Distributes traffic across backend servers
3. **Health Checker** - Monitors backend server health
4. **Circuit Breaker** - Prevents cascade failures
5. **Rate Limiter** - Protects backends from overload
6. **Metrics Collector** - Observability and monitoring
7. **Configuration Manager** - Hot reload support
8. **Connection Pool** - Reuses connections to backends

### Project Structure
```
Reverse-Proxy-Load-Balancing/
├── cmd/
│   └── proxy/
│       └── main.go
├── internal/
│   ├── config/
│   │   └── config.go
│   ├── proxy/
│   │   ├── proxy.go
│   │   └── handler.go
│   ├── balancer/
│   │   ├── balancer.go
│   │   ├── roundrobin.go
│   │   ├── leastconn.go
│   │   ├── weighted.go
│   │   └── iphash.go
│   ├── health/
│   │   └── health.go
│   ├── circuit/
│   │   └── breaker.go
│   ├── limiter/
│   │   └── limiter.go
│   ├── metrics/
│   │   └── metrics.go
│   ├── logging/
│   │   └── logger.go
│   ├── middleware/
│   │   └── middleware.go
│   └── server/
│       └── server.go
├── configs/
│   └── config.yaml
├── test/
│   └── test_server.go
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

## Functionality Specification

### 1. Load Balancing Algorithms
- **Round Robin**: Sequential distribution
- **Least Connections**: Routes to server with fewest active connections
- **Weighted Round Robin**: Server-specific weights for traffic distribution
- **IP Hash**: Consistent hashing based on client IP

### 2. Health Checking
- Periodic HTTP health checks to backends
- Configurable interval and timeout
- Automatic removal of unhealthy servers
- Recovery detection and re-addition

### 3. Circuit Breaker
- Failure threshold tracking
- Half-open state for testing recovery
- Configurable reset timeout
- Prevents cascade failures

### 4. Rate Limiting
- Token bucket algorithm
- Per-IP and global rate limiting
- Configurable requests per second
- Burst allowance

### 5. Retry Logic
- Automatic retry on failed requests
- Configurable max retries
- Retry on specific status codes
- Exponential backoff

### 6. Connection Pooling
- Reuses HTTP connections to backends
- Configurable pool size per backend
- Keep-alive support

### 7. Metrics & Observability
- Request count, latency percentiles
- Error rates by type
- Backend health status
- Active connections

### 8. Request/Response Handling
- Header forwarding (X-Forwarded-For, X-Real-IP, etc.)
- Request/Response logging
- Gzip compression support
- WebSocket support

### 9. Configuration Management
- YAML-based configuration
- Hot reload support via signal
- Environment variable support

### 10. TLS/SSL Support
- HTTPS termination
- Backend TLS support
- Certificate management

### 11. Graceful Shutdown
- Wait for in-flight requests
- Drain connections properly
- Cleanup resources

## Configuration Specification

```yaml
server:
  host: "0.0.0.0"
  port: 8080
  read_timeout: 30s
  write_timeout: 30s
  idle_timeout: 120s

tls:
  enabled: false
  cert_file: ""
  key_file: ""

backends:
  - url: "http://localhost:8001"
    weight: 1
    max_conns: 100
  - url: "http://localhost:8002"
    weight: 2
    max_conns: 100

load_balancer:
  algorithm: "round_robin"  # round_robin, least_conn, weighted, ip_hash
  health_check:
    enabled: true
    interval: 10s
    timeout: 5s
    path: "/health"

circuit_breaker:
  enabled: true
  failure_threshold: 5
  success_threshold: 2
  timeout: 30s

rate_limiter:
  enabled: true
  requests_per_second: 100
  burst: 200

retry:
  max_attempts: 3
  backoff: "exponential"
  max_backoff: 10s

logging:
  level: "info"
  format: "json"

metrics:
  enabled: true
  port: 9090
```

## Acceptance Criteria

1. **Build Success**: Code compiles without errors
2. **Load Balancing**: Traffic correctly distributed across backends
3. **Health Checks**: Unhealthy backends are automatically removed
4. **Circuit Breaker**: Failed backends are isolated
5. **Rate Limiting**: Excess requests are rejected
6. **Graceful Shutdown**: No dropped requests on shutdown
7. **Hot Reload**: Configuration changes without restart
8. **Metrics**: Stats are accurately reported
9. **Tests Pass**: All unit and integration tests pass

## API Endpoints

- `GET /health` - Proxy health check
- `GET /metrics` - Prometheus metrics (if enabled)
- `POST /reload` - Trigger config reload (if enabled)
