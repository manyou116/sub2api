package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSlowFirstOutputCountsSeparatelyAndSuccessNeverErasesSlowEvent(t *testing.T) {
	c, server := capacityTestStore(t)
	v := service.DefaultOpenAICapacityCooldownSettings()
	v.FailureThreshold, v.SlowFirstOutputThreshold = 3, 2
	ctx := context.Background()
	first, err := c.ObserveOpenAICapacity(ctx, 42, "model-a", "slow-a", 0, 3, v)
	require.NoError(t, err)
	require.False(t, first.Tripped)
	require.EqualValues(t, 1, first.Failures)
	for _, outcome := range []int{3, 2, 0, 3} {
		d, err := c.ObserveOpenAICapacity(ctx, 42, "model-a", "slow-a", 0, outcome, v)
		require.NoError(t, err)
		require.False(t, d.Tripped)
	}
	_, err = c.ObserveOpenAICapacity(ctx, 42, "model-b", "overload", 0, 1, v)
	require.NoError(t, err)
	second, err := c.ObserveOpenAICapacity(ctx, 42, "model-b", "slow-b", 0, 3, v)
	require.NoError(t, err)
	require.True(t, second.Tripped)
	require.EqualValues(t, 2, second.Failures, "success did not erase slow-a; overload was not counted as a slow request")
	d, err := c.AdmitOpenAICapacity(ctx, 42, "another-model", "blocked", 60)
	require.NoError(t, err)
	require.False(t, d.Allowed)
	server.SetTime(time.UnixMilli(second.UntilMS + 1))
	probe, err := c.AdmitOpenAICapacity(ctx, 42, "model-a", "slow-probe", 60)
	require.NoError(t, err)
	require.True(t, probe.Probe)
	slowProbe, err := c.ObserveOpenAICapacity(ctx, 42, "model-a", "slow-probe", probe.Generation, 3, v)
	require.NoError(t, err)
	require.True(t, slowProbe.Tripped, "a slow recovery probe reopens immediately, without waiting for N")
	require.EqualValues(t, 2, slowProbe.Level)
	_, err = c.ObserveOpenAICapacity(ctx, 42, "model-a", "slow-probe", probe.Generation, 2, v)
	require.NoError(t, err)
	d, err = c.AdmitOpenAICapacity(ctx, 42, "model-a", "still-blocked", 60)
	require.NoError(t, err)
	require.False(t, d.Allowed, "its later success cannot close the new generation")
}

func TestSlowFirstOutputRollingWindowSingleHitAndManualReset(t *testing.T) {
	c, server := capacityTestStore(t)
	v := service.DefaultOpenAICapacityCooldownSettings()
	v.WindowSeconds, v.SlowFirstOutputWindowSeconds, v.SlowFirstOutputThreshold = 300, 10, 2
	ctx := context.Background()
	d, err := c.ObserveOpenAICapacity(ctx, 42, "model-a", "old", 0, 3, v)
	require.NoError(t, err)
	require.False(t, d.Tripped)
	server.SetTime(time.Unix(1_800_000_010, 0))
	d, err = c.ObserveOpenAICapacity(ctx, 42, "model-b", "new", 0, 3, v)
	require.NoError(t, err)
	require.False(t, d.Tripped, "slow events expire using their own window, including the exact boundary")
	require.EqualValues(t, 1, d.Failures)
	require.NoError(t, c.ResetOpenAICapacity(ctx, 42))
	current, err := c.CurrentOpenAICapacity(ctx, 42)
	require.NoError(t, err)
	d, err = c.ObserveOpenAICapacity(ctx, 42, "model-a", "late-before-reset", 0, 3, v)
	require.NoError(t, err)
	require.False(t, d.Tripped)
	v.SlowFirstOutputThreshold = 1
	d, err = c.ObserveOpenAICapacity(ctx, 42, "model-a", "first-after-reset", current.Generation, 3, v)
	require.NoError(t, err)
	require.True(t, d.Tripped)
	require.EqualValues(t, 1, d.Failures)
	server.SetTime(time.UnixMilli(d.UntilMS + 1))
	for i := range 3 {
		id := fmt.Sprintf("healthy-%d", i)
		probe, err := c.AdmitOpenAICapacity(ctx, 42, "model-a", id, 60)
		require.NoError(t, err)
		require.True(t, probe.Probe)
		_, err = c.ObserveOpenAICapacity(ctx, 42, "model-a", id, probe.Generation, 2, v)
		require.NoError(t, err)
	}
	normal, err := c.AdmitOpenAICapacity(ctx, 42, "model-b", "normal", 60)
	require.NoError(t, err)
	require.True(t, normal.Allowed)
	require.False(t, normal.Probe)
}

func TestSlowFirstOutputDoesNotUseOverloadThreshold(t *testing.T) {
	c, _ := capacityTestStore(t)
	v := service.DefaultOpenAICapacityCooldownSettings() // overload N=1, slow N=2
	d, err := c.ObserveOpenAICapacity(context.Background(), 42, "model-a", "slow", 0, 3, v)
	require.NoError(t, err)
	require.False(t, d.Tripped)
	require.EqualValues(t, 1, d.Failures)
}
