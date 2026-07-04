package ratelimiter

import (
	"context"
	"fmt"

	_ "embed"

	"github.com/redis/go-redis/v9"
)

//go:embed token_bucket.lua
var tokenBucketSrc string

var tokenBucketScript = redis.NewScript(tokenBucketSrc)

type RedisLimiter struct {
	rdb      redis.Scripter
	rate     float64 // tokens per second
	capacity float64 // burst
	failOpen bool    // behavior when Redis is unavailable
}


func NewRedisLimiter(rdb redis.Scripter, rate float64, capacity float64, failOpen bool) *RedisLimiter {
	return &RedisLimiter{
		rdb:      rdb,
		rate:     rate,
		capacity: capacity,
		failOpen: failOpen,
	}
}

func (l *RedisLimiter) Allow(ctx context.Context, key string) (bool, error) {
	return l.AllowN(ctx, key, 1)
}

func (l *RedisLimiter) AllowN(ctx context.Context, key string, cost int) (bool, error) {
	if cost <= 0 {
		return false, fmt.Errorf("ratelimiter: cost must be greater than 0, got %d", cost)
	}

	res, err := tokenBucketScript.Run(ctx, l.rdb, []string{key}, l.rate, l.capacity, cost).Result()
	if err != nil {
		return l.failOpen, err
	}

	arr, ok := res.([]interface{})
	if !ok || len(arr) < 1 {
		return false, fmt.Errorf("ratelimiter: unexpected script result %T: %v", res, res)
	}

	var allowed int64
	switch v := arr[0].(type) {
	case int64:
		allowed = v
	case bool:
		if v {
			allowed = 1
		}
	default:
		return false, fmt.Errorf("ratelimiter: unexpected type for allowed field: %T", arr[0])
	}

	return allowed == 1, nil
}
