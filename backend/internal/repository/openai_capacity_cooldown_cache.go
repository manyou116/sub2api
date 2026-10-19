package repository

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// All keys share a canonical-model hash tag. The index lets a selection pass
// skip cooled accounts without a Redis round trip for every pool candidate.
func capacityKeys(accountID int64, model string) []string {
	hash := sha256.Sum256([]byte(model))
	prefix := fmt.Sprintf("openai_capacity:{%x}:", hash[:16])
	key := prefix + strconv.FormatInt(accountID, 10)
	return []string{key, key + ":events", key + ":failures", key + ":successes", prefix + "blocked"}
}

var capacityCooldownScript = redis.NewScript(`
local clock = redis.call('TIME')
local now = tonumber(clock[1])*1000 + math.floor(tonumber(clock[2])/1000)
local op, account, id = ARGV[1], ARGV[2], ARGV[3]
local state, events, failures, successes, blocked = unpack(KEYS)
if op == 'blocked' then
  return redis.call('ZRANGEBYSCORE', blocked, now+1, '+inf')
end
local generation = tonumber(redis.call('HGET', state, 'generation') or '0')
local level = tonumber(redis.call('HGET', state, 'level') or '0')
local until_ms = tonumber(redis.call('HGET', state, 'until') or '0')
local phase = tonumber(redis.call('HGET', state, 'phase') or '0')
local function result(allowed, probe, tripped, f, n)
  return {allowed,probe,tripped,generation,until_ms,f or 0,n or 0,level}
end
if op == 'release' then
  if generation == tonumber(ARGV[4]) and redis.call('HGET',state,'owner') == id then
    redis.call('HDEL',state,'owner','lease')
    redis.call('ZREM',blocked,account)
  end
  return result(1,0,0)
end
redis.call('ZREMRANGEBYSCORE',blocked,'-inf',now)
if op == 'admit' then
  if phase == 0 then return result(1,0,0) end
  if until_ms > now then return result(0,0,0) end
  local owner = redis.call('HGET',state,'owner')
  local lease = tonumber(redis.call('HGET',state,'lease') or '0')
  if owner and owner ~= id and lease > now then return result(0,0,0) end
  local deadline = now + math.min(3600,tonumber(ARGV[4]))*1000
  redis.call('HSET',state,'owner',id,'lease',deadline)
  redis.call('EXPIRE',state,14400)
  redis.call('ZADD',blocked,deadline,account)
  redis.call('EXPIRE',blocked,14400)
  return result(1,1,0)
end
-- Late in-flight completions cannot clear or extend a newer circuit generation.
if generation ~= tonumber(ARGV[4]) then return result(1,0,0) end
local outcome = tonumber(ARGV[5])
if phase == 1 then
  if until_ms > now or redis.call('HGET',state,'owner') ~= id or
    tonumber(redis.call('HGET',state,'lease') or '0') <= now then return result(1,0,0) end
  redis.call('HDEL',state,'owner','lease')
  redis.call('ZREM',blocked,account)
  if outcome == 2 then
    local recovered = redis.call('HINCRBY',state,'recovered',1)
    if recovered >= 3 then
      generation = generation+1
      level = math.max(0,level-1)
      redis.call('HSET',state,'phase',0,'generation',generation,'level',level,'recovered',0)
      redis.call('DEL',events,failures,successes)
    end
    return result(1,0,0)
  end
  if outcome ~= 1 then return result(1,0,0) end
end
local window = tonumber(ARGV[6])*1000
for _,key in ipairs({events,failures,successes}) do
  redis.call('ZREMRANGEBYSCORE',key,'-inf',now-window)
end
local score = redis.call('ZSCORE',events,id)
if not score then score=now; redis.call('ZADD',events,score,id) end
if outcome == 2 then
  redis.call('ZADD',successes,score,id)
  redis.call('ZREM',failures,id)
elseif outcome == 1 and not redis.call('ZSCORE',successes,id) then
  redis.call('ZADD',failures,score,id)
end
for _,key in ipairs({events,failures,successes}) do redis.call('PEXPIRE',key,window+60000) end
local n, f = redis.call('ZCARD',events), redis.call('ZCARD',failures)
if phase == 1 or (f >= tonumber(ARGV[7]) and f*100 >= n*tonumber(ARGV[8])) then
  level = math.min(level+1,4)
  local scale = ({1,2,5,10})[level]
  local jitter = (90 + tonumber(string.sub(redis.sha1hex(state..tostring(now)),1,6),16)%21)/100
  local duration = math.min(tonumber(ARGV[10]),tonumber(ARGV[9])*scale*jitter)*1000
  until_ms = now + math.max(1000,math.floor(duration))
  generation = generation+1
  redis.call('HSET',state,'phase',1,'until',until_ms,'generation',generation,'level',level,'recovered',0)
  redis.call('EXPIRE',state,14400)
  redis.call('ZADD',blocked,until_ms,account)
  redis.call('EXPIRE',blocked,14400)
  redis.call('DEL',events,failures,successes)
  return result(0,0,1,f,n)
end
return result(1,0,0,f,n)
`)

func (c *tempUnschedCache) capacityDecision(ctx context.Context, accountID int64, model, op, id string, args ...any) (service.OpenAICapacityDecision, error) {
	values := append([]any{op, accountID, id}, args...)
	items, err := capacityCooldownScript.Run(ctx, c.rdb, capacityKeys(accountID, model), values...).Int64Slice()
	if err != nil {
		return service.OpenAICapacityDecision{}, err
	}
	if len(items) != 8 {
		return service.OpenAICapacityDecision{}, fmt.Errorf("invalid capacity decision length: %d", len(items))
	}
	return service.OpenAICapacityDecision{Allowed: items[0] == 1, Probe: items[1] == 1, Tripped: items[2] == 1,
		Generation: items[3], UntilMS: items[4], Failures: items[5], Total: items[6], Level: items[7]}, nil
}

func (c *tempUnschedCache) AdmitOpenAICapacity(ctx context.Context, accountID int64, model, id string, lease int) (service.OpenAICapacityDecision, error) {
	return c.capacityDecision(ctx, accountID, model, "admit", id, lease)
}

func (c *tempUnschedCache) ObserveOpenAICapacity(ctx context.Context, accountID int64, model, id string, generation int64, outcome int, v service.OpenAICapacityCooldownSettings) (service.OpenAICapacityDecision, error) {
	return c.capacityDecision(ctx, accountID, model, "record", id, generation, outcome, v.WindowSeconds, v.FailureThreshold, v.FailureRatePercent, v.CooldownSeconds, v.MaxCooldownSeconds)
}

func (c *tempUnschedCache) ReleaseOpenAICapacity(ctx context.Context, accountID int64, model, id string, generation int64) error {
	_, err := c.capacityDecision(ctx, accountID, model, "release", id, generation)
	return err
}

func (c *tempUnschedCache) BlockedOpenAICapacityAccounts(ctx context.Context, model string) ([]int64, error) {
	return capacityCooldownScript.Run(ctx, c.rdb, capacityKeys(0, model), "blocked", 0, "").Int64Slice()
}
