package circuit

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// goRedisClientAdapter wraps a *redis.Client from go-redis/v9 to satisfy the
// redisClient interface used by RedisSyncer. The go-redis methods return
// concrete command types (*redis.Cmd, *redis.MapStringStringCmd, etc.) whose
// return types differ from our interface's result wrapper types, so this thin
// adapter bridges the two without exposing go-redis types at the interface
// boundary (which keeps tests injectable without miniredis).
type goRedisClientAdapter struct {
	c *redis.Client
}

// NewGoRedisAdapter wraps a *redis.Client so it satisfies redisClient.
// Use this in production to construct a RedisSyncer from a go-redis client.
func NewGoRedisAdapter(c *redis.Client) redisClient {
	return &goRedisClientAdapter{c: c}
}

func (a *goRedisClientAdapter) Eval(ctx context.Context, script string, keys []string, args ...interface{}) redisResult {
	return a.c.Eval(ctx, script, keys, args...)
}

func (a *goRedisClientAdapter) HGetAll(ctx context.Context, key string) redisStringMapResult {
	return a.c.HGetAll(ctx, key)
}

func (a *goRedisClientAdapter) Keys(ctx context.Context, pattern string) redisStringSliceResult {
	return a.c.Keys(ctx, pattern)
}

func (a *goRedisClientAdapter) Close() error {
	return a.c.Close()
}
