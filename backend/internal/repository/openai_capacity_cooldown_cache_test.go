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

func capacityTestStore(t *testing.T) (*tempUnschedCache, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	server.SetTime(time.Unix(1_800_000_000, 0))
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return &tempUnschedCache{rdb: client}, server
}

func capacityRecord(t *testing.T, c *tempUnschedCache, id string, generation int64, outcome int) service.OpenAICapacityDecision {
	t.Helper()
	settings := service.DefaultOpenAICapacityCooldownSettings()
	settings.FailureThreshold = 3
	d, err := c.ObserveOpenAICapacity(context.Background(), 42, "model-a", id, generation, outcome, settings)
	require.NoError(t, err)
	return d
}

func capacityTrip(t *testing.T, c *tempUnschedCache) service.OpenAICapacityDecision {
	t.Helper()
	var d service.OpenAICapacityDecision
	for i := range 3 {
		d = capacityRecord(t, c, fmt.Sprint(i), 0, 1)
	}
	require.True(t, d.Tripped)
	return d
}

func TestCapacityCooldownDeduplicatesRetriesAndSSEFrames(t *testing.T) {
	c, _ := capacityTestStore(t)
	for range 10 {
		d := capacityRecord(t, c, "same-http-request", 0, 1)
		require.EqualValues(t, 1, d.Failures)
		require.EqualValues(t, 1, d.Total)
		require.False(t, d.Tripped)
	}
	capacityRecord(t, c, "second-request", 0, 1)
	d := capacityRecord(t, c, "third-request", 0, 1)
	require.True(t, d.Tripped)
	require.EqualValues(t, 3, d.Failures)
}

func TestCapacityCooldownTripsOnCountRegardlessOfSuccesses(t *testing.T) {
	c, _ := capacityTestStore(t)
	for i := range 20 {
		capacityRecord(t, c, fmt.Sprintf("ok-%d", i), 0, 2)
	}
	for i := range 3 {
		d := capacityRecord(t, c, fmt.Sprintf("bad-%d", i), 0, 1)
		require.Equal(t, i == 2, d.Tripped, "the third overload trips even with twenty successful requests")
		require.EqualValues(t, i+1, d.Failures)
	}
}

func TestCapacityCooldownSingleOverloadTripsImmediately(t *testing.T) {
	c, _ := capacityTestStore(t)
	settings := service.DefaultOpenAICapacityCooldownSettings()
	require.Equal(t, 1, settings.FailureThreshold)
	ctx := context.Background()
	for i := range 20 {
		d, err := c.ObserveOpenAICapacity(ctx, 42, "model-a", fmt.Sprintf("ok-%d", i), 0, i%2*2, settings)
		require.NoError(t, err)
		require.False(t, d.Tripped, "successful and inconclusive requests do not count as overloads")
	}
	d, err := c.ObserveOpenAICapacity(ctx, 42, "model-a", "first-overload", 0, 1, settings)
	require.NoError(t, err)
	require.True(t, d.Tripped)
	require.EqualValues(t, 1, d.Failures)
	require.EqualValues(t, 21, d.Total)
	admission, err := c.AdmitOpenAICapacity(ctx, 42, "model-a", "next-request", 60)
	require.NoError(t, err)
	require.False(t, admission.Allowed, "the very next request must avoid this account and model")
}

func TestCapacityCooldownExpiresOverloadsOutsideWindow(t *testing.T) {
	c, server := capacityTestStore(t)
	capacityRecord(t, c, "old-1", 0, 1)
	capacityRecord(t, c, "old-2", 0, 1)
	server.SetTime(time.Unix(1_800_000_061, 0))
	d := capacityRecord(t, c, "new", 0, 1)
	require.EqualValues(t, 1, d.Total)
	require.EqualValues(t, 1, d.Failures)
	require.False(t, d.Tripped, "expired overloads must not contribute to the threshold")
}

func TestCapacityCooldownRecoveredRetryCannotBecomeFailureAgain(t *testing.T) {
	c, _ := capacityTestStore(t)
	capacityRecord(t, c, "retry", 0, 1)
	d := capacityRecord(t, c, "retry", 0, 2)
	require.Zero(t, d.Failures)
	d = capacityRecord(t, c, "retry", 0, 1)
	require.Zero(t, d.Failures, "a late duplicate error cannot undo completion")
	require.EqualValues(t, 1, d.Total)
}

func TestCapacityCooldownPausesAllTextModelsAndIsolatesAccounts(t *testing.T) {
	c, _ := capacityTestStore(t)
	capacityTrip(t, c)
	ctx := context.Background()
	blocked, err := c.BlockedOpenAICapacityAccounts(ctx, "model-a")
	require.NoError(t, err)
	require.Equal(t, []int64{42}, blocked)
	for _, model := range []string{"model-a", "model-b"} {
		d, err := c.AdmitOpenAICapacity(ctx, 42, model, "fresh", 60)
		require.NoError(t, err)
		require.False(t, d.Allowed)
		blocked, err := c.BlockedOpenAICapacityAccounts(ctx, model)
		require.NoError(t, err)
		require.Equal(t, []int64{42}, blocked)
		d, err = c.AdmitOpenAICapacity(ctx, 43, model, "fresh", 60)
		require.NoError(t, err)
		require.True(t, d.Allowed)
		require.False(t, d.Probe)
	}
}

func TestCapacityCooldownCountsOverloadsAcrossModels(t *testing.T) {
	c, _ := capacityTestStore(t)
	settings := service.DefaultOpenAICapacityCooldownSettings()
	settings.FailureThreshold = 2
	first, err := c.ObserveOpenAICapacity(context.Background(), 42, "model-a", "request-a", 0, 1, settings)
	require.NoError(t, err)
	require.False(t, first.Tripped)
	second, err := c.ObserveOpenAICapacity(context.Background(), 42, "model-b", "request-b", 0, 1, settings)
	require.NoError(t, err)
	require.True(t, second.Tripped)
	require.EqualValues(t, 2, second.Failures)
}

func TestCapacityCooldownResetFencesInflightResultsAcrossModels(t *testing.T) {
	c, _ := capacityTestStore(t)
	opened := capacityTrip(t, c)
	ctx := context.Background()
	require.NoError(t, c.ResetOpenAICapacity(ctx, 42))
	current, err := c.CurrentOpenAICapacity(ctx, 42)
	require.NoError(t, err)
	require.Greater(t, current.Generation, opened.Generation)
	late := capacityRecord(t, c, "late-old-failure", opened.Generation, 1)
	require.False(t, late.Tripped)
	settings := service.DefaultOpenAICapacityCooldownSettings()
	for _, model := range []string{"model-a", "model-b"} {
		d, err := c.AdmitOpenAICapacity(ctx, 42, model, "after-reset", 60)
		require.NoError(t, err)
		require.True(t, d.Allowed)
		require.False(t, d.Probe)
	}
	fresh, err := c.ObserveOpenAICapacity(ctx, 42, "model-b", "new-failure", current.Generation, 1, settings)
	require.NoError(t, err)
	require.True(t, fresh.Tripped, "genuine post-recovery overloads must still pause the account")
}

func TestCapacityCooldownControlLockExcludesOtherInstances(t *testing.T) {
	c, _ := capacityTestStore(t)
	unlock, err := c.LockOpenAICapacity(context.Background(), 42)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = c.LockOpenAICapacity(ctx, 42)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	unlock()
	otherUnlock, err := c.LockOpenAICapacity(context.Background(), 42)
	require.NoError(t, err)
	unlock() // An old owner cannot delete the replacement lease.
	freshCtx, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	_, err = c.LockOpenAICapacity(freshCtx, 42)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	otherUnlock()
}

func TestCapacityCooldownSingleProbeAcrossClientsAndRecovery(t *testing.T) {
	c, server := capacityTestStore(t)
	opened := capacityTrip(t, c)
	ctx := context.Background()
	d, err := c.AdmitOpenAICapacity(ctx, 42, "model-a", "too-early", 60)
	require.NoError(t, err)
	require.False(t, d.Allowed)
	server.SetTime(time.UnixMilli(opened.UntilMS + 1))
	client2 := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client2.Close() })
	other := &tempUnschedCache{rdb: client2}
	var wg sync.WaitGroup
	type admission struct {
		id  string
		d   service.OpenAICapacityDecision
		err error
	}
	results := make(chan admission, 12)
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := c
			if i%2 == 1 {
				store = other
			}
			id := fmt.Sprintf("probe-%d", i)
			d, err := store.AdmitOpenAICapacity(ctx, 42, fmt.Sprintf("model-%d", i%3), id, 60)
			results <- admission{id, d, err}
		}(i)
	}
	wg.Wait()
	close(results)
	winner := ""
	for r := range results {
		require.NoError(t, r.err)
		if r.d.Allowed {
			require.Empty(t, winner)
			winner = r.id
			require.True(t, r.d.Probe)
		}
	}
	require.NotEmpty(t, winner)
	capacityRecord(t, c, winner, opened.Generation, 2)
	for i := range 2 {
		id := fmt.Sprintf("recovery-%d", i)
		d, err = other.AdmitOpenAICapacity(ctx, 42, "model-a", id, 60)
		require.NoError(t, err)
		require.True(t, d.Allowed)
		require.True(t, d.Probe)
		capacityRecord(t, c, id, d.Generation, 2)
	}
	d, err = other.AdmitOpenAICapacity(ctx, 42, "model-a", "normal", 60)
	require.NoError(t, err)
	require.True(t, d.Allowed)
	require.False(t, d.Probe)
}

func TestCapacityCooldownEscalatesAndRejectsStaleSuccess(t *testing.T) {
	c, server := capacityTestStore(t)
	first := capacityTrip(t, c)
	server.SetTime(time.UnixMilli(first.UntilMS + 1))
	d, err := c.AdmitOpenAICapacity(context.Background(), 42, "model-a", "probe", 60)
	require.NoError(t, err)
	require.True(t, d.Probe)
	second := capacityRecord(t, c, "probe", d.Generation, 1)
	require.True(t, second.Tripped)
	require.EqualValues(t, 2, second.Level)
	require.GreaterOrEqual(t, second.UntilMS-first.UntilMS, int64(108_000))
	capacityRecord(t, c, "old-success", first.Generation, 2)
	d, err = c.AdmitOpenAICapacity(context.Background(), 42, "model-a", "new-request", 60)
	require.NoError(t, err)
	require.False(t, d.Allowed)
}

func TestCapacityCooldownProbeLeaseCancellationUsesOwnerCAS(t *testing.T) {
	c, server := capacityTestStore(t)
	opened := capacityTrip(t, c)
	server.SetTime(time.UnixMilli(opened.UntilMS + 1))
	ctx := context.Background()
	_, err := c.AdmitOpenAICapacity(ctx, 42, "model-a", "old-probe", 1)
	require.NoError(t, err)
	server.SetTime(time.UnixMilli(opened.UntilMS + 1002))
	d, err := c.AdmitOpenAICapacity(ctx, 42, "model-a", "new-probe", 60)
	require.NoError(t, err)
	require.True(t, d.Allowed)
	require.NoError(t, c.ReleaseOpenAICapacity(ctx, 42, "model-a", "old-probe", d.Generation))
	capacityRecord(t, c, "old-probe", d.Generation, 2)
	d, err = c.AdmitOpenAICapacity(ctx, 42, "model-a", "another-probe", 60)
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.NoError(t, c.ReleaseOpenAICapacity(ctx, 42, "model-a", "new-probe", d.Generation))
	d, err = c.AdmitOpenAICapacity(ctx, 42, "model-a", "after-cancel", 60)
	require.NoError(t, err)
	require.True(t, d.Allowed)
	require.True(t, d.Probe)
}
