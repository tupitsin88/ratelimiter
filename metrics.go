package ratelimiter

import "github.com/prometheus/client_golang/prometheus"

const (
	metricsNamespace = "ratelimiter"
	metricsSubsystem = "middleware"

	decisionAllowed   = "allowed"
	decisionDenied    = "denied"
	decisionUnlimited = "unlimited"
)

type metrics struct {
	requests *prometheus.CounterVec
	errors   prometheus.Counter
	duration prometheus.Histogram
}

// DefBuckets start at 5ms and cannot resolve sub-millisecond decisions.
var decisionBuckets = []float64{0.00005, 0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25}

func newMetrics() *metrics {
	return &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "requests_total",
			Help:      "Rate-limiter decisions by outcome.",
		}, []string{"decision"}),
		errors: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "errors_total",
			Help:      "Limiter calls that returned a non-nil error.",
		}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "decision_duration_seconds",
			Help:      "Time spent in the limiter Allow call.",
			Buckets:   decisionBuckets,
		}),
	}
}

func (m *metrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{m.requests, m.errors, m.duration}
}
