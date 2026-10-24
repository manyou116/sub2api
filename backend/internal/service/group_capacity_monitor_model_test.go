package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func monitorTestAccount(now time.Time, id int64) CapacityMonitorAccount {
	return CapacityMonitorAccount{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, QuotaDimension: "global", Status: StatusActive, Schedulable: true, IdentityKey: time.Unix(id, 0).String(), Plan: "plus|1",
		Window5Cost: 5, Window7Cost: 20, Extra: map[string]any{"codex_usage_updated_at": now.Add(-10 * time.Minute).Format(time.RFC3339), "codex_5h_used_percent": 50., "codex_7d_used_percent": 20., "codex_5h_window_minutes": 300., "codex_7d_window_minutes": 10080., "codex_5h_reset_at": now.Add(time.Hour).Format(time.RFC3339), "codex_7d_reset_at": now.Add(24 * time.Hour).Format(time.RFC3339)}}
}

func TestCapacityMonitorDualWindowsAndHalfOpenEndpoint(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	a := monitorTestAccount(now, 1)
	inventory, quality := CapacityMonitorInventory{}, CapacityMonitorQuality{}
	events := monitorCapacityEvents([]CapacityMonitorAccount{a}, now, 15*24*time.Hour, &inventory, &quality)
	d, e := monitorCapacityAt(events, now, now)
	require.Equal(t, 5., d+e)
	d, e = monitorCapacityAt(events, now.Add(time.Hour), now)
	require.Equal(t, 5., d+e, "reset exactly at endpoint must be excluded")
	d, e = monitorCapacityAt(events, now.Add(time.Hour+time.Nanosecond), now)
	require.Equal(t, 15., d+e)
	d, e = monitorCapacityAt(events, now.Add(24*time.Hour), now)
	require.Equal(t, 55., d+e)
	require.Equal(t, 1, inventory.DirectCount)
}

func TestCapacityMonitorShortHorizonKeepsFullMilestoneSupply(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	r := buildCapacityMonitorResponse(24, &GroupCapacityMonitorSnapshot{AsOf: now, Accounts: []CapacityMonitorAccount{monitorTestAccount(now, 1)}, SharedUsage: []CapacityMonitorSharedUsage{{Cost: 100, BucketStart: now.Add(-time.Hour)}, {Cost: 200, BucketStart: now.Add(-48 * time.Hour)}}}, 24*time.Hour, 24*time.Hour)
	require.Len(t, r.Scenarios, 3)
	require.Greater(t, *r.Scenarios[0].Milestones[5].Capacity, *r.Scenarios[0].Milestones[2].Capacity)
	require.Equal(t, 24., r.CapacityPoints[len(r.CapacityPoints)-1].Hours)
	require.Len(t, r.SharedUsage, 1)
	require.Equal(t, 100., r.SharedUsage[0].Cost)
}

func TestCapacityMonitorExcludesUnprovableOrInvalidSamples(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	for _, mutate := range []func(*CapacityMonitorAccount){
		func(a *CapacityMonitorAccount) { a.IdentityKey = "" },
		func(a *CapacityMonitorAccount) { delete(a.Extra, "codex_5h_used_percent") },
		func(a *CapacityMonitorAccount) { a.Extra["codex_7d_used_percent"] = "NaN" },
		func(a *CapacityMonitorAccount) {
			a.Extra["codex_usage_updated_at"] = now.Add(-3 * time.Hour).Format(time.RFC3339)
		},
		func(a *CapacityMonitorAccount) { a.Extra["codex_5h_window_minutes"] = 0 },
		func(a *CapacityMonitorAccount) { id := int64(2); a.ParentID = &id },
		func(a *CapacityMonitorAccount) { a.DuplicateIdentity = true },
		func(a *CapacityMonitorAccount) {
			until := now.Add(time.Hour)
			a.TempUntil = &until
			a.TempReason = "unknown_auth_pause"
		},
	} {
		a := monitorTestAccount(now, 1)
		mutate(&a)
		inventory, quality := CapacityMonitorInventory{}, CapacityMonitorQuality{}
		require.Empty(t, monitorCapacityEvents([]CapacityMonitorAccount{a}, now, time.Hour, &inventory, &quality))
		require.Zero(t, inventory.ModeledCount)
	}
}

func TestCapacityMonitorDuplicateIdentityExcludedAndPausedCount(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	a, b := monitorTestAccount(now, 1), monitorTestAccount(now, 2)
	b.IdentityKey = a.IdentityKey
	inventory, quality := CapacityMonitorInventory{}, CapacityMonitorQuality{}
	require.Empty(t, monitorCapacityEvents([]CapacityMonitorAccount{a, b}, now, time.Hour, &inventory, &quality))
	require.Equal(t, 1, inventory.DuplicateCount)
	pause := now.Add(2 * time.Hour)
	a.TempUntil = &pause
	a.TempReason = OpenAICapacityCooldownReasonPrefix
	inventory = CapacityMonitorInventory{}
	events := monitorCapacityEvents([]CapacityMonitorAccount{a}, now, 5*time.Hour, &inventory, &quality)
	require.Zero(t, inventory.SchedulableCount)
	require.NotEmpty(t, events)
	require.Equal(t, pause, events[0].at)
}

func TestCapacityMonitorPostResetCostsAndMedianFallback(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	accounts := []CapacityMonitorAccount{}
	for id := int64(1); id <= 4; id++ {
		accounts = append(accounts, monitorTestAccount(now, id))
	}
	accounts[3].Window5Cost = 0
	accounts[3].Extra["codex_5h_used_percent"] = 0.
	inventory, quality := CapacityMonitorInventory{}, CapacityMonitorQuality{}
	events := monitorCapacityEvents(accounts, now, time.Hour, &inventory, &quality)
	d, e := monitorCapacityAt(events, now, now)
	require.Equal(t, 15., d)
	require.Equal(t, 10., e)
	require.Equal(t, 1, inventory.EstimatedCount)
	a := monitorTestAccount(now, 1)
	a.Extra["codex_5h_reset_at"] = now.Add(-5 * time.Minute).Format(time.RFC3339)
	a.Post5Cost = 2
	a.Post7Cost = 7
	inventory = CapacityMonitorInventory{}
	events = monitorCapacityEvents([]CapacityMonitorAccount{a}, now, time.Hour, &inventory, &quality)
	d, e = monitorCapacityAt(events, now, now)
	require.Equal(t, 8., d+e)
}

func TestCapacityMonitorDemandUsesOnlyCompleteHoursAndCrossing(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 30, 0, 0, time.UTC)
	end := now.Truncate(time.Hour)
	history := []CapacityMonitorHistory{}
	for i := 168; i > 0; i-- {
		at := end.Add(-time.Duration(i) * time.Hour)
		history = append(history, CapacityMonitorHistory{StartAt: at, EndAt: at.Add(time.Hour), Complete: true, PoolAccountCost: 2, PoolRequests: 1})
	}
	history = append(history, CapacityMonitorHistory{StartAt: end, EndAt: now, PoolAccountCost: 1000, PoolRequests: 1000})
	bases := monitorDemandBases(history, end)
	require.Equal(t, 48., bases[0].Cost)
	require.Equal(t, 2., *bases[0].RatePerHour)
	s := monitorScenario([]monitorEvent{{at: now, direct: 100}}, now, time.Hour, 1, CapacityMonitorDemand{Available: true, RatePerHour: monitorNumber(1)})
	require.Equal(t, 99., *s.MinimumGap)
	require.Nil(t, s.FirstShortfallAt)
	s = monitorScenario([]monitorEvent{{at: now, direct: 1}, {at: now.Add(2 * time.Hour), direct: 100}}, now, 3*time.Hour, 1, CapacityMonitorDemand{Available: true, RatePerHour: monitorNumber(1)})
	require.Equal(t, now.Add(time.Hour), *s.FirstShortfallAt)
}

type monitorRepoProbe struct{ calls int }

func (r *monitorRepoProbe) ReadSnapshot(context.Context, int64, time.Duration) (*GroupCapacityMonitorSnapshot, error) {
	r.calls++
	return nil, ErrCapacityMonitorNotFound
}

func TestCapacityMonitorConstructsLazilyAndOnlyGetReadsRepository(t *testing.T) {
	repo := &monitorRepoProbe{}
	svc := NewGroupCapacityMonitorService(repo)
	require.Zero(t, repo.calls)
	for _, params := range []struct {
		id               int64
		history, horizon time.Duration
	}{{0, 24 * time.Hour, 24 * time.Hour}, {24, 0, 24 * time.Hour}, {24, 16 * 24 * time.Hour, 24 * time.Hour}, {24, 24 * time.Hour, 0}, {24, 24 * time.Hour, 16 * 24 * time.Hour}} {
		_, err := svc.Get(context.Background(), params.id, params.history, params.horizon)
		require.Error(t, err)
		require.Zero(t, repo.calls)
	}
	_, err := svc.Get(context.Background(), 24, 24*time.Hour, 24*time.Hour)
	require.ErrorIs(t, err, ErrCapacityMonitorNotFound)
	require.Equal(t, 1, repo.calls)
}
