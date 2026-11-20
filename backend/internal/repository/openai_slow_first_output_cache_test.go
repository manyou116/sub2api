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

func TestSlowFirstOutputPercentageAndIndependentCriteria(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		count, percent, slow int
		trip                 bool
	}{
		{"percentage-boundary", 0, 30, 3, true},
		{"below-percentage", 0, 31, 3, false},
		{"count-only", 3, 0, 3, true},
		{"both-disabled", 0, 0, 3, false},
		{"percentage-wins", 5, 30, 3, true},
		{"count-wins", 3, 90, 3, true},
		{"single-sample-100-percent", 0, 100, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := capacityTestStore(t)
			v := service.DefaultOpenAICapacityCooldownSettings()
			v.SlowFirstOutputThreshold, v.SlowFirstOutputPercent = tc.count, tc.percent
			ctx := context.Background()
			fast := 7
			if tc.slow == 1 {
				fast = 0
			}
			for i := range fast {
				id := fmt.Sprintf("fast-%d", i)
				for range 2 {
					d, err := c.ObserveOpenAICapacity(ctx, 42, "model-a", id, 0, 4, v)
					require.NoError(t, err)
					require.False(t, d.Tripped)
					require.EqualValues(t, i+1, d.Total, "fast retry is deduplicated")
				}
			}
			// Unmeasured successes/failures must not dilute the first-output ratio.
			for i := range 20 {
				_, err := c.ObserveOpenAICapacity(ctx, 42, "other", fmt.Sprintf("unmeasured-%d", i), 0, i%2*2, v)
				require.NoError(t, err)
			}
			for i := range tc.slow {
				d, err := c.ObserveOpenAICapacity(ctx, 42, "model-b", fmt.Sprintf("slow-%d", i), 0, 3, v)
				require.NoError(t, err)
				require.Equal(t, tc.trip && i == tc.slow-1, d.Tripped)
				require.EqualValues(t, i+1, d.Failures)
				require.EqualValues(t, fast+i+1, d.Total)
			}
		})
	}
}

func TestSlowFirstOutputPercentageWindowDeduplicationAndReset(t *testing.T) {
	c, server := capacityTestStore(t)
	ctx := context.Background()
	v := service.DefaultOpenAICapacityCooldownSettings()
	v.WindowSeconds, v.SlowFirstOutputWindowSeconds = 300, 10
	v.SlowFirstOutputThreshold, v.SlowFirstOutputPercent = 0, 50
	for _, id := range []string{"fast-a", "fast-b", "fast-c"} {
		_, err := c.ObserveOpenAICapacity(ctx, 42, "model-a", id, 0, 4, v)
		require.NoError(t, err)
	}
	for _, outcome := range []int{3, 3, 2, 4, 3} {
		d, err := c.ObserveOpenAICapacity(ctx, 42, "model-b", "slow", 0, outcome, v)
		require.NoError(t, err)
		require.False(t, d.Tripped, "same request never becomes multiple slow events")
		if outcome == 3 || outcome == 4 {
			require.EqualValues(t, 1, d.Failures)
			require.EqualValues(t, 4, d.Total)
		}
	}
	server.SetTime(time.Unix(1_800_000_010, 0))
	d, err := c.ObserveOpenAICapacity(ctx, 42, "model-a", "new-slow", 0, 3, v)
	require.NoError(t, err)
	require.True(t, d.Tripped, "fast and slow samples both expire at the slow window boundary")
	require.EqualValues(t, 1, d.Total)
	require.NoError(t, c.ResetOpenAICapacity(ctx, 42))
	current, err := c.CurrentOpenAICapacity(ctx, 42)
	require.NoError(t, err)
	_, err = c.ObserveOpenAICapacity(ctx, 42, "model-a", "stale-fast", 0, 4, v)
	require.NoError(t, err)
	d, err = c.ObserveOpenAICapacity(ctx, 42, "model-b", "fresh", current.Generation, 3, v)
	require.NoError(t, err)
	require.True(t, d.Tripped, "manual reset clears samples and fences stale fast results")
	require.EqualValues(t, 1, d.Total)
}

func TestSlowFirstOutputTimelyContentDoesNotReleaseRecoveryLease(t *testing.T) {
	c, server := capacityTestStore(t)
	ctx := context.Background()
	v := service.DefaultOpenAICapacityCooldownSettings()
	v.SlowFirstOutputThreshold, v.SlowFirstOutputPercent = 0, 100
	d, err := c.ObserveOpenAICapacity(ctx, 42, "model-a", "slow", 0, 3, v)
	require.NoError(t, err)
	require.True(t, d.Tripped)
	server.SetTime(time.UnixMilli(d.UntilMS + 1))
	for i := range 3 {
		id := fmt.Sprintf("probe-%d", i)
		probe, err := c.AdmitOpenAICapacity(ctx, 42, "model-a", id, 60)
		require.NoError(t, err)
		require.True(t, probe.Probe)
		_, err = c.ObserveOpenAICapacity(ctx, 42, "model-a", id, probe.Generation, 4, v)
		require.NoError(t, err)
		other, err := c.AdmitOpenAICapacity(ctx, 42, "model-b", "other", 60)
		require.NoError(t, err)
		require.False(t, other.Allowed, "timely output alone is not a successful probe")
		_, err = c.ObserveOpenAICapacity(ctx, 42, "model-a", id, probe.Generation, 2, v)
		require.NoError(t, err)
	}
	normal, err := c.AdmitOpenAICapacity(ctx, 42, "model-a", "normal", 60)
	require.NoError(t, err)
	require.True(t, normal.Allowed)
	require.False(t, normal.Probe)
}
