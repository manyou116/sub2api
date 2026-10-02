package repository

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	schedulerCursorPrefix      = "sched:cursor:"
	schedulerPriorityLedgerKey = "sched:priority-ledger"
	schedulerWindowTTL         = 7 * 24 * time.Hour
)

// A global generation conservatively invalidates uniform-priority proofs when
// any account priority changes. Same-priority credential/status writes do not
// advance it. The random epoch prevents an evicted ledger resurrecting proofs.
var initializeSchedulerPriorityLedgerScript = redis.NewScript(`
redis.call('HSETNX', KEYS[1], 'epoch', ARGV[1])
redis.call('HSETNX', KEYS[1], 'generation', '0')
return redis.call('HMGET', KEYS[1], 'epoch', 'generation')
`)

var setSchedulerMetadataWithPriorityScript = redis.NewScript(`
redis.call('HSETNX', KEYS[2], 'epoch', ARGV[4])
redis.call('HSETNX', KEYS[2], 'generation', '0')
local changed = 0
if redis.call('HGET', KEYS[2], ARGV[1]) ~= ARGV[2] then
    redis.call('HINCRBY', KEYS[2], 'generation', 1)
    redis.call('HSET', KEYS[2], ARGV[1], ARGV[2])
    changed = 1
end
redis.call('SET', KEYS[1], ARGV[3])
return changed
`)

var deleteSchedulerAccountWithPriorityScript = redis.NewScript(`
if redis.call('HDEL', KEYS[1], ARGV[1]) > 0 then
    redis.call('HINCRBY', KEYS[1], 'generation', 1)
end
return redis.call('DEL', KEYS[2], KEYS[3], KEYS[4])
`)

var writeSchedulerPriorityProofScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'epoch') ~= ARGV[1] or
   redis.call('HGET', KEYS[1], 'generation') ~= ARGV[2] then
    return 0
end
redis.call('SET', KEYS[2], ARGV[1] .. ':' .. ARGV[2] .. ':' .. ARGV[3], 'EX', ARGV[4])
return 1
`)

// Pin membership with the caller's version, but share the cursor across
// versions and processes. Advancing and renewing it is one atomic operation.
var readSchedulerWindowScript = redis.NewScript(`
local proof = redis.call('GET', KEYS[2])
if proof == false then return {} end
local epoch, generation, priority = string.match(proof, '^([^:]+):(%d+):(-?%d+)$')
if epoch == nil or redis.call('HGET', KEYS[3], 'epoch') ~= epoch or
   redis.call('HGET', KEYS[3], 'generation') ~= generation then
    return {}
end
local count = redis.call('ZCARD', KEYS[1])
if count == 0 then return {} end
local width = math.min(tonumber(ARGV[1]), count)
local next = redis.call('INCRBY', KEYS[4], width)
redis.call('EXPIRE', KEYS[4], ARGV[2])
local first = (next - width) % count
local last = first + width
local result = {proof}
for _, id in ipairs(redis.call('ZRANGE', KEYS[1], first, math.min(last, count) - 1)) do
    table.insert(result, id)
end
if last > count then
    for _, id in ipairs(redis.call('ZRANGE', KEYS[1], 0, last - count - 1)) do
        table.insert(result, id)
    end
end
return result
`)

type schedulerPriorityProof struct {
	epoch      string
	generation int64
	priority   int
	uniform    bool
}

func schedulerBoundedProbeEnabled() bool {
	flag := strings.TrimSpace(os.Getenv("SUB2API_SCHEDULER_BOUNDED_PROBE"))
	return flag == "1" || strings.EqualFold(flag, "true")
}

func (c *schedulerCache) writeSnapshotPriorityProof(ctx context.Context, bucket service.SchedulerBucket, version string, proof schedulerPriorityProof) error {
	if !proof.uniform {
		return nil
	}
	return writeSchedulerPriorityProofScript.Run(ctx, c.rdb, []string{
		schedulerPriorityLedgerKey, schedulerSnapshotKey(bucket, version) + ":uniform",
	}, proof.epoch, proof.generation, proof.priority, int64(schedulerWindowTTL/time.Second)).Err()
}

func (c *schedulerCache) writeAccountIDs(ctx context.Context, accounts []service.Account) ([]int64, error) {
	ids, _, err := c.writeAccountIDsWithPriorityProof(ctx, accounts)
	return ids, err
}

func (c *schedulerCache) writeAccountIDsWithPriorityProof(ctx context.Context, accounts []service.Account) ([]int64, schedulerPriorityProof, error) {
	proof := schedulerPriorityProof{}
	if len(accounts) == 0 {
		return nil, proof, nil
	}
	tracking := schedulerBoundedProbeEnabled()
	var epochSeed string
	if tracking {
		var err error
		epochSeed, err = newSchedulerGroupLifecycleOwnerToken()
		if err != nil {
			return nil, proof, err
		}
		ledger, err := initializeSchedulerPriorityLedgerScript.Run(ctx, c.rdb, []string{schedulerPriorityLedgerKey}, epochSeed).StringSlice()
		if err != nil {
			return nil, proof, err
		}
		proof.epoch = ledger[0]
		proof.generation, err = strconv.ParseInt(ledger[1], 10, 64)
		if err != nil {
			return nil, proof, err
		}
	}
	proof.uniform = tracking
	pipe := c.rdb.Pipeline()
	accountIDs := make([]int64, 0, len(accounts))
	updates := make([]*redis.Cmd, 0, c.writeChunkSize)
	pending := 0
	flush := func() error {
		if pending == 0 {
			return nil
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return err
		}
		for _, update := range updates {
			changed, err := update.Int64()
			if err != nil {
				return err
			}
			proof.generation += changed
		}
		pipe = c.rdb.Pipeline()
		updates = updates[:0]
		pending = 0
		return nil
	}
	for _, account := range accounts {
		fullPayload, metaPayload, err := marshalSchedulerCacheAccount(account)
		if err != nil {
			slog.Warn("scheduler cache skips account with unencodable payload", "account_id", account.ID, "error", err)
			continue
		}
		if len(accountIDs) == 0 {
			proof.priority = account.Priority
		} else if proof.priority != account.Priority {
			proof.uniform = false
		}
		id := strconv.FormatInt(account.ID, 10)
		pipe.Set(ctx, schedulerAccountKey(id), fullPayload, 0)
		if tracking {
			updates = append(updates, setSchedulerMetadataWithPriorityScript.Eval(ctx, pipe,
				[]string{schedulerAccountMetaKey(id), schedulerPriorityLedgerKey}, id, account.Priority, metaPayload, epochSeed))
		} else {
			pipe.Set(ctx, schedulerAccountMetaKey(id), metaPayload, 0)
		}
		// The hot LastUsedAt side key must survive lagging snapshot rebuilds.
		accountIDs = append(accountIDs, account.ID)
		pending++
		if pending >= c.writeChunkSize {
			if err := flush(); err != nil {
				return nil, proof, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, proof, err
	}
	proof.uniform = proof.uniform && len(accountIDs) > 0
	return accountIDs, proof, nil
}

func (c *schedulerCache) GetSnapshotWindow(ctx context.Context, bucket service.SchedulerBucket, width int) ([]*service.Account, bool, error) {
	if width <= 0 {
		return nil, false, nil
	}
	state, err := c.rdb.MGet(ctx, schedulerBucketKey(schedulerReadyPrefix, bucket), schedulerBucketKey(schedulerActivePrefix, bucket)).Result()
	if err != nil {
		return nil, false, err
	}
	if len(state) != 2 || state[0] != "1" || state[1] == nil {
		return nil, false, nil
	}
	snapshotKey := schedulerSnapshotKey(bucket, fmt.Sprint(state[1]))
	window, err := readSchedulerWindowScript.Run(ctx, c.rdb, []string{
		snapshotKey, snapshotKey + ":uniform", schedulerPriorityLedgerKey, schedulerBucketKey(schedulerCursorPrefix, bucket),
	}, width, int64(schedulerWindowTTL/time.Second)).StringSlice()
	if err != nil || len(window) == 0 {
		return nil, false, err
	}
	proof := strings.Split(window[0], ":")
	if len(proof) != 3 {
		return nil, false, nil
	}
	priority, err := strconv.Atoi(proof[2])
	if err != nil {
		return nil, false, err
	}
	ids := window[1:]
	if len(ids) == 0 {
		return nil, false, nil
	}
	metadataKeys := make([]string, len(ids))
	lastUsedKeys := make([]string, len(ids))
	for i, id := range ids {
		metadataKeys[i], lastUsedKeys[i] = schedulerAccountMetaKey(id), schedulerLastUsedKey(id)
	}
	pipe := c.rdb.Pipeline()
	metadataCmd := pipe.MGet(ctx, metadataKeys...)
	lastUsedCmd := pipe.MGet(ctx, lastUsedKeys...)
	ledgerCmd := pipe.HMGet(ctx, schedulerPriorityLedgerKey, "epoch", "generation")
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, false, err
	}
	ledger := ledgerCmd.Val()
	if len(ledger) != 2 || ledger[0] == nil || ledger[1] == nil {
		return nil, false, nil
	}
	if ledger[0] != proof[0] || ledger[1] != proof[1] {
		return nil, false, nil
	}
	accounts := make([]*service.Account, 0, len(ids))
	lastUsed := lastUsedCmd.Val()
	metadata := metadataCmd.Val()
	if len(metadata) != len(ids) || len(lastUsed) != len(ids) {
		return nil, false, nil
	}
	for i, value := range metadata {
		if value == nil {
			return nil, false, nil
		}
		account, err := decodeCachedAccount(value)
		if err != nil {
			return nil, false, err
		}
		if account.Priority != priority {
			return nil, false, nil
		}
		if err := applySchedulerLastUsed(account, lastUsed[i]); err != nil {
			return nil, false, err
		}
		accounts = append(accounts, account)
	}
	return accounts, true, nil
}
