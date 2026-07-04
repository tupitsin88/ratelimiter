package ratelimiter

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"
)

type reserver interface {
	AllowN(ctx context.Context, key string, cost int) (bool, error)
}

const numShards = 256

type lease struct {
	remaining int
}

type shard struct {
	mu     sync.Mutex
	leases map[string]lease
}

type TieredLimiter struct {
	reserver  reserver
	batchSize int
	shards    [numShards]shard
}

func NewTiered(r reserver, batchSize int) (*TieredLimiter, error) {
	if batchSize < 1 {
		return nil, fmt.Errorf("ratelimiter: batchSize must be >= 1, got %d", batchSize)
	}
	t := &TieredLimiter{
		reserver:  r,
		batchSize: batchSize,
	}
	for i := range t.shards {
		t.shards[i].leases = make(map[string]lease)
	}
	return t, nil
}

func (t *TieredLimiter) shardFor(key string) *shard {
	h := fnv.New32a()
	h.Write([]byte(key))
	return &t.shards[h.Sum32()&(numShards-1)]
}

func (t *TieredLimiter) Allow(ctx context.Context, key string) (bool, error) {
	s := t.shardFor(key)

	s.mu.Lock()
	if st := s.leases[key]; st.remaining > 0 {
		st.remaining--
		s.leases[key] = st
		s.mu.Unlock()
		return true, nil
	}
	s.mu.Unlock()

	granted, err := t.reserver.AllowN(ctx, key, t.batchSize)
	if err != nil {
		return granted, fmt.Errorf("ratelimiter: reserving batch for %q: %w", key, err)
	}
	if !granted {
		return false, nil
	}

	s.mu.Lock()
	st := s.leases[key]
	st.remaining += t.batchSize - 1
	s.leases[key] = st
	s.mu.Unlock()

	return true, nil
}
