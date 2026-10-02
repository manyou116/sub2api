package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type loadBatchCommandRecorder struct {
	mu    sync.Mutex
	names []string
}

func (h *loadBatchCommandRecorder) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *loadBatchCommandRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.mu.Lock()
		h.names = append(h.names, cmd.Name())
		h.mu.Unlock()
		return next(ctx, cmd)
	}
}

func (h *loadBatchCommandRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.mu.Lock()
		for _, cmd := range cmds {
			h.names = append(h.names, cmd.Name())
		}
		h.mu.Unlock()
		return next(ctx, cmds)
	}
}

func newLoadBatchTestClient(t *testing.T, server *miniredis.Miniredis) (*redis.Client, *concurrencyCache) {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	cache, ok := NewConcurrencyCache(client, 2, 90).(*concurrencyCache)
	require.True(t, ok)
	return client, cache
}

func TestGetAccountsLoadBatchCountsEffectiveSlotsWithoutWrites(t *testing.T) {
	ctx := context.Background()
	server := miniredis.RunT(t)
	now := time.Unix(2_000_000_000, 0)
	server.SetTime(now) // Deliberately different from the process clock.
	client, cache := newLoadBatchTestClient(t, server)
	_, otherInstance := newLoadBatchTestClient(t, server)
	for _, tc := range []struct {
		key string
		ttl int64
	}{{accountSlotKey(1), 120}, {liveAccountSlotKey(1), 60}} {
		cutoff := float64(now.Unix() - tc.ttl)
		for i, score := range []float64{cutoff - 1, cutoff, cutoff + 0.25, cutoff + 1, float64(now.Unix() + 1)} {
			require.NoError(t, client.ZAdd(ctx, tc.key, redis.Z{Score: score, Member: fmt.Sprint(i)}).Err())
		}
		require.NoError(t, client.Expire(ctx, tc.key, 90*time.Second).Err())
	}
	require.NoError(t, client.Set(ctx, accountWaitKey(1), "2", 90*time.Second).Err())
	require.NoError(t, client.Set(ctx, accountWaitKey(3), "legacy-nonnumeric", 90*time.Second).Err())
	require.NoError(t, client.Set(ctx, accountWaitKey(4), "9", time.Millisecond).Err())
	server.FastForward(time.Second)
	keys := []string{accountSlotKey(1), liveAccountSlotKey(1), accountWaitKey(1)}
	ttls := make([]time.Duration, len(keys))
	for i, key := range keys {
		ttls[i] = client.TTL(ctx, key).Val()
	}

	recorder := &loadBatchCommandRecorder{}
	client.AddHook(recorder)
	accounts := []service.AccountWithConcurrency{{ID: 1, MaxConcurrency: 4}, {ID: 2, MaxConcurrency: 1}, {ID: 3}, {ID: 4, MaxConcurrency: 2}}
	loads, err := cache.GetAccountsLoadBatch(ctx, accounts)
	require.NoError(t, err)
	require.Equal(t, &service.AccountLoadInfo{AccountID: 1, CurrentConcurrency: 6, WaitingCount: 2, LoadRate: 200}, loads[1])
	for _, id := range []int64{2, 3, 4} {
		require.Equal(t, &service.AccountLoadInfo{AccountID: id}, loads[id])
	}
	recorder.mu.Lock()
	commands := append([]string(nil), recorder.names...)
	recorder.mu.Unlock()
	require.Equal(t, 1+3*len(accounts), len(commands))
	require.Equal(t, "time", commands[0])
	for i := 1; i < len(commands); i += 3 {
		require.Equal(t, []string{"zcount", "zcount", "get"}, commands[i:i+3])
	}
	for i, key := range keys {
		require.Equal(t, ttls[i], client.TTL(ctx, key).Val(), "a load read must not renew TTL")
	}
	require.EqualValues(t, 5, client.ZCard(ctx, accountSlotKey(1)).Val(), "expired entries may remain physically present")
	require.EqualValues(t, 5, client.ZCard(ctx, liveAccountSlotKey(1)).Val())
	otherLoads, err := otherInstance.GetAccountsLoadBatch(ctx, accounts)
	require.NoError(t, err)
	require.Equal(t, loads, otherLoads, "instances must use the shared Redis clock")
}

func TestGetAccountsLoadBatchDoesNotHideErrorsAfterMissingWaitKey(t *testing.T) {
	for _, kind := range []string{"regular", "live", "wait"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			server := miniredis.RunT(t)
			client, cache := newLoadBatchTestClient(t, server)
			switch kind {
			case "regular":
				require.NoError(t, client.Set(ctx, accountSlotKey(2), "wrong-type", 0).Err())
			case "live":
				require.NoError(t, client.Set(ctx, liveAccountSlotKey(2), "wrong-type", 0).Err())
			case "wait":
				require.NoError(t, client.ZAdd(ctx, accountWaitKey(2), redis.Z{Score: 1, Member: "wrong-type"}).Err())
			}
			loads, err := cache.GetAccountsLoadBatch(ctx, []service.AccountWithConcurrency{{ID: 1, MaxConcurrency: 1}, {ID: 2, MaxConcurrency: 1}})
			require.ErrorContains(t, err, "WRONGTYPE")
			require.Nil(t, loads, "a later error must not be returned as an idle account")
		})
	}
}

func TestGetAccountsLoadBatchEmptyAndTimeFailure(t *testing.T) {
	ctx := context.Background()
	server := miniredis.RunT(t)
	client, cache := newLoadBatchTestClient(t, server)
	require.NoError(t, client.Ping(ctx).Err())
	server.SetError("ERR unavailable")
	loads, err := cache.GetAccountsLoadBatch(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, loads)
	loads, err = cache.GetAccountsLoadBatch(ctx, []service.AccountWithConcurrency{{ID: 1, MaxConcurrency: 1}})
	require.ErrorContains(t, err, "redis TIME")
	require.Nil(t, loads)
}

func TestLiveLeaseCountsEffectiveRegularSlotsWithoutPriorCleanup(t *testing.T) {
	for _, tc := range []struct {
		name          string
		accountActive bool
		userActive    bool
		liveActive    bool
		replacing     bool
		want          bool
	}{
		{name: "expired_regular_and_live", want: true},
		{name: "account_regular_full", accountActive: true},
		{name: "user_regular_full", userActive: true},
		{name: "live_full", liveActive: true},
		{name: "replace_regular", accountActive: true, userActive: true, replacing: true, want: true},
		{name: "replace_still_respects_live_cap", accountActive: true, userActive: true, liveActive: true, replacing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			server := miniredis.RunT(t)
			now := time.Unix(2_000_000_000, 0)
			server.SetTime(now)
			client, cache := newLoadBatchTestClient(t, server)
			_, otherInstance := newLoadBatchTestClient(t, server)
			for _, entry := range []struct {
				key    string
				active bool
			}{{accountSlotKey(10), tc.accountActive}, {userSlotKey(20), tc.userActive}} {
				cutoff := float64(now.Unix() - 120)
				require.NoError(t, client.ZAdd(ctx, entry.key,
					redis.Z{Score: cutoff - 1, Member: "expired"},
					redis.Z{Score: cutoff, Member: "boundary"}).Err())
				if entry.active {
					require.NoError(t, client.ZAdd(ctx, entry.key, redis.Z{Score: cutoff + 0.25, Member: "active"}).Err())
				}
				require.NoError(t, client.Expire(ctx, entry.key, time.Minute).Err())
			}
			for _, key := range []string{liveAccountSlotKey(10), liveUserSlotKey(20), liveAPIKeySlotKey(30)} {
				require.NoError(t, client.ZAdd(ctx, key, redis.Z{Score: float64(now.Unix() - 60), Member: "expired-live"}).Err())
				if tc.liveActive {
					require.NoError(t, client.ZAdd(ctx, key, redis.Z{Score: float64(now.Unix()) - 59.75, Member: "active-live"}).Err())
				}
			}
			// Do not call GetAccountConcurrency: it would hide the regression by pruning.
			acquired, err := cache.AcquireLiveLease(ctx, 10, 1, 20, 1, 30, "new-live", tc.replacing)
			require.NoError(t, err)
			require.Equal(t, tc.want, acquired)
			for _, key := range []string{accountSlotKey(10), userSlotKey(20)} {
				require.GreaterOrEqual(t, client.ZCard(ctx, key).Val(), int64(2), "regular stale slots remain present")
				require.Equal(t, time.Minute, client.TTL(ctx, key).Val())
			}
			if tc.want {
				acquired, err = otherInstance.AcquireLiveLease(ctx, 10, 1, 20, 1, 30, "new-live", tc.replacing)
				require.NoError(t, err)
				require.True(t, acquired, "same lease is idempotent across instances")
				acquired, err = otherInstance.AcquireLiveLease(ctx, 10, 1, 20, 1, 30, "different-live", false)
				require.NoError(t, err)
				require.False(t, acquired, "live capacity remains enforced")
				refreshed, err := otherInstance.RefreshLiveLease(ctx, 10, 20, 30, "new-live")
				require.NoError(t, err)
				require.True(t, refreshed)
			}
		})
	}
}
