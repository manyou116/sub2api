//go:build unit

package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type snapshotReadBatchHook struct {
	batches   [][]redis.Cmder
	before    func(int, []redis.Cmder) error
	after     func(int, []redis.Cmder) error
	readErrAt string
}

func (h *snapshotReadBatchHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *snapshotReadBatchHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if len(cmd.Args()) > 1 && cmd.Args()[1] == h.readErrAt {
			return errors.New("redis read failed")
		}
		return next(ctx, cmd)
	}
}
func (h *snapshotReadBatchHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.batches = append(h.batches, cmds)
		if h.before != nil {
			if err := h.before(len(h.batches), cmds); err != nil {
				return err
			}
		}
		if err := next(ctx, cmds); err != nil {
			return err
		}
		if h.after != nil {
			return h.after(len(h.batches), cmds)
		}
		return nil
	}
}

// Seed the pre-existing Redis schema directly, without a current snapshot writer.
func seedLegacySnapshotRead(t *testing.T, cache *schedulerCache, count int) (service.SchedulerBucket, []int64, time.Time) {
	t.Helper()
	ctx := context.Background()
	bucket := service.SchedulerBucket{GroupID: 81, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	embedded := time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC)
	ids := make([]int64, count)
	pipe := cache.rdb.Pipeline()
	pipe.Set(ctx, schedulerBucketKey(schedulerReadyPrefix, bucket), "1", 0)
	pipe.Set(ctx, schedulerBucketKey(schedulerActivePrefix, bucket), "71", 0)
	for i := range ids {
		ids[i] = int64(10_000 + count - i) // Deliberately unlike numeric ID order.
		id := strconv.FormatInt(ids[i], 10)
		payload := fmt.Sprintf(`{"ID":%d,"Platform":"openai","Status":"active","Schedulable":true,"GroupIDs":[81],"LastUsedAt":"2026-09-22T01:00:00Z","Credentials":{"model_mapping":{"requested":"upstream"}}}`, ids[i])
		pipe.Set(ctx, schedulerAccountMetaKey(id), payload, 0)
		pipe.ZAdd(ctx, schedulerSnapshotKey(bucket, "71"), redis.Z{Score: float64(i), Member: id})
		switch i % 3 {
		case 0:
			pipe.Set(ctx, schedulerLastUsedKey(id), embedded.Add(time.Hour).UnixMilli(), 0)
		case 1:
			pipe.Set(ctx, schedulerLastUsedKey(id), embedded.Add(-time.Hour).UnixMilli(), 0)
		}
	}
	_, err := pipe.Exec(ctx)
	require.NoError(t, err)
	return bucket, ids, embedded
}

func TestSchedulerCacheSnapshotReadPreservesLegacyPayloadOrderAndBounds(t *testing.T) {
	for _, chunkSize := range []int{0, 17, 128, 511, 4096} {
		t.Run(strconv.Itoa(chunkSize), func(t *testing.T) {
			cache := newSchedulerCacheUnit(t)
			cache.mgetChunkSize = chunkSize
			bucket, ids, embedded := seedLegacySnapshotRead(t, cache, 2055)
			hook := &snapshotReadBatchHook{}
			cache.rdb.AddHook(hook)

			accounts, hit, err := cache.GetSnapshot(context.Background(), bucket)
			require.NoError(t, err)
			require.True(t, hit)
			require.Len(t, accounts, len(ids))
			for i, account := range accounts {
				require.Equal(t, ids[i], account.ID)
				require.Equal(t, []int64{81}, account.GroupIDs)
				require.Equal(t, "upstream", account.GetModelMapping()["requested"])
				want := embedded
				if i%3 == 0 {
					want = embedded.Add(time.Hour)
				}
				require.Equal(t, want, *account.LastUsedAt)
			}
			require.Greater(t, len(hook.batches), 1)
			readAccounts := 0
			for _, commands := range hook.batches {
				require.LessOrEqual(t, len(commands), 16)
				require.Zero(t, len(commands)%2)
				batchAccounts := 0
				for i := 0; i < len(commands); i += 2 {
					metadata, lastUsed := commands[i], commands[i+1]
					require.Equal(t, "mget", metadata.Name())
					require.Equal(t, "mget", lastUsed.Name())
					require.Len(t, lastUsed.Args(), len(metadata.Args()))
					batchAccounts += len(metadata.Args()) - 1
					for j, key := range metadata.Args()[1:] {
						id := strings.TrimPrefix(key.(string), schedulerAccountMetaPrefix)
						require.Equal(t, schedulerLastUsedKey(id), lastUsed.Args()[j+1])
					}
				}
				require.LessOrEqual(t, batchAccounts, 1024)
				readAccounts += batchAccounts
			}
			require.Equal(t, len(ids), readAccounts)
		})
	}
}

func publishSnapshotWindowFixture(t *testing.T, cache *schedulerCache, bucket service.SchedulerBucket, accounts []*service.Account) {
	t.Helper()
	values := make([]service.Account, len(accounts))
	for i, account := range accounts {
		values[i] = *account
	}
	ctx := context.Background()
	token, err := cache.CaptureBucketWriteToken(ctx, bucket)
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(ctx, bucket, token, values))
}

func seedSnapshotWindowRead(t *testing.T, cache *schedulerCache, count int) (service.SchedulerBucket, []int64, time.Time) {
	t.Helper()
	t.Setenv("SUB2API_SCHEDULER_BOUNDED_PROBE", "true")
	bucket, ids, embedded := seedLegacySnapshotRead(t, cache, count)
	accounts, hit, err := cache.GetSnapshot(context.Background(), bucket)
	require.NoError(t, err)
	require.True(t, hit)
	// Re-publish through the real writer from a clean Redis namespace so the
	// active version and priority-proof ledger are initialized consistently.
	require.NoError(t, cache.rdb.FlushDB(context.Background()).Err())
	publishSnapshotWindowFixture(t, cache, bucket, accounts)
	return bucket, ids, embedded
}

func TestSchedulerCacheSnapshotWindowRequiresPublishedProof(t *testing.T) {
	cache := newSchedulerCacheUnit(t)
	bucket, _, _ := seedLegacySnapshotRead(t, cache, 5)
	accounts, hit, err := cache.GetSnapshotWindow(context.Background(), bucket, 2)
	require.NoError(t, err)
	require.False(t, hit)
	require.Nil(t, accounts)
}

func TestSchedulerCacheSnapshotWindowRotatesAndWraps(t *testing.T) {
	cache := newSchedulerCacheUnit(t)
	bucket, ids, _ := seedSnapshotWindowRead(t, cache, 5)
	ctx := context.Background()
	want := [][]int64{
		ids[:2],
		ids[2:4],
		[]int64{ids[4], ids[0]},
	}
	for _, expected := range want {
		accounts, hit, err := cache.GetSnapshotWindow(ctx, bucket, 2)
		require.NoError(t, err)
		require.True(t, hit)
		got := make([]int64, 0, len(accounts))
		for _, account := range accounts {
			got = append(got, account.ID)
		}
		require.Equal(t, expected, got)
	}
}

func TestSchedulerCacheSnapshotWindowRejectsMixedPriorityPools(t *testing.T) {
	for _, priorities := range [][]int{{0, 0, 1, 1}, {2, 2, 1, 1, 1}} {
		t.Run(fmt.Sprint(priorities), func(t *testing.T) {
			cache := newSchedulerCacheUnit(t)
			bucket, _, _ := seedSnapshotWindowRead(t, cache, len(priorities))
			accounts, hit, err := cache.GetSnapshot(context.Background(), bucket)
			require.NoError(t, err)
			require.True(t, hit)
			for i, account := range accounts {
				account.Priority = priorities[i]
			}
			publishSnapshotWindowFixture(t, cache, bucket, accounts)
			for request := 0; request < 3; request++ {
				window, hit, err := cache.GetSnapshotWindow(context.Background(), bucket, 2)
				require.NoError(t, err)
				require.False(t, hit, "mixed priorities require the original global ordering")
				require.Nil(t, window)
			}
		})
	}
}

func TestSchedulerCacheSnapshotWindowClampsWidthAndPreservesLastUsed(t *testing.T) {
	cache := newSchedulerCacheUnit(t)
	bucket, ids, embedded := seedSnapshotWindowRead(t, cache, 3)
	for request := 0; request < 2; request++ {
		accounts, hit, err := cache.GetSnapshotWindow(context.Background(), bucket, 1024)
		require.NoError(t, err)
		require.True(t, hit)
		require.Len(t, accounts, len(ids))
		for i, account := range accounts {
			require.Equal(t, ids[i], account.ID)
			want := embedded
			if i == 0 {
				want = embedded.Add(time.Hour)
			}
			require.Equal(t, want, *account.LastUsedAt)
		}
	}
}

func TestSchedulerCacheSnapshotWindowTreatsMissingMetadataAsMiss(t *testing.T) {
	cache := newSchedulerCacheUnit(t)
	bucket, ids, _ := seedSnapshotWindowRead(t, cache, 3)
	cache.rdb.Del(context.Background(), schedulerAccountMetaKey(strconv.FormatInt(ids[1], 10)))
	accounts, hit, err := cache.GetSnapshotWindow(context.Background(), bucket, 3)
	require.NoError(t, err)
	require.False(t, hit)
	require.Nil(t, accounts)
}

func TestSchedulerCacheSnapshotWindowContinuesAcrossActiveVersions(t *testing.T) {
	cache := newSchedulerCacheUnit(t)
	bucket, ids, _ := seedSnapshotWindowRead(t, cache, 3)
	ctx := context.Background()
	_, hit, err := cache.GetSnapshotWindow(ctx, bucket, 2)
	require.NoError(t, err)
	require.True(t, hit)
	accounts, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	accounts[0], accounts[2] = accounts[2], accounts[0]
	publishSnapshotWindowFixture(t, cache, bucket, accounts)
	accounts, hit, err = cache.GetSnapshotWindow(ctx, bucket, 2)
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, []int64{ids[0], ids[2]}, []int64{accounts[0].ID, accounts[1].ID})
}

func TestSchedulerCacheSnapshotWindowCoversEveryAccountEqually(t *testing.T) {
	cache := newSchedulerCacheUnit(t)
	bucket, ids, _ := seedSnapshotWindowRead(t, cache, 31)
	seen := make(map[int64]int, len(ids))
	// Coprime pool/window sizes exercise every wrap offset, not just whole pages.
	for request := 0; request < len(ids); request++ {
		accounts, hit, err := cache.GetSnapshotWindow(context.Background(), bucket, 7)
		require.NoError(t, err)
		require.True(t, hit)
		require.Len(t, accounts, 7)
		windowIDs := make(map[int64]bool, len(accounts))
		for _, account := range accounts {
			require.False(t, windowIDs[account.ID], "duplicate account within a window")
			windowIDs[account.ID] = true
			seen[account.ID]++
		}
	}
	for _, id := range ids {
		require.Equal(t, 7, seen[id], "account %d must receive equal candidate exposure", id)
	}
}

func TestSchedulerCacheSnapshotWindowSharesCursorAcrossClients(t *testing.T) {
	cache, mr := newSchedulerCacheUnitWithRedis(t)
	bucket, ids, _ := seedSnapshotWindowRead(t, cache, 128)
	type outcome struct {
		accounts []*service.Account
		hit      bool
		err      error
	}
	results := make(chan outcome, 32)
	var workers sync.WaitGroup
	for range 4 {
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })
		instance := NewSchedulerCache(rdb).(*schedulerCache)
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 8 {
				accounts, hit, err := instance.GetSnapshotWindow(context.Background(), bucket, 4)
				results <- outcome{accounts, hit, err}
			}
		}()
	}
	workers.Wait()
	close(results)
	seen := make(map[int64]int, len(ids))
	for result := range results {
		require.NoError(t, result.err)
		require.True(t, result.hit)
		require.Len(t, result.accounts, 4)
		for _, account := range result.accounts {
			seen[account.ID]++
		}
	}
	for _, id := range ids {
		require.Equal(t, 1, seen[id], "separate clients must share one atomic rotation")
	}
}

func TestSchedulerCacheSnapshotWindowPinsVersionDuringPublish(t *testing.T) {
	cache, mr := newSchedulerCacheUnitWithRedis(t)
	bucket, ids, _ := seedSnapshotWindowRead(t, cache, 5)
	cache.rdb.AddHook(&snapshotReadBatchHook{before: func(_ int, _ []redis.Cmder) error {
		// Publishing a different active version must not mix its membership into
		// an in-flight window, even when the new version is unavailable locally.
		return mr.Set(schedulerBucketKey(schedulerActivePrefix, bucket), "72")
	}})
	accounts, hit, err := cache.GetSnapshotWindow(context.Background(), bucket, 2)
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, []int64{ids[0], ids[1]}, []int64{accounts[0].ID, accounts[1].ID})
	accounts, hit, err = cache.GetSnapshotWindow(context.Background(), bucket, 2)
	require.NoError(t, err)
	require.False(t, hit)
	require.Nil(t, accounts)
}

func TestSchedulerCacheSnapshotWindowFailureNeverReturnsPartialCandidates(t *testing.T) {
	for _, failure := range []string{"invalid_metadata", "invalid_last_used", "missing_head", "pipeline_error", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			cache, mr := newSchedulerCacheUnitWithRedis(t)
			bucket, ids, _ := seedSnapshotWindowRead(t, cache, 5)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			id := strconv.FormatInt(ids[1], 10)
			switch failure {
			case "invalid_metadata":
				require.NoError(t, mr.Set(schedulerAccountMetaKey(id), "{"))
			case "invalid_last_used":
				require.NoError(t, mr.Set(schedulerLastUsedKey(id), "invalid"))
			case "missing_head":
				_, hit, err := cache.GetSnapshotWindow(ctx, bucket, 2)
				require.NoError(t, err)
				require.True(t, hit)
				mr.Del(schedulerAccountMetaKey(strconv.FormatInt(ids[2], 10)))
			case "pipeline_error":
				cache.rdb.AddHook(&snapshotReadBatchHook{before: func(_ int, _ []redis.Cmder) error {
					return errors.New("window pipeline unavailable")
				}})
			case "canceled":
				cancel()
			}
			accounts, hit, err := cache.GetSnapshotWindow(ctx, bucket, 2)
			require.Nil(t, accounts)
			require.False(t, hit)
			if failure == "missing_head" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestSchedulerCacheSnapshotWindowPriorityChangeInvalidatesProof(t *testing.T) {
	cache := newSchedulerCacheUnit(t)
	bucket, ids, _ := seedSnapshotWindowRead(t, cache, 5)
	ctx := context.Background()
	_, hit, err := cache.GetSnapshotWindow(ctx, bucket, 2)
	require.NoError(t, err)
	require.True(t, hit)
	// Updating an account outside the next window must invalidate its proof;
	// otherwise a newly higher-priority account could be silently skipped.
	changed, err := cache.GetAccount(ctx, ids[4])
	require.NoError(t, err)
	changed.Priority = -1
	require.NoError(t, cache.SetAccount(ctx, changed))
	accounts, hit, err := cache.GetSnapshotWindow(ctx, bucket, 2)
	require.NoError(t, err)
	require.False(t, hit)
	require.Nil(t, accounts)
	// The full snapshot remains available while its window proof is stale.
	accounts, hit, err = cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Len(t, accounts, 5)
	require.Equal(t, -1, accounts[4].Priority)
}

func TestSchedulerCacheSnapshotWindowCredentialRefreshPreservesProof(t *testing.T) {
	cache := newSchedulerCacheUnit(t)
	bucket, ids, _ := seedSnapshotWindowRead(t, cache, 5)
	ctx := context.Background()
	_, hit, err := cache.GetSnapshotWindow(ctx, bucket, 2)
	require.NoError(t, err)
	require.True(t, hit)
	changed, err := cache.GetAccount(ctx, ids[4])
	require.NoError(t, err)
	changed.Credentials["access_token"] = "synthetic-refreshed-token"
	require.NoError(t, cache.SetAccount(ctx, changed))
	accounts, hit, err := cache.GetSnapshotWindow(ctx, bucket, 2)
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, []int64{ids[2], ids[3]}, []int64{accounts[0].ID, accounts[1].ID})
}

func TestSchedulerCacheSnapshotReadPinsMembershipAndReadsDynamicLastUsed(t *testing.T) {
	cache, mr := newSchedulerCacheUnitWithRedis(t)
	bucket, ids, embedded := seedLegacySnapshotRead(t, cache, 2055)
	changed := ids[1030]
	newer := embedded.Add(2 * time.Hour)
	hook := &snapshotReadBatchHook{before: func(batch int, _ []redis.Cmder) error {
		if batch == 2 {
			// Another instance publishes a new membership version and a hot LRU update.
			require.NoError(t, mr.Set(schedulerBucketKey(schedulerActivePrefix, bucket), "72"))
			require.NoError(t, mr.Set(schedulerLastUsedKey(strconv.FormatInt(changed, 10)), strconv.FormatInt(newer.UnixMilli(), 10)))
		}
		return nil
	}}
	cache.rdb.AddHook(hook)
	accounts, hit, err := cache.GetSnapshot(context.Background(), bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Len(t, accounts, len(ids))
	for i, account := range accounts {
		require.Equal(t, ids[i], account.ID)
	}
	require.Equal(t, newer, *accounts[1030].LastUsedAt)
}

func TestSchedulerCacheSnapshotReadLateFailureNeverReturnsPartialPool(t *testing.T) {
	for _, failure := range []string{"missing_metadata", "invalid_metadata", "invalid_last_used", "redis_error", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			cache, mr := newSchedulerCacheUnitWithRedis(t)
			bucket, ids, _ := seedLegacySnapshotRead(t, cache, 2055)
			id := strconv.FormatInt(ids[1030], 10)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wantErr := errors.New("redis pipeline failed after an earlier batch")
			hook := &snapshotReadBatchHook{}
			switch failure {
			case "missing_metadata":
				mr.Del(schedulerAccountMetaKey(id))
			case "invalid_metadata":
				require.NoError(t, mr.Set(schedulerAccountMetaKey(id), "{"))
			case "invalid_last_used":
				require.NoError(t, mr.Set(schedulerLastUsedKey(id), "invalid"))
			case "redis_error":
				hook.after = func(batch int, cmds []redis.Cmder) error {
					if batch == 2 {
						cmds[2].SetErr(wantErr)
						return wantErr
					}
					return nil
				}
			case "canceled":
				hook.after = func(batch int, _ []redis.Cmder) error {
					if batch == 1 {
						cancel()
					}
					return nil
				}
			}
			cache.rdb.AddHook(hook)
			accounts, hit, err := cache.GetSnapshot(ctx, bucket)
			require.Nil(t, accounts)
			require.False(t, hit)
			if failure == "missing_metadata" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			if failure == "redis_error" {
				require.ErrorIs(t, err, wantErr)
			}
			if failure == "canceled" {
				require.ErrorIs(t, err, context.Canceled)
				require.Len(t, hook.batches, 1)
			}
		})
	}
}

func TestSchedulerCacheSnapshotReadPreservesNotReadyAndEmptyBoundaries(t *testing.T) {
	for _, state := range []string{"no_ready", "wrong_ready", "no_active", "empty", "ready_error", "active_error", "membership_error"} {
		t.Run(state, func(t *testing.T) {
			cache, mr := newSchedulerCacheUnitWithRedis(t)
			bucket, _, _ := seedLegacySnapshotRead(t, cache, 0)
			switch state {
			case "no_ready":
				mr.Del(schedulerBucketKey(schedulerReadyPrefix, bucket))
			case "wrong_ready":
				require.NoError(t, mr.Set(schedulerBucketKey(schedulerReadyPrefix, bucket), "0"))
			case "no_active":
				mr.Del(schedulerBucketKey(schedulerActivePrefix, bucket))
			}
			hook := &snapshotReadBatchHook{}
			switch state {
			case "ready_error":
				hook.readErrAt = schedulerBucketKey(schedulerReadyPrefix, bucket)
			case "active_error":
				hook.readErrAt = schedulerBucketKey(schedulerActivePrefix, bucket)
			case "membership_error":
				hook.readErrAt = schedulerSnapshotKey(bucket, "71")
			}
			cache.rdb.AddHook(hook)
			accounts, hit, err := cache.GetSnapshot(context.Background(), bucket)
			if hook.readErrAt != "" {
				require.EqualError(t, err, "redis read failed")
			} else {
				require.NoError(t, err)
			}
			require.Nil(t, accounts)
			require.False(t, hit)
			require.Empty(t, hook.batches)
		})
	}
}
