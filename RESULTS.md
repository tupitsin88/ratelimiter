# Load-test results — Stage 4b

End-to-end latency of the rate-limiter middleware under identical load in three
modes: **off** (no middleware, the floor), **redis** (Redis-authoritative token
bucket, one round-trip per request), **tiered** (local batch lease over the Redis
tier). Plus a CPU profile of the tiered mode proving Redis is off the per-request
path.

## Environment

| | |
|---|---|
| Machine | Apple M4, 10 cores |
| OS | macOS 26.5 (arm64) |
| Go | go1.25.0 darwin/arm64 |
| Redis | 7.4.9 (image `redis:7`, via docker-compose) |
| vegeta | v12.13.0 (library, `go run`) |

**The load generator, the server under test, and Redis all run on this one
laptop** (alongside unrelated Docker containers). These numbers are for
**relative comparison between modes only** — they are not absolute production
figures. Co-location means the generator and server compete for the same cores,
which inflates tails; the mode-to-mode *deltas* are the signal.

### Port note (this machine)

Host `6379` was already taken by an unrelated project's Redis and `8080` by
another container. To avoid touching anything else, this run used its own Redis
on host port **6380** (`REDIS_PORT=6380`, container still `6379`) and the measured
server on **:18080**. `docker-compose.yml` still defaults to `6379`; on a clean
machine the canonical commands below work unchanged (`docker compose up -d`,
`-addr :8080`, `-redis localhost:6379`).

## Protocol (exact commands)

```sh
# 1. Redis (clean machine: no REDIS_PORT override needed)
REDIS_PORT=6380 docker compose up -d --wait

go build -o loadtest ./cmd/loadtest

# 2. per mode ∈ {off, redis, tiered} — identical flags otherwise
./loadtest -mode <mode> -batch 100 -redis localhost:6380 -addr :18080 -admin :6060 &
go run ./cmd/loadgen -addr localhost:18080 -rate 10000 -duration 30s -warmup 10s -keys 1000 -out results/<mode>
curl -s http://localhost:6060/metrics > results/<mode>-metrics.txt
kill -TERM %1            # graceful shutdown (SIGTERM)

# 3. during the tiered measured window, a concurrent CPU profile
curl -o results/tiered.pprof 'http://localhost:6060/debug/pprof/profile?seconds=15'
go tool pprof -top -nodecount=15 loadtest results/tiered.pprof
```

Load per mode: 10,000 req/s for 30 s measured (300,000 requests) after a 10 s
warm-up (discarded). `X-API-Key` round-robins over `key-0..key-999`. Rate and
capacity are `1e6` so nothing is ever denied — this measures the **allow** path.

## Results

10,000 req/s was fully sustained in **all three** modes (achieved rate =
target = 10,000 req/s, 100.00% success), so the comparison is valid at this rate;
no rate reduction was needed.

| mode | p50 | p95 | p99 | max | success | achieved RPS |
|--------|---------:|---------:|----------:|------------:|--------:|-------------:|
| off    | 34.551µs | 48.491µs | 158.242µs | 24.890542ms | 100.00% | 10000 |
| redis  | 156.002µs | 374.577µs | 1.283803ms | 43.528084ms | 100.00% | 10000 |
| tiered | 35.06µs  | 49.958µs | 201.215µs | 17.607458ms | 100.00% | 10000 |

(Raw vegeta text/JSON per mode in `results/<mode>.txt` / `results/<mode>.json`.)

### Deltas (p50)

- **redis − off = +121.451µs.** This is the cost the Redis-authoritative limiter
  adds to every request: one network round-trip and Lua token-bucket execution on
  Redis, synchronously in the request path.
- **tiered − off = +0.509µs.** The tiered layer adds essentially nothing at the
  median — the local lease decision (shard lock + integer decrement) is within
  measurement noise of serving no limiter at all.
- **tiered is 4.45× faster than redis at p50** (156.002µs → 35.06µs): the lease
  layer removes ~120µs — about 99.6% of the Redis overhead — from the median
  request. At p99 the gap is 1.283803ms → 201.215µs (~6.4×).

### Where the tiered tail comes from

The internal `decision_duration_seconds` histogram (the limiter call only, not the
full HTTP round-trip; count = 400,000 = warm-up + measured) makes the batching
visible directly:

| decisions ≤ 50µs | redis mode | tiered mode |
|---|---:|---:|
| count (of 400,000) | 0 | 395,998 |
| fraction | 0.0% | 99.0% |

In **redis** mode *no* decision is under 50µs — every one waits on Redis. In
**tiered** mode 99.0% are under 50µs (served from the local lease), and the
remaining ~1% (≈4,002 requests ≈ 400,000 / batch 100) are exactly the refill
requests that take one Redis round-trip. That ~1% is the whole story of tiered's
slightly higher p99 vs off (201µs vs 158µs). `errors_total = 0` in every mode.

## pprof — tiered hot path

`go tool pprof -top -nodecount=15 loadtest results/tiered.pprof` (15.11 s window,
3270 ms samples = 21.64% CPU — the trivial handler leaves the box mostly idle):

```
      flat  flat%   sum%        cum   cum%
         0     0%     0%     1580ms 48.32%  runtime.findRunnable
    1300ms 39.76% 39.76%     1300ms 39.76%  runtime.kevent
         0     0% 39.76%     1210ms 37.00%  runtime.netpoll
         0     0% 39.76%      940ms 28.75%  net/http.(*conn).serve
     900ms 27.52% 67.28%      900ms 27.52%  syscall.syscall
     740ms 22.63% 89.91%      740ms 22.63%  runtime.pthread_cond_signal
```

The profile is **entirely** Go scheduler (`findRunnable`/`schedule`), the kqueue
netpoller (`runtime.kevent`), and the **HTTP server's own** socket I/O to the
vegeta client (`net/http.(*conn).serve`, `net.(*conn).Read`/`Write`,
`syscall.syscall`).

**No `go-redis`, `EVALSHA`, `RedisLimiter.AllowN`, `TieredLimiter`, or
`prometheus` frame appears anywhere in the profile** (grep over all 46 nodes:
none). Two independent facts follow:

1. Redis / network-to-Redis frames are **not** on the per-request path. The only
   network in the profile is the inbound HTTP being served. Redis is touched only
   on batch refill — ~4,000 calls across the whole run — too few to register above
   pprof's sampling floor.
2. The tiered allow path itself (FNV shard hash + mutex + map decrement) is so
   cheap it does not surface as its own node either. It costs ~0.5µs at the median
   (see the delta above), consistent with being invisible to a 100 Hz CPU profile.

This is the property the tiered layer exists for, shown directly: **at steady
state the request path never talks to Redis.**

## Anything surprising (flagged, not hidden)

- **`max` is noisy.** off 24.9ms, tiered 17.6ms, redis 43.5ms — single worst
  samples, orders of magnitude above the respective p99s. On a shared laptop with
  the generator, server, Redis, and other containers all resident, these are OS
  scheduling / GC / netpoll stalls, not limiter cost. `max` should be read as "one
  unlucky request," not a mode property.
- **tiered p99 (201µs) sits above off p99 (158µs).** Not noise — it is the ~1% of
  requests that trigger a batch refill and pay one Redis round-trip, exactly as
  the histogram shows. Expected and consistent.
- **CPU utilisation is only ~22%** during the tiered profile: at 10k trivial
  requests the machine is far from saturated, so latency here reflects per-request
  overhead, not queuing. A saturation test (higher rate until success < 100%) is a
  separate exercise.
- **`requests_total{unlimited} = 1`** in redis and tiered snapshots: the one
  readiness probe (`curl /` with no `X-API-Key`) correctly took the unlimited
  bypass. Everything else is `{allowed}`.

## Needs a human decision

- **Ports on shared machines.** This run used `REDIS_PORT=6380` and `-addr :18080`
  because 6379/8080 were occupied. `docker-compose.yml` defaults to 6379. If you
  want the harness to auto-pick free ports instead of documenting overrides, that
  is a small follow-up.
- **`results/` artifacts.** The `.txt`/`.json`/`.pprof`/`-metrics.txt` files are
  generated outputs currently sitting untracked in `results/`. Decide whether to
  commit them (reproducible record) or add `results/` to `.gitignore`.
