package ratelimiter

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/redis/go-redis/v9"
)

// countingReserver is a fake Redis tier that always grants and records how many
// times AllowN was called, so a test can prove batching collapses round-trips.
type countingReserver struct {
	calls int
}

func (c *countingReserver) AllowN(ctx context.Context, key string, cost int) (bool, error) {
	c.calls++
	return true, nil
}

func TestNewTieredRejectsBadBatchSize(t *testing.T) {
	for _, bs := range []int{0, -1} {
		if _, err := NewTiered(&countingReserver{}, bs); err == nil {
			t.Errorf("NewTiered(batchSize=%d): expected error, got nil", bs)
		}
	}
	if _, err := NewTiered(&countingReserver{}, 1); err != nil {
		t.Errorf("NewTiered(batchSize=1): unexpected error: %v", err)
	}
}

// Test B — batching cuts round-trips. With a fake that always grants, N sequential
// single-permit Allow calls on one key must reserve exactly ceil(N/batchSize)
// batches, not N. Sequential keeps the count deterministic.
func TestTieredBatchingReducesRoundTrips(t *testing.T) {
	const (
		n         = 100
		batchSize = 10
	)

	fake := &countingReserver{}
	l, err := NewTiered(fake, batchSize)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	for i := 0; i < n; i++ {
		ok, err := l.Allow(ctx, "user")
		if err != nil {
			t.Fatalf("Allow #%d: %v", i, err)
		}
		if !ok {
			t.Fatalf("Allow #%d returned false, fake always grants", i)
		}
	}

	want := (n + batchSize - 1) / batchSize // ceil(n/batchSize)
	if fake.calls != want {
		t.Errorf("AllowN calls = %d, want %d (ceil(%d/%d))", fake.calls, want, n, batchSize)
	}
}

// Test A — global ceiling holds with leasing. Flood one key from many goroutines
// through the tiered layer over a real Redis bucket and assert the admitted count
// stays at the bucket ceiling, not the number of attempts: leasing must not admit
// beyond what the global bucket holds.
func TestTieredGlobalCeiling(t *testing.T) {
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
		batchSize   = 10
		workers     = 50
		perWorker   = 100
		refillSlack = 5
	)

	l, err := NewTiered(NewRedisLimiter(rdb, rate, capacity, false), batchSize)
	if err != nil {
		t.Fatal(err)
	}

	var allowed int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				ok, err := l.Allow(ctx, "test_user")
				if err != nil {
					t.Errorf("unexpected error: %v", err)
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
		t.Errorf("allowed = %d, expected [%d, %d]: leasing must not admit beyond the global bucket",
			allowed, capacity, capacity+refillSlack)
	}
}
