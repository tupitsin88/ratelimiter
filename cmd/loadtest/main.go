package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/pprof"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/tupitsin88/ratelimiter"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var (
		addr      = flag.String("addr", ":8080", "measured server address (handler only)")
		admin     = flag.String("admin", ":6060", "admin address (/metrics + /debug/pprof)")
		mode      = flag.String("mode", "off", "limiter mode: off|redis|tiered")
		redisAddr = flag.String("redis", "localhost:6379", "redis address")
		rate      = flag.Float64("rate", 1e6, "tokens per second")
		capacity  = flag.Float64("capacity", 1e6, "bucket capacity (burst)")
		batch     = flag.Int("batch", 100, "tiered lease batch size")
	)
	flag.Parse()

	if *rate <= 0 || *capacity <= 0 {
		return fmt.Errorf("rate and capacity must be > 0")
	}

	handler, reg, err := buildHandler(*mode, *redisAddr, *rate, *capacity, *batch)
	if err != nil {
		return err
	}

	measured := &http.Server{Addr: *addr, Handler: handler}
	adminSrv := &http.Server{Addr: *admin, Handler: adminHandler(reg)}

	errCh := make(chan error, 2)
	go serve(measured, errCh)
	go serve(adminSrv, errCh)
	log.Printf("loadtest: mode=%s measured=%s admin=%s", *mode, *addr, *admin)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = measured.Shutdown(shutdownCtx)
	_ = adminSrv.Shutdown(shutdownCtx)
	return nil
}

func serve(s *http.Server, errCh chan<- error) {
	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errCh <- fmt.Errorf("server %s: %w", s.Addr, err)
	}
}

func buildHandler(mode, redisAddr string, rate, capacity float64, batch int) (http.Handler, *prometheus.Registry, error) {
	reg := prometheus.NewRegistry()
	raw := rawHandler()
	if mode == "off" {
		return raw, reg, nil
	}

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		return nil, nil, fmt.Errorf("redis ping %s: %w", redisAddr, err)
	}

	var limiter ratelimiter.Limiter
	switch mode {
	case "redis":
		limiter = ratelimiter.NewRedisLimiter(rdb, rate, capacity, false)
	case "tiered":
		tl, err := ratelimiter.NewTiered(ratelimiter.NewRedisLimiter(rdb, rate, capacity, false), batch)
		if err != nil {
			return nil, nil, fmt.Errorf("new tiered limiter: %w", err)
		}
		limiter = tl
	default:
		return nil, nil, fmt.Errorf("invalid mode %q (want off|redis|tiered)", mode)
	}

	mw, err := ratelimiter.NewMiddleware(limiter, apiKey)
	if err != nil {
		return nil, nil, fmt.Errorf("new middleware: %w", err)
	}
	for _, c := range mw.Collectors() {
		if err := reg.Register(c); err != nil {
			return nil, nil, fmt.Errorf("register metrics: %w", err)
		}
	}
	return mw.Wrap(raw), reg, nil
}

func rawHandler() http.Handler {
	body := []byte("ok")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	})
}

func apiKey(r *http.Request) string {
	return r.Header.Get("X-API-Key")
}

func adminHandler(reg *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}
