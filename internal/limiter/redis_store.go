package limiter

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// gcraLuaScript is a server-side GCRA implementation executed atomically via
// EVALSHA/EVAL. It reads and writes the Theoretical Arrival Time (TAT) stored
// at the namespaced key and returns whether the event is allowed and, when not,
// how many microseconds the caller should wait.
//
// KEYS[1]      – the rate-limit key (already namespaced by the caller)
// ARGV[1]      – burst (integer, >= 1)
// ARGV[2]      – period_us: microseconds between emissions = 1e6 / rps
// ARGV[3]      – now_us: current Unix time in microseconds
//
// Returns: {allowed, retry_after_us}
//
//	allowed       = 1 (admitted) or 0 (rejected)
//	retry_after_us = 0 when allowed; microseconds until a slot opens otherwise
const gcraLuaScript = `
local key        = KEYS[1]
local burst      = tonumber(ARGV[1])
local period_us  = tonumber(ARGV[2])
local now_us     = tonumber(ARGV[3])

local tat_str = redis.call("GET", key)
local tat_us
if tat_str then
    tat_us = tonumber(tat_str)
else
    tat_us = now_us
end

if tat_us < now_us then
    tat_us = now_us
end

local new_tat   = tat_us + period_us
local allow_at  = new_tat - burst * period_us
local allowed

if now_us < allow_at then
    -- rejected: report how long until a slot opens
    allowed = 0
    local retry_us = allow_at - now_us
    return {allowed, retry_us}
end

-- admitted: store the new TAT with a TTL so stale keys are GC'd
local ttl_us = (burst + 1) * period_us
local ttl_sec = math.ceil(ttl_us / 1000000)
if ttl_sec < 1 then ttl_sec = 1 end
redis.call("SET", key, new_tat, "EX", ttl_sec)
allowed = 1
return {allowed, 0}
`

// RedisStore is a distributed rate-limit Store backed by Redis. It implements
// Store using a server-side GCRA Lua script executed atomically so multiple
// proxy instances sharing the same Redis cluster enforce a combined limit.
type RedisStore struct {
	client    redis.UniversalClient
	prefix    string
	scriptSHA string // cached EVALSHA digest; empty until first call
}

// NewRedisStore creates a RedisStore connecting to the given Redis addr. keyPrefix
// namespaces every rate-limit key stored in Redis; the default is "rplb:rl".
func NewRedisStore(addr, password string, db int, keyPrefix string) (*RedisStore, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
	if keyPrefix == "" {
		keyPrefix = "rplb:rl"
	}
	return &RedisStore{client: client, prefix: keyPrefix}, nil
}

// NewRedisStoreFromClient creates a RedisStore from an existing redis.UniversalClient.
// keyPrefix namespaces every rate-limit key; the default is "rplb:rl".
func NewRedisStoreFromClient(client redis.UniversalClient, keyPrefix string) *RedisStore {
	if keyPrefix == "" {
		keyPrefix = "rplb:rl"
	}
	return &RedisStore{client: client, prefix: keyPrefix}
}

// Allow implements Store. It consults the server-side GCRA script to atomically
// determine whether the event for key is admitted under the rps/burst budget.
func (r *RedisStore) Allow(key string, rps float64, burst int, now time.Time) (bool, time.Duration) {
	if rps <= 0 {
		return true, 0
	}
	if burst < 1 {
		burst = 1
	}

	periodUS := int64(1e6 / rps) // microseconds between emissions
	nowUS := now.UnixMicro()
	fullKey := r.prefix + ":" + key

	ctx := context.Background()

	// Try EVALSHA first; on NOSCRIPT load and execute.
	result, err := r.evalScript(ctx, fullKey, burst, periodUS, nowUS)
	if err != nil {
		// On any Redis error fall back to allowing so a Redis outage does not
		// bring down the proxy entirely.
		return true, 0
	}

	vals, ok := result.([]interface{})
	if !ok || len(vals) < 2 {
		return true, 0
	}

	allowed := toInt64(vals[0])
	retryUS := toInt64(vals[1])

	if allowed == 1 {
		return true, 0
	}
	return false, time.Duration(retryUS) * time.Microsecond
}

// evalScript runs the GCRA Lua script via EVALSHA if possible, falling back to
// EVAL on NOSCRIPT and caching the resulting SHA for subsequent calls.
func (r *RedisStore) evalScript(ctx context.Context, key string, burst int, periodUS, nowUS int64) (interface{}, error) {
	if r.scriptSHA == "" {
		// First call: load the script and cache its SHA.
		sha, err := r.client.ScriptLoad(ctx, gcraLuaScript).Result()
		if err != nil {
			// Cannot load script — fall back to raw EVAL for this call.
			return r.client.Eval(ctx, gcraLuaScript, []string{key},
				burst, periodUS, nowUS).Result()
		}
		r.scriptSHA = sha
	}

	result, err := r.client.EvalSha(ctx, r.scriptSHA, []string{key},
		burst, periodUS, nowUS).Result()
	if err != nil && isNOSCRIPT(err) {
		// Script was flushed from Redis; reload it.
		sha, loadErr := r.client.ScriptLoad(ctx, gcraLuaScript).Result()
		if loadErr != nil {
			return r.client.Eval(ctx, gcraLuaScript, []string{key},
				burst, periodUS, nowUS).Result()
		}
		r.scriptSHA = sha
		return r.client.EvalSha(ctx, r.scriptSHA, []string{key},
			burst, periodUS, nowUS).Result()
	}
	return result, err
}

// isNOSCRIPT reports whether a Redis error is NOSCRIPT (SHA not loaded).
func isNOSCRIPT(err error) bool {
	if err == nil {
		return false
	}
	// The go-redis library surfaces this as an error whose string starts with "NOSCRIPT".
	s := err.Error()
	return len(s) >= 8 && s[:8] == "NOSCRIPT"
}

// toInt64 converts a Redis reply value to int64, returning 0 on failure.
func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

// Close releases the underlying Redis connection.
func (r *RedisStore) Close() error {
	return r.client.Close()
}
