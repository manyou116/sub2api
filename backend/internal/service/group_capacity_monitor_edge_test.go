package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCapacityMonitorExpiryStopsFutureContribution(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	a := monitorTestAccount(now, 1)
	expires := now.Add(30 * time.Minute)
	a.AutoPauseOnExpired, a.ExpiresAt = true, &expires
	i, q := CapacityMonitorInventory{}, CapacityMonitorQuality{}
	events := monitorCapacityEvents([]CapacityMonitorAccount{a}, now, 15*24*time.Hour, &i, &q)
	d, e := monitorCapacityAt(events, now.Add(15*24*time.Hour), now)
	require.Equal(t, 5., d+e, "future resets cannot release usable capacity after account expiry")
	for _, event := range events {
		require.True(t, event.at.Before(expires))
	}
	a.ExpiresAt = &now
	i = CapacityMonitorInventory{}
	require.Empty(t, monitorCapacityEvents([]CapacityMonitorAccount{a}, now, time.Hour, &i, &q))
	require.Zero(t, i.ModeledCount)
}

func TestCapacityMonitorBothExhaustedWaitForBothWindows(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	a := monitorTestAccount(now, 1)
	a.Extra["codex_5h_used_percent"], a.Extra["codex_7d_used_percent"] = 100., 100.
	a.Extra["codex_7d_reset_at"] = now.Add(2 * time.Hour).Format(time.RFC3339)
	a.Window5Cost, a.Window7Cost = 10, 100
	i, q := CapacityMonitorInventory{}, CapacityMonitorQuality{}
	events := monitorCapacityEvents([]CapacityMonitorAccount{a}, now, 10*time.Hour, &i, &q)
	require.NotEmpty(t, events)
	require.Equal(t, now.Add(2*time.Hour), events[0].at)
	d, e := monitorCapacityAt(events, now.Add(2*time.Hour), now)
	require.Zero(t, d+e)
	d, e = monitorCapacityAt(events, now.Add(2*time.Hour+time.Nanosecond), now)
	require.Equal(t, 10., d+e)
	// The idle 5h bucket started when the week window released and was used,
	// not at the unused 5h reset one hour earlier.
	require.Equal(t, now.Add(7*time.Hour), events[1].at)
}

func TestCapacityMonitorZeroPercentWithoutSamplesRemainsUnknown(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	a := monitorTestAccount(now, 1)
	a.Extra["codex_5h_used_percent"], a.Extra["codex_7d_used_percent"] = 0., 0.
	a.Window5Cost, a.Window7Cost = 0, 0
	i, q := CapacityMonitorInventory{}, CapacityMonitorQuality{}
	require.Empty(t, monitorCapacityEvents([]CapacityMonitorAccount{a}, now, 24*time.Hour, &i, &q))
	require.Zero(t, i.ModeledCount)
	points := monitorCapacityPoints(nil, now, time.Hour, 0)
	require.Nil(t, points[0].Capacity, "unknown capacity must not be presented as zero or full")
}
