package ratelimiter

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/redis/go-redis/v9"
)

func TestAllowConcurrent(t *testing.T) {
	ctx := context.Background()

	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { container.Terminate(ctx) })

	url, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { rdb.Close() })

	const (
		rate        = 1.0
		capacity    = 100
		workers     = 50
		perWorker   = 1000
		refillSlack = 5
	)

	l := NewRedisLimiter(rdb, rate, capacity, false)

	var allowed int64
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				ok, err := l.Allow(ctx, "test_user")
				if err != nil {
					t.Errorf("unexpected redis error: %v", err)
					return
				}
				if ok {
					atomic.AddInt64(&allowed, 1)
				}
			}
		}()
	}
	wg.Wait()

	if allowed < capacity || allowed > capacity+refillSlack {
		t.Errorf("allowed = %d, expected [%d, %d]", allowed, capacity, capacity+refillSlack)
	}
}
