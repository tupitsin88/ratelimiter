package ratelimiter

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type Limiter interface {
	Allow(ctx context.Context, key string) (bool, error)
}

type KeyFunc func(*http.Request) string

type Option func(*Middleware)

func WithRejectStatus(code int) Option {
	return func(m *Middleware) { m.rejectStatus = code }
}

type Middleware struct {
	limiter      Limiter
	key          KeyFunc
	rejectStatus int
	metrics      *metrics
}

func NewMiddleware(l Limiter, key KeyFunc, opts ...Option) (*Middleware, error) {
	if l == nil {
		return nil, errors.New("ratelimiter: limiter must not be nil")
	}
	if key == nil {
		return nil, errors.New("ratelimiter: key func must not be nil")
	}
	m := &Middleware{
		limiter:      l,
		key:          key,
		rejectStatus: http.StatusTooManyRequests,
		metrics:      newMetrics(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

func (m *Middleware) Collectors() []prometheus.Collector {
	return m.metrics.collectors()
}

func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := m.key(r)
		if key == "" {
			m.metrics.requests.WithLabelValues(decisionUnlimited).Inc()
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		allowed, err := m.limiter.Allow(r.Context(), key)
		m.metrics.duration.Observe(time.Since(start).Seconds())
		if err != nil {
			m.metrics.errors.Inc()
		}

		if allowed {
			m.metrics.requests.WithLabelValues(decisionAllowed).Inc()
			next.ServeHTTP(w, r)
			return
		}
		m.metrics.requests.WithLabelValues(decisionDenied).Inc()
		http.Error(w, http.StatusText(m.rejectStatus), m.rejectStatus)
	})
}
