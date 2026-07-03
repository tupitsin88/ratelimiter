package ratelimiter

import (
	"sync"
	"time"
)

type Limiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	counters map[string]*counter
}

type counter struct {
	count       int
	windowStart time.Time
}

func New(limit int, window time.Duration) *Limiter {
	return &Limiter{
		limit:    limit,
		window:   window,
		counters: make(map[string]*counter),
	}
}

func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	c, ok := l.counters[key]
	if !ok || now.Sub(c.windowStart) > l.window {
		l.counters[key] = &counter{
			count:       1,
			windowStart: now,
		}
		return true
	}

	if c.count < l.limit {
		c.count++
		return true
	}
	return false
}
