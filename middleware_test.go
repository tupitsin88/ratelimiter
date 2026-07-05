package ratelimiter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	dto "github.com/prometheus/client_model/go"
)

type fakeLimiter struct {
	allow bool
	err   error
}

func (f fakeLimiter) Allow(ctx context.Context, key string) (bool, error) {
	return f.allow, f.err
}

type recordingLimiter struct{ called bool }

func (r *recordingLimiter) Allow(ctx context.Context, key string) (bool, error) {
	r.called = true
	return true, nil
}

func constKey(v string) KeyFunc {
	return func(*http.Request) string { return v }
}

func markRan(ran *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*ran = true
		w.WriteHeader(http.StatusOK)
	})
}

func serve(h http.Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec
}

func TestNewMiddlewareValidation(t *testing.T) {
	key := func(*http.Request) string { return "k" }
	if _, err := NewMiddleware(nil, key); err == nil {
		t.Error("nil limiter: expected error, got nil")
	}
	if _, err := NewMiddleware(fakeLimiter{allow: true}, nil); err == nil {
		t.Error("nil key func: expected error, got nil")
	}
}

// Test A — decisions map to status codes, next execution, and requests_total.
func TestMiddlewareDecisionsAndStatus(t *testing.T) {
	t.Run("allowed", func(t *testing.T) {
		var ran bool
		mw, err := NewMiddleware(fakeLimiter{allow: true}, constKey("k"))
		if err != nil {
			t.Fatal(err)
		}
		rec := serve(mw.Wrap(markRan(&ran)))

		if !ran {
			t.Error("next handler should have run")
		}
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := testutil.ToFloat64(mw.metrics.requests.WithLabelValues(decisionAllowed)); got != 1 {
			t.Errorf("requests_total{allowed} = %v, want 1", got)
		}
	})

	t.Run("denied", func(t *testing.T) {
		var ran bool
		mw, err := NewMiddleware(fakeLimiter{allow: false}, constKey("k"))
		if err != nil {
			t.Fatal(err)
		}
		rec := serve(mw.Wrap(markRan(&ran)))

		if ran {
			t.Error("next handler should NOT have run")
		}
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
		}
		if got := testutil.ToFloat64(mw.metrics.requests.WithLabelValues(decisionDenied)); got != 1 {
			t.Errorf("requests_total{denied} = %v, want 1", got)
		}
	})
}

// Test B — on a limiter error the middleware honors the returned bool and counts
// the error, never imposing its own open/closed policy.
func TestMiddlewareErrorHonorsBool(t *testing.T) {
	sentinel := errors.New("redis down")

	t.Run("fail-open", func(t *testing.T) {
		var ran bool
		mw, err := NewMiddleware(fakeLimiter{allow: true, err: sentinel}, constKey("k"))
		if err != nil {
			t.Fatal(err)
		}
		rec := serve(mw.Wrap(markRan(&ran)))

		if !ran {
			t.Error("fail-open: next should have run")
		}
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := testutil.ToFloat64(mw.metrics.errors); got != 1 {
			t.Errorf("errors_total = %v, want 1", got)
		}
	})

	t.Run("fail-closed", func(t *testing.T) {
		var ran bool
		mw, err := NewMiddleware(fakeLimiter{allow: false, err: sentinel}, constKey("k"))
		if err != nil {
			t.Fatal(err)
		}
		rec := serve(mw.Wrap(markRan(&ran)))

		if ran {
			t.Error("fail-closed: next should NOT have run")
		}
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
		}
		if got := testutil.ToFloat64(mw.metrics.errors); got != 1 {
			t.Errorf("errors_total = %v, want 1", got)
		}
	})
}

// Test C — an empty key bypasses the limiter entirely and counts as unlimited.
func TestMiddlewareEmptyKeyBypass(t *testing.T) {
	lim := &recordingLimiter{}
	var ran bool
	mw, err := NewMiddleware(lim, func(*http.Request) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	rec := serve(mw.Wrap(markRan(&ran)))

	if lim.called {
		t.Error("limiter must not be called for an empty key")
	}
	if !ran {
		t.Error("next should have run for the unlimited path")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := testutil.ToFloat64(mw.metrics.requests.WithLabelValues(decisionUnlimited)); got != 1 {
		t.Errorf("requests_total{unlimited} = %v, want 1", got)
	}
}

// Test D — collectors register on a fresh registry and gather with the expected
// label values.
func TestMiddlewareMetricsRegisterAndGather(t *testing.T) {
	mw, err := NewMiddleware(fakeLimiter{allow: true}, constKey("k"))
	if err != nil {
		t.Fatal(err)
	}

	reg := prometheus.NewRegistry()
	for _, c := range mw.Collectors() {
		if err := reg.Register(c); err != nil {
			t.Fatalf("register collector: %v", err)
		}
	}

	serve(mw.Wrap(markRan(new(bool))))

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	names := familyNames(mfs)
	for _, want := range []string{
		"ratelimiter_middleware_requests_total",
		"ratelimiter_middleware_errors_total",
		"ratelimiter_middleware_decision_duration_seconds",
	} {
		if !names[want] {
			t.Errorf("gathered families missing %q; got %v", want, names)
		}
	}

	labels := decisionValues(mfs, "ratelimiter_middleware_requests_total")
	if !labels[decisionAllowed] {
		t.Errorf("requests_total missing decision=%q; got %v", decisionAllowed, labels)
	}
}

// Test E — concurrent traffic is race-free and every request is accounted for.
func TestMiddlewareConcurrent(t *testing.T) {
	const goroutines = 200

	var ranCount int64
	mw, err := NewMiddleware(fakeLimiter{allow: true}, constKey("k"))
	if err != nil {
		t.Fatal(err)
	}
	h := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&ranCount, 1)
	}))

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			serve(h)
		}()
	}
	wg.Wait()

	if ranCount != goroutines {
		t.Errorf("next ran %d times, want %d", ranCount, goroutines)
	}
	if got := testutil.ToFloat64(mw.metrics.requests.WithLabelValues(decisionAllowed)); got != goroutines {
		t.Errorf("requests_total{allowed} = %v, want %d", got, goroutines)
	}
}

func familyNames(mfs []*dto.MetricFamily) map[string]bool {
	out := make(map[string]bool, len(mfs))
	for _, mf := range mfs {
		out[mf.GetName()] = true
	}
	return out
}

func decisionValues(mfs []*dto.MetricFamily, name string) map[string]bool {
	out := map[string]bool{}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "decision" {
					out[lp.GetValue()] = true
				}
			}
		}
	}
	return out
}
