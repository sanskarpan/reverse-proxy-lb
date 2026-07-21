package circuit

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"reverse-proxy-lb/internal/balancer"
)

// syncLuaScript atomically writes this replica's circuit state to a Redis hash
// and returns all fields of that hash so the caller can inspect other replicas
// in the same key. The hash key is per-backend; each replica writes to its own
// field within that hash.
//
// KEYS[1]  = hash key (e.g. "rplb:cb:<backend-hash>")
// ARGV[1]  = replica ID (hostname:pid or random string)
// ARGV[2]  = state string ("open"|"closed"|"half-open")
// ARGV[3]  = failures count as string
// ARGV[4]  = TTL in seconds
//
// Returns the full HGETALL of the updated hash.
const syncLuaScript = `
local key     = KEYS[1]
local replica = ARGV[1]
local state   = ARGV[2]
local failures= ARGV[3]
local ttl     = tonumber(ARGV[4])
redis.call('HSET', key,
    replica .. ':state',    state,
    replica .. ':failures', failures,
    replica .. ':updated',  redis.call('TIME')[1])
redis.call('EXPIRE', key, ttl)
return redis.call('HGETALL', key)
`

// redisClient is the minimal Redis surface used by RedisSyncer. Keeping it as
// an interface makes it trivially injectable for unit tests without miniredis.
type redisClient interface {
	// Eval runs a Lua script against the server.
	Eval(ctx context.Context, script string, keys []string, args ...interface{}) redisResult
	// HGetAll returns all fields of a Redis hash.
	HGetAll(ctx context.Context, key string) redisStringMapResult
	// Keys returns all keys matching the given pattern.
	Keys(ctx context.Context, pattern string) redisStringSliceResult
	// Close releases the client.
	Close() error
}

// redisResult wraps the result of a Redis Eval call to avoid importing
// go-redis types at the interface boundary.
type redisResult interface {
	Result() (interface{}, error)
}

// redisStringMapResult wraps the result of a Redis HGetAll call.
type redisStringMapResult interface {
	Result() (map[string]string, error)
}

// redisStringSliceResult wraps the result of a Redis Keys call.
type redisStringSliceResult interface {
	Result() ([]string, error)
}

// goRedisClientAdapter wraps a *redis.Client from go-redis/v9 to satisfy the
// redisClient interface. It is used in production; tests use a mock.
// The adapter is defined in redis_sync_adapter.go (generated only when the real
// Redis client is imported). We keep it here as a compile-time guard: if
// go-redis is not available the build fails loudly rather than silently.

// RedisSyncer asynchronously synchronises per-backend circuit-breaker state
// across replica instances via a Redis hash. It does NOT replace the local
// circuit breaker; it adds a lightweight async overlay:
//
//   - Every SyncInterval it pushes local state to Redis.
//   - It reads all sibling replica states from the same key.
//   - If any sibling reports StateOpen for the same backend, the local circuit
//     is opened immediately via ForceOpen.
//   - On Redis failure it logs a warning and continues with local state only.
type RedisSyncer struct {
	cb        *CircuitBreaker
	client    redisClient
	replicaID string
	prefix    string
	ttl       time.Duration
	interval  time.Duration

	mu       sync.Mutex
	backends map[*balancer.Backend]string // backend -> stable key suffix

	stop chan struct{}
	done chan struct{}
}

// NewRedisSyncer creates a RedisSyncer and starts its background goroutine.
// replicaID identifies this instance in the shared hash (defaults to
// hostname:pid). Call Stop() to clean up.
func NewRedisSyncer(
	cb *CircuitBreaker,
	client redisClient,
	prefix string,
	ttl time.Duration,
	interval time.Duration,
	replicaID string,
) *RedisSyncer {
	if prefix == "" {
		prefix = "rplb:cb"
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if interval <= 0 {
		interval = 1 * time.Second
	}
	if replicaID == "" {
		replicaID = defaultReplicaID()
	}
	s := &RedisSyncer{
		cb:        cb,
		client:    client,
		replicaID: replicaID,
		prefix:    prefix,
		ttl:       ttl,
		interval:  interval,
		backends:  make(map[*balancer.Backend]string),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}

	// Read any existing hook so we can compose it with the Redis push below.
	// This lets callers (e.g. server.go) register a logging hook before creating
	// the syncer without losing their hook when we overwrite SetOnStateChange.
	cb.mu.Lock()
	prevHook := cb.onStateChange
	cb.mu.Unlock()

	// Hook state changes so an OPEN transition is pushed to Redis immediately
	// rather than waiting for the next tick. A bounded context prevents a
	// slow/unreachable Redis from blocking the request goroutine indefinitely
	// (Bug 5: the hook fires on the request path; context.Background() with no
	// deadline can block for TCP-timeout duration under a Redis outage).
	cb.SetOnStateChange(func(b *balancer.Backend, from, to State) {
		if prevHook != nil {
			prevHook(b, from, to)
		}
		if to == StateOpen {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			s.pushOne(ctx, b)
		}
	})

	go s.loop()
	return s
}

// Track registers a backend so the syncer includes it in each sync cycle.
// It is idempotent. Call it for every backend managed by the associated
// CircuitBreaker.
func (s *RedisSyncer) Track(b *balancer.Backend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.backends[b]; !ok {
		s.backends[b] = backendKey(b.URL)
	}
}

// Stop halts the background sync loop and waits for it to exit.
func (s *RedisSyncer) Stop() {
	close(s.stop)
	<-s.done
}

// loop runs the periodic push+pull cycle.
func (s *RedisSyncer) loop() {
	defer close(s.done)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.sync()
		}
	}
}

// sync pushes local state and pulls remote state for all tracked backends.
func (s *RedisSyncer) sync() {
	ctx, cancel := context.WithTimeout(context.Background(), s.interval/2+time.Millisecond)
	defer cancel()

	s.mu.Lock()
	backends := make([]*balancer.Backend, 0, len(s.backends))
	for b := range s.backends {
		backends = append(backends, b)
	}
	s.mu.Unlock()

	for _, b := range backends {
		raw, err := s.pushAndRead(ctx, b)
		if err != nil {
			log.Printf("[circuit/redis_sync] Redis error for backend %s: %v (continuing with local state)", b.URL, err)
			continue
		}
		s.applyRemote(b, raw)
	}
}

// pushOne is called immediately on state changes to push current state without
// waiting for the next tick.
func (s *RedisSyncer) pushOne(ctx context.Context, b *balancer.Backend) {
	raw, err := s.pushAndRead(ctx, b)
	if err != nil {
		log.Printf("[circuit/redis_sync] Redis push error for backend %s: %v", b.URL, err)
		return
	}
	s.applyRemote(b, raw)
}

// pushAndRead calls the Lua script to atomically write this replica's state and
// return the current hash, then returns the full hash as a flat string map.
func (s *RedisSyncer) pushAndRead(ctx context.Context, b *balancer.Backend) (map[string]string, error) {
	s.mu.Lock()
	keyHash, ok := s.backends[b]
	s.mu.Unlock()
	if !ok {
		keyHash = backendKey(b.URL)
	}

	redisKey := s.prefix + ":" + keyHash

	// Read local state and failure count under the circuit breaker's lock in a
	// single acquisition to avoid the redundant double-GetState call (Bug 4).
	var localState State
	failures := 0
	s.cb.mu.Lock()
	if bst, exists := s.cb.backendStates[b]; exists {
		localState = bst.state
		failures = bst.failures
	}
	s.cb.mu.Unlock()

	result := s.client.Eval(ctx, syncLuaScript, []string{redisKey},
		s.replicaID,
		localState.String(),
		strconv.Itoa(failures),
		strconv.Itoa(int(s.ttl.Seconds())),
	)
	raw, err := result.Result()
	if err != nil {
		return nil, err
	}

	// The Lua script returns HGETALL as a flat []interface{} of alternating
	// field/value pairs. Convert to map[string]string.
	return hgetallToMap(raw), nil
}

// applyRemote checks whether any sibling replica is OPEN for the given backend
// and, if so, forces the local circuit open. It also respects TTL by checking
// the updated timestamp.
func (s *RedisSyncer) applyRemote(b *balancer.Backend, fields map[string]string) {
	now := time.Now().Unix()
	ttlSec := int64(s.ttl.Seconds())

	for field, val := range fields {
		// Fields have the form "<replicaID>:state", "<replicaID>:failures",
		// "<replicaID>:updated". We only care about :state fields that belong
		// to other replicas.
		replicaID, suffix := splitLastColon(field)
		if suffix != "state" {
			continue
		}
		if replicaID == s.replicaID {
			// Skip our own entry.
			continue
		}

		// Check that the entry is not stale.
		updatedKey := replicaID + ":updated"
		if updStr, ok := fields[updatedKey]; ok {
			updTS, err := strconv.ParseInt(updStr, 10, 64)
			if err == nil && now-updTS > ttlSec {
				// Stale — ignore.
				continue
			}
		}

		if val == "open" {
			// Check whether this remote entry predates the local circuit's last
			// recovery. If the local circuit already closed (HalfOpen → Closed)
			// AFTER the remote entry was written, that entry represents a state
			// the local circuit has already moved past, so ignore it. This
			// prevents a stale remote open from blocking local recovery (Bug 3).
			if updStr, ok := fields[replicaID+":updated"]; ok {
				updTS, err := strconv.ParseInt(updStr, 10, 64)
				if err == nil {
					closedAt := s.cb.GetClosedAt(b)
					if closedAt > 0 && closedAt > updTS {
						// Local circuit recovered after this remote entry was written;
						// the remote entry is from before our recovery — skip it.
						continue
					}
				}
			}
			s.cb.forceOpen(b)
			return // one open replica is enough
		}
	}
}

// forceOpen opens the local circuit for a backend without the normal
// failure-counting logic. Used when a remote replica reports OPEN.
func (c *CircuitBreaker) forceOpen(b *balancer.Backend) {
	c.mu.Lock()
	state := c.stateFor(b)
	if state.state == StateOpen {
		c.mu.Unlock()
		return
	}
	var pending [2]State
	c.transition(state, StateOpen, &pending)
	state.lastFailure = c.now()
	b.SetHealthy(false)
	hook := c.onStateChange
	c.mu.Unlock()

	if hook != nil {
		hook(b, pending[0], pending[1])
	}
}

// backendKey returns a short, stable hex string that identifies a backend URL.
func backendKey(url string) string {
	h := sha256.Sum256([]byte(url))
	return fmt.Sprintf("%x", h[:8])
}

// defaultReplicaID returns a string that uniquely identifies this process.
func defaultReplicaID() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}

// hgetallToMap converts the flat []interface{} returned by a Redis HGETALL (or
// EVAL returning HGETALL) into map[string]string.
func hgetallToMap(raw interface{}) map[string]string {
	m := make(map[string]string)
	slice, ok := raw.([]interface{})
	if !ok {
		return m
	}
	for i := 0; i+1 < len(slice); i += 2 {
		k, _ := slice[i].(string)
		v, _ := slice[i+1].(string)
		if k != "" {
			m[k] = v
		}
	}
	return m
}

// splitLastColon splits a string at the last ':' and returns (prefix, suffix).
// If there is no ':', it returns ("", s).
func splitLastColon(s string) (prefix, suffix string) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return s[:i], s[i+1:]
		}
	}
	return "", s
}
