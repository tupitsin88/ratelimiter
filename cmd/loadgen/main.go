package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	vegeta "github.com/tsenart/vegeta/v12/lib"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var (
		addr     = flag.String("addr", "localhost:8080", "target server address")
		rate     = flag.Int("rate", 10000, "requests per second")
		duration = flag.Duration("duration", 30*time.Second, "measured attack duration")
		warmup   = flag.Duration("warmup", 10*time.Second, "warmup duration (results discarded)")
		keys     = flag.Int("keys", 1000, "distinct X-API-Key values, round-robined")
		out      = flag.String("out", "results/run", "output path prefix (.txt and .json)")
	)
	flag.Parse()

	if *rate <= 0 || *keys <= 0 || *duration <= 0 {
		return fmt.Errorf("rate, keys and duration must be > 0")
	}

	targetURL := fmt.Sprintf("http://%s/", *addr)
	pacer := vegeta.Rate{Freq: *rate, Per: time.Second}

	if *warmup > 0 {
		warm := vegeta.NewAttacker()
		for range warm.Attack(newTargeter(targetURL, *keys), pacer, *warmup, "warmup") {
		}
		warm.Stop()
	}

	attacker := vegeta.NewAttacker()
	var metrics vegeta.Metrics
	for res := range attacker.Attack(newTargeter(targetURL, *keys), pacer, *duration, "measured") {
		metrics.Add(res)
	}
	metrics.Close()

	if err := writeReports(*out, &metrics); err != nil {
		return err
	}
	printSummary(*rate, &metrics)
	return nil
}

func newTargeter(url string, keys int) vegeta.Targeter {
	var counter uint64
	return func(t *vegeta.Target) error {
		if t == nil {
			return vegeta.ErrNilTarget
		}
		t.Method = http.MethodGet
		t.URL = url
		n := atomic.AddUint64(&counter, 1)
		t.Header = http.Header{"X-API-Key": []string{fmt.Sprintf("key-%d", n%uint64(keys))}}
		return nil
	}
}

func writeReports(out string, m *vegeta.Metrics) error {
	if dir := filepath.Dir(out); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create out dir: %w", err)
		}
	}
	if err := writeReport(out+".txt", vegeta.NewTextReporter(m)); err != nil {
		return err
	}
	return writeReport(out+".json", vegeta.NewJSONReporter(m))
}

func writeReport(path string, rep vegeta.Reporter) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if err := rep(f); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func printSummary(target int, m *vegeta.Metrics) {
	fmt.Printf("target-rate   %d req/s\n", target)
	fmt.Printf("achieved-rate %.0f req/s\n", m.Rate)
	fmt.Printf("throughput    %.0f req/s\n", m.Throughput)
	fmt.Printf("requests      %d\n", m.Requests)
	fmt.Printf("success       %.2f%%\n", m.Success*100)
	fmt.Printf("p50           %s\n", m.Latencies.P50)
	fmt.Printf("p95           %s\n", m.Latencies.P95)
	fmt.Printf("p99           %s\n", m.Latencies.P99)
	fmt.Printf("max           %s\n", m.Latencies.Max)
	if len(m.Errors) > 0 {
		fmt.Printf("errors        %v\n", m.Errors)
	}
}
