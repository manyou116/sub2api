package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func webImageQuotaTestCache(t *testing.T) (service.WebImageQuotaCache, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewWebImageQuotaCache(client), server
}

func TestWebImageQuotaCacheExpiredOwnerCannotPublishOrRelease(t *testing.T) {
	cache, server := webImageQuotaTestCache(t)
	ctx := context.Background()
	id := int64(12)
	acquired, err := cache.AcquireProbe(ctx, id, "old", time.Second)
	require.NoError(t, err)
	require.True(t, acquired)
	acquired, err = cache.AcquireProbe(ctx, id, "concurrent", time.Second)
	require.NoError(t, err)
	require.False(t, acquired)
	server.FastForward(2 * time.Second)
	acquired, err = cache.AcquireProbe(ctx, id, "new", time.Second)
	require.NoError(t, err)
	require.True(t, acquired)
	written, err := cache.PublishProbe(ctx, id, "old", &service.WebImageStatus{ParentAccountID: id, QuotaKnown: true})
	require.NoError(t, err)
	require.False(t, written)
	require.NoError(t, cache.ReleaseProbe(ctx, id, "old"))
	owner, err := server.Get(webImageProbeKey(id))
	require.NoError(t, err)
	require.Equal(t, "new", owner)
	status, err := cache.Get(ctx, id)
	require.NoError(t, err)
	require.Nil(t, status)
	now := time.Now().UTC()
	written, err = cache.PublishProbe(ctx, id, "new", &service.WebImageStatus{ParentAccountID: id, ProbedAt: &now})
	require.NoError(t, err)
	require.True(t, written)
	status, err = cache.Get(ctx, id)
	require.NoError(t, err)
	require.False(t, status.QuotaKnown)
	require.Nil(t, status.Remaining)
	require.NotNil(t, status.ProbedAt)
	require.NoError(t, cache.ReleaseProbe(ctx, id, "new"))
	require.False(t, server.Exists(webImageProbeKey(id)))
	server.FastForward(601 * time.Second)
	status, err = cache.Get(ctx, id)
	require.NoError(t, err)
	require.Nil(t, status)
}

func TestWebImageQuotaCacheExhaustionFencesOutstandingProbe(t *testing.T) {
	cache, server := webImageQuotaTestCache(t)
	ctx := context.Background()
	id := int64(12)
	acquired, err := cache.AcquireProbe(ctx, id, "probe", time.Minute)
	require.NoError(t, err)
	require.True(t, acquired)
	zero := 0
	until := time.Now().Add(time.Hour).UTC()
	require.NoError(t, cache.Exhaust(ctx, id, &service.WebImageStatus{ParentAccountID: id, QuotaKnown: true, Remaining: &zero, ResetAt: &until}))
	require.False(t, server.Exists(webImageProbeKey(id)))
	remaining := 9
	written, err := cache.PublishProbe(ctx, id, "probe", &service.WebImageStatus{ParentAccountID: id, QuotaKnown: true, Remaining: &remaining})
	require.NoError(t, err)
	require.False(t, written)
	status, err := cache.Get(ctx, id)
	require.NoError(t, err)
	require.True(t, status.QuotaKnown)
	require.NotNil(t, status.Remaining)
	require.Zero(t, *status.Remaining)
	require.NoError(t, cache.Delete(ctx, id))
	status, err = cache.Get(ctx, id)
	require.NoError(t, err)
	require.Nil(t, status)
}
