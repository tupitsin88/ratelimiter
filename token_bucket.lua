-- KEYS[1]  — ключ бакета
-- ARGV[1]  — rate: токенов в секунду
-- ARGV[2]  — capacity: ёмкость (burst)
-- ARGV[3]  — cost: сколько списываем за вызов
-- return   — {allowed(0/1), remaining(строка)}

---@diagnostic disable: undefined-global
local KEYS     = KEYS
local ARGV     = ARGV

local rate     = tonumber(ARGV[1]) or 0
local capacity = tonumber(ARGV[2]) or 0
local cost     = tonumber(ARGV[3]) or 1

local t        = redis.call('TIME')
local now      = tonumber(t[1]) * 1000000 + tonumber(t[2]) -- time to microseconds

local st       = redis.call("HGETALL", KEYS[1])
local tokens   = nil
local ts       = nil

for i = 1, #st, 2 do
    if st[i] == "tokens" then
        tokens = tonumber(st[i + 1])
    end
    if st[i] == "ts" then
        ts = tonumber(st[i + 1])
    end
end

if tokens == nil or ts == nil then -- first time
    tokens = capacity
else                               -- not first time, refill tokens
    local elapsed = now - ts
    if elapsed < 0 then
        elapsed = 0
    end
    tokens = math.min(capacity, tokens + (elapsed * rate / 1000000))
end

local allowed = 0
if tokens >= cost then
    tokens = tokens - cost
    allowed = 1

    redis.call("HSET", KEYS[1], "tokens", tokens, "ts", now) -- update tokens and timestamp

    local ttl = math.ceil((capacity - tokens) / rate)
    if ttl < 60 then
        ttl = 60
    end

    redis.call("EXPIRE", KEYS[1], ttl)
end

return { allowed, tostring(tokens) }
