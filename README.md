# ratelimiter

Embeddable Go rate-limiting library: a Redis-authoritative token bucket with an
optional local lease layer that cuts Redis round-trips, plus net/http middleware
with Prometheus metrics.

## Requirements

- Go 1.25+
- A running Redis, accessed via [go-redis v9](https://github.com/redis/go-redis)

## Install

```sh
go get github.com/tupitsin88/ratelimiter
```

## Quick start: RedisLimiter

Every decision is made atomically in Redis by a Lua script; `*redis.Client`
satisfies the `redis.Scripter` parameter.

```go
package main

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
	"github.com/tupitsin88/ratelimiter"
)

func main() {
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})

	// rate 100 tokens/s, capacity (burst) 200, failOpen=false (fail-closed)
	limiter := ratelimiter.NewRedisLimiter(rdb, 100, 200, false)

	ok, err := limiter.Allow(context.Background(), "user:42")
	if err != nil {
		// Redis error: ok already reflects the failOpen policy (see Degradation).
	}
	fmt.Println(ok)
}
```

`AllowN(ctx, key, cost)` takes `cost` tokens at once, all-or-nothing: it grants
`cost` tokens or none. `cost` must be > 0.

```go
ok, err := limiter.AllowN(context.Background(), "user:42", 5)
```

## TieredLimiter

`TieredLimiter` wraps a `*RedisLimiter` (anything with its `AllowN` method).
On a local miss it leases `batchSize` permits from Redis in one `AllowN` call
and serves subsequent requests for that key locally, with no network. This
turns one Redis round-trip per request into one per batch. `batchSize == 1`
degrades to a Redis call per request.

```go
tiered, err := ratelimiter.NewTiered(limiter, 100)
if err != nil {
	// batchSize < 1
}

ok, err := tiered.Allow(context.Background(), "user:42")
```

Local state is sharded across 256 mutex-guarded maps keyed by an FNV-1a hash,
so distinct keys do not contend on one lock. The shard lock is never held
across the Redis call.

## Middleware

`Middleware` gates an `http.Handler` with any `Limiter`
(`Allow(ctx context.Context, key string) (bool, error)` — both `*RedisLimiter`
and `*TieredLimiter` satisfy it). The caller supplies a
`KeyFunc func(*http.Request) string`; if it returns `""` the request bypasses
the limiter and is counted as `unlimited`. Rejected requests get
`429 Too Many Requests` (override with `WithRejectStatus(code)`).

```go
mw, err := ratelimiter.NewMiddleware(tiered, func(r *http.Request) string {
	return r.Header.Get("X-API-Key")
})
if err != nil {
	// nil limiter or nil key func
}

reg := prometheus.NewRegistry()
for _, c := range mw.Collectors() {
	if err := reg.Register(c); err != nil {
		// duplicate registration
	}
}

http.ListenAndServe(":8080", mw.Wrap(handler))
```

Metrics (registered on your own `*prometheus.Registry` via `Collectors()`):

| metric | type | notes |
|---|---|---|
| `ratelimiter_middleware_requests_total{decision}` | counter | `allowed` \| `denied` \| `unlimited` |
| `ratelimiter_middleware_errors_total` | counter | limiter returned a non-nil error |
| `ratelimiter_middleware_decision_duration_seconds` | histogram | time in `Allow`, buckets 50µs–250ms |

Keys and URL paths are deliberately kept off metric labels (unbounded
cardinality).

## Config

- `rate` (float64) — tokens added per second, per key.
- `capacity` (float64) — bucket size; the maximum burst a key can spend at once.
- `failOpen` (bool) — what a `RedisLimiter` answers when Redis is unreachable.
- `batchSize` (int, ≥ 1) — permits leased from Redis per `TieredLimiter` refill;
  higher = fewer round-trips, more permits stranded locally.

## Degradation

When the Redis call fails, `AllowN` returns `(failOpen, err)`: with
`failOpen=true` the request is allowed (fail-open), with `failOpen=false` it is
denied (fail-closed) — and the error is returned alongside either way.
`TieredLimiter` passes that bool through unchanged and does not touch its local
state. The middleware honors the bool, increments
`ratelimiter_middleware_errors_total`, and never applies a policy of its own.

## How it works

The bucket lives in a Redis hash (`tokens`, `ts`) and is updated by a Lua
script executed atomically inside Redis. The script reads the clock from Redis
`TIME` (microseconds) — the single clock source, so client clock skew cannot
distort refill. Refill is lazy: on each call tokens grow by
`elapsed * rate`, capped at `capacity`; if the balance covers `cost` it is
deducted, otherwise nothing changes. The key expires after the bucket would
have refilled anyway (minimum 60s).

`TieredLimiter` keeps a per-key count of locally leased permits. A request that
finds a permit decrements it and never touches the network; a request that
finds none calls `AllowN(ctx, key, batchSize)` on the Redis tier and, if
granted, banks the remaining `batchSize-1` permits locally. Concurrent refills
for one key accumulate rather than overwrite, so no granted batch is lost.

## Known limitations

- The local lease map never evicts idle keys: memory grows with the number of
  distinct keys seen.
- Leased local permits have no TTL: permits Redis already deducted stay
  reserved by an idle process indefinitely.
- `TieredLimiter` is single-permit only (`Allow`); there is no `cost > 1` on
  the tiered layer.
- Batching is all-or-nothing: when the global bucket holds fewer than
  `batchSize` tokens, a tiered refill is denied even though some tokens remain.
- Two goroutines refilling the same key concurrently may each lease a batch;
  the global ceiling still holds, but the extra permits sit stranded locally.
