//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestDisabledThresholdPauseDatabase(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	account := mustCreateAccount(t, tx.Client(), &service.Account{
		Name: "disabled-threshold", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{}, Extra: map[string]any{"auto_pause_7d_disabled": true},
	})
	until := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	reason := service.BuildDetailedAccountSchedulingThresholdReason(service.AccountSchedulingThresholdReasonInput{
		Platform: service.PlatformOpenAI, Window: "7d", ThresholdPercent: 90, UsedPercent: 99, Until: until,
	})
	// A delayed scheduler cannot reinstate the disabled 7d window.
	require.NoError(t, repo.SetTempUnschedulable(ctx, account.ID, until, reason))
	got, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Nil(t, got.TempUnschedulableUntil)
	// Existing persisted state (created before the disable) is reconciled.
	_, err = tx.ExecContext(ctx, "UPDATE accounts SET temp_unschedulable_until=$1,temp_unschedulable_reason=$2 WHERE id=$3", until, reason, account.ID)
	require.NoError(t, err)
	observed, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	changed, err := repo.CompareAndSwapSchedulingThresholdPause(ctx, observed, nil, "")
	require.NoError(t, err)
	require.True(t, changed)
	got, err = repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Nil(t, got.TempUnschedulableUntil)
	var events int
	rows, err := tx.QueryContext(ctx, "SELECT count(*) FROM scheduler_outbox WHERE account_id=$1", account.ID)
	require.NoError(t, err)
	require.True(t, rows.Next())
	require.NoError(t, rows.Scan(&events))
	require.NoError(t, rows.Close())
	require.Positive(t, events)
	// A new unrelated failure wins over the old recovery observation.
	require.NoError(t, repo.SetTempUnschedulable(ctx, account.ID, until, "authentication failed"))
	changed, err = repo.CompareAndSwapSchedulingThresholdPause(ctx, observed, nil, "")
	require.NoError(t, err)
	require.False(t, changed)
	got, err = repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, "authentication failed", got.TempUnschedulableReason)
	// Configuration and usage changes also invalidate a previously read pause.
	for _, update := range []string{
		`extra = extra || '{"auto_pause_7d_disabled":false}'::jsonb`,
		`extra = extra || '{"codex_7d_used_percent":10}'::jsonb`,
		`credentials = '{"account_scheduling_threshold":80}'::jsonb`,
	} {
		_, err = tx.ExecContext(ctx, "UPDATE accounts SET temp_unschedulable_reason=$1 WHERE id=$2", reason, account.ID)
		require.NoError(t, err)
		observed, err = repo.GetByID(ctx, account.ID)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, "UPDATE accounts SET "+update+" WHERE id=$1", account.ID)
		require.NoError(t, err)
		changed, err = repo.CompareAndSwapSchedulingThresholdPause(ctx, observed, nil, "")
		require.NoError(t, err)
		require.False(t, changed)
	}
}
