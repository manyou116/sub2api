//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthRecoveryDatabase(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	until := time.Now().Add(time.Hour)
	a := mustCreateAccount(t, tx.Client(), &service.Account{Name: "oauth-recovery", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: false, Credentials: map[string]any{"access_token": "old", "refresh_token": "old-rt"}, Extra: map[string]any{"unrelated": "preserve"}, TempUnschedulableUntil: &until})
	require.NoError(t, tx.Client().Account.UpdateOneID(a.ID).SetSchedulable(false).SetTempUnschedulableUntil(until).Exec(ctx))
	fresh, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	pending := &service.OpenAIOAuthRecovery{Phase: "pending", TokenHash: "old-hash", Generation: "one"}
	applied, err := repo.TransitionOpenAIOAuthRecovery(ctx, fresh, nil, pending, nil)
	require.NoError(t, err)
	require.True(t, applied)
	// A duplicate/late transition loses CAS and produces no additional outbox row.
	applied, err = repo.TransitionOpenAIOAuthRecovery(ctx, fresh, nil, pending, nil)
	require.NoError(t, err)
	require.False(t, applied)
	page, err := repo.ListOpenAIOAuthRecoveryCandidates(ctx, 0, 100)
	require.NoError(t, err)
	found := false
	for _, item := range page {
		if item.ID == a.ID {
			found = true
		}
	}
	require.True(t, found)
	fresh, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	verify := *pending
	verify.Phase = "verify"
	verify.TokenHash = "new-hash"
	applied, err = repo.TransitionOpenAIOAuthRecovery(ctx, fresh, pending, &verify, map[string]any{"access_token": "new", "refresh_token": "new-rt"})
	require.NoError(t, err)
	require.True(t, applied)
	// A late failure using the original credential document cannot undo rotation.
	applied, err = repo.TransitionOpenAIOAuthRecovery(ctx, fresh, pending, pending, nil)
	require.NoError(t, err)
	require.False(t, applied)
	fresh, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "new-rt", fresh.GetOpenAIRefreshToken())
	applied, err = repo.TransitionOpenAIOAuthRecovery(ctx, fresh, &verify, nil, nil)
	require.NoError(t, err)
	require.True(t, applied)
	fresh, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.NotContains(t, fresh.Extra, service.OpenAIOAuthRecoveryKey)
	require.Equal(t, "preserve", fresh.Extra["unrelated"])
	require.False(t, fresh.Schedulable)
	require.NotNil(t, fresh.TempUnschedulableUntil)
	var count int
	err = scanSingleRow(ctx, repo.sql, `SELECT count(*) FROM scheduler_outbox WHERE account_id=$1 AND payload='{"cache_only":true}'::jsonb`, []any{a.ID}, &count)
	require.NoError(t, err)
	require.Equal(t, 3, count)

	// An admin stop during an upstream rotation must not lose a consumed RT.
	rotating := *pending
	rotating.Phase = "refreshing"
	applied, err = repo.TransitionOpenAIOAuthRecovery(ctx, fresh, nil, &rotating, nil)
	require.NoError(t, err)
	require.True(t, applied)
	fresh, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.NoError(t, tx.Client().Account.UpdateOneID(a.ID).SetStatus(service.StatusError).SetErrorMessage("admin stop").Exec(ctx))
	applied, err = repo.TransitionOpenAIOAuthRecovery(ctx, fresh, &rotating, &verify, nil)
	require.NoError(t, err)
	require.False(t, applied)
	applied, err = repo.TransitionOpenAIOAuthRecovery(ctx, fresh, &rotating, &verify, map[string]any{"access_token": "rotated", "refresh_token": "saved-after-stop"})
	require.NoError(t, err)
	require.True(t, applied)
	fresh, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "saved-after-stop", fresh.GetOpenAIRefreshToken())
	require.Equal(t, service.StatusError, fresh.Status)
	require.Equal(t, "admin stop", fresh.ErrorMessage)
	require.False(t, fresh.Schedulable)
}
