//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSchedulerBoundedSnapshotMemoPriorityAndDeleteInvalidateBothCaches(t *testing.T) {
	t.Setenv("SUB2API_SCHEDULER_BOUNDED_PROBE", "true")
	ctx := context.Background()
	cache, _, _ := newSchedulerCacheMemoUnit(t, 2)
	bucket := memoTestBucket(9401)
	memoPublish(t, cache, bucket, memoTestAccount(94011, "first"), memoTestAccount(94012, "second"))
	memoRead(t, cache, bucket)
	_, hit, err := cache.GetSnapshotWindow(ctx, bucket, 1)
	require.NoError(t, err)
	require.True(t, hit)

	changed, err := cache.GetAccount(ctx, 94012)
	require.NoError(t, err)
	changed.Name, changed.Priority = "updated", -1
	require.NoError(t, cache.SetAccount(ctx, changed))
	accounts := memoRead(t, cache, bucket)
	require.Equal(t, "updated", accounts[1].Name)
	require.Equal(t, -1, accounts[1].Priority)
	_, hit, err = cache.GetSnapshotWindow(ctx, bucket, 1)
	require.NoError(t, err)
	require.False(t, hit, "a higher priority outside the next window invalidates its proof")

	revision := memoRevision(t, cache, schedulerAccountMetaRevisionKey)
	require.NoError(t, cache.DeleteAccount(ctx, changed.ID))
	require.NotEqual(t, revision, memoRevision(t, cache, schedulerAccountMetaRevisionKey))
	accounts, hit, err = cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.False(t, hit, "a warmed process snapshot must not resurrect the deleted account")
	require.Nil(t, accounts)
}

func TestSchedulerBoundedSnapshotMemoPlatformChangeAndPartialWriteAreVisible(t *testing.T) {
	t.Setenv("SUB2API_SCHEDULER_BOUNDED_PROBE", "true")
	ctx := context.Background()
	cache, _, recorder := newSchedulerCacheMemoUnit(t, 1)
	bucket := memoTestBucket(9402)
	memoPublish(t, cache, bucket, memoTestAccount(94021, "first"), memoTestAccount(94022, "second"))
	memoRead(t, cache, bucket)

	changed, err := cache.GetAccount(ctx, 94021)
	require.NoError(t, err)
	changed.Platform = service.PlatformGemini
	require.NoError(t, cache.SetAccount(ctx, changed))
	require.Equal(t, service.PlatformGemini, memoRead(t, cache, bucket)[0].Platform)

	before := memoRevision(t, cache, schedulerAccountMetaRevisionKey)
	changed.Name = "partially-written"
	recorder.reset()
	recorder.setFailPipelineAt(2)
	_, err = cache.writeAccountIDs(ctx, []service.Account{*changed, memoTestAccount(94022, "changed second")})
	require.Error(t, err)
	recorder.setFailPipelineAt(0)
	require.Eventually(t, func() bool {
		return before != memoRevision(t, cache, schedulerAccountMetaRevisionKey)
	}, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, "partially-written", memoRead(t, cache, bucket)[0].Name)
}
