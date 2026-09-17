package queue

import "github.com/redis/go-redis/v9"

// Every RedisQueue state transition that reads-then-writes is one Lua
// script, so concurrent processes sharing the same Redis never observe a
// partial transition (see redis.go's package doc for the full state
// machine). Scripts are loaded once per client via EVALSHA/EVAL fallback,
// which redis.NewScript already handles.

// enqueueScript: KEYS={available,claimed,job}, ARGV={id,messageID,enqueuedAtMs,availableAtMs,capacity}
// Returns 1 (enqueued), 0 (duplicate, no-op), -1 (at capacity).
var enqueueScript = redis.NewScript(`
local jobKey = KEYS[3]
if redis.call('EXISTS', jobKey) == 1 then
	return 0
end
local total = redis.call('ZCARD', KEYS[1]) + redis.call('ZCARD', KEYS[2])
if total >= tonumber(ARGV[5]) then
	return -1
end
redis.call('HSET', jobKey, 'id', ARGV[1], 'message_id', ARGV[2], 'enqueued_at', ARGV[3], 'available_at', ARGV[4], 'token', '0')
redis.call('ZADD', KEYS[1], ARGV[4], ARGV[1])
return 1
`)

// claimScript: KEYS={available,claimed}, ARGV={nowMs,leaseMs,jobPrefix,tokenSeqKey,reclaimLimit}
// First reclaims a bounded batch of lease-expired claims back to available,
// then pops the earliest due job (score<=now) and stamps a fresh token.
// Returns {} if nothing is claimable, else {id,token,messageID,enqueuedAt,availableAt}.
var claimScript = redis.NewScript(`
local expired = redis.call('ZRANGEBYSCORE', KEYS[2], '-inf', ARGV[1], 'LIMIT', 0, tonumber(ARGV[5]))
for i = 1, #expired do
	local id = expired[i]
	redis.call('ZREM', KEYS[2], id)
	redis.call('ZADD', KEYS[1], ARGV[1], id)
	redis.call('HSET', ARGV[3] .. id, 'available_at', ARGV[1], 'token', '0')
end

local top = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, 1)
if #top == 0 then
	return {}
end
local id = top[1]
redis.call('ZREM', KEYS[1], id)
local token = redis.call('INCR', ARGV[4])
local leaseExpiry = tonumber(ARGV[1]) + tonumber(ARGV[2])
redis.call('ZADD', KEYS[2], leaseExpiry, id)
local jobKey = ARGV[3] .. id
redis.call('HSET', jobKey, 'token', token)
local job = redis.call('HMGET', jobKey, 'message_id', 'enqueued_at', 'available_at')
return {id, tostring(token), job[1], job[2], job[3]}
`)

// ackScript: KEYS={claimed}, ARGV={id,token,jobPrefix}
// Returns 1 (acked), -1 (unknown job), -2 (stale/wrong token).
var ackScript = redis.NewScript(`
local jobKey = ARGV[3] .. ARGV[1]
if redis.call('EXISTS', jobKey) == 0 then
	return -1
end
local cur = redis.call('HGET', jobKey, 'token')
if cur == false or cur == '0' or cur ~= ARGV[2] then
	return -2
end
redis.call('ZREM', KEYS[1], ARGV[1])
redis.call('DEL', jobKey)
return 1
`)

// renewScript: KEYS={claimed}, ARGV={id,token,leaseExpiryMs,jobPrefix}
// Returns 1 (renewed), -1 (unknown job), -2 (stale/wrong token).
var renewScript = redis.NewScript(`
local jobKey = ARGV[4] .. ARGV[1]
if redis.call('EXISTS', jobKey) == 0 then
	return -1
end
local cur = redis.call('HGET', jobKey, 'token')
if cur == false or cur == '0' or cur ~= ARGV[2] then
	return -2
end
redis.call('ZADD', KEYS[1], ARGV[3], ARGV[1])
return 1
`)

// releaseScript: KEYS={available,claimed}, ARGV={id,token,availableAtMs,jobPrefix}
// Returns 1 (released), -1 (unknown job), -2 (stale/wrong token).
var releaseScript = redis.NewScript(`
local jobKey = ARGV[4] .. ARGV[1]
if redis.call('EXISTS', jobKey) == 0 then
	return -1
end
local cur = redis.call('HGET', jobKey, 'token')
if cur == false or cur == '0' or cur ~= ARGV[2] then
	return -2
end
redis.call('ZREM', KEYS[2], ARGV[1])
redis.call('HSET', jobKey, 'token', '0', 'available_at', ARGV[3])
redis.call('ZADD', KEYS[1], ARGV[3], ARGV[1])
return 1
`)
