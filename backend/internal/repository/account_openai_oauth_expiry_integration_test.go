//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthExpiryDatabase(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	a := mustCreateAccount(t, tx.Client(), &service.Account{Name: "expiry-cas", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Credentials: map[string]any{"access_token": "old-at", "refresh_token": "old-rt", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}, Extra: map[string]any{"unrelated": "keep"}})
	fresh, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	// The RT changed after the request snapshot. Expiry must never roll it back.
	require.NoError(t, tx.Client().Account.UpdateOneID(a.ID).SetSchedulable(false).SetCredentials(map[string]any{"access_token": "old-at", "refresh_token": "rotated-rt", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}).Exec(ctx))
	marker := &service.OpenAIOAuth401{TokenHash: service.OpenAIOAuthTokenHash("old-at"), Count: 1, FirstAt: time.Now().UnixMilli()}
	until := time.Now().Add(10 * time.Minute)
	applied, err := repo.ExpireOpenAIOAuthToken(ctx, fresh, "old-at", marker, until)
	require.NoError(t, err)
	require.True(t, applied)
	applied, err = repo.ExpireOpenAIOAuthToken(ctx, fresh, "old-at", marker, until)
	require.NoError(t, err)
	require.False(t, applied)
	fresh, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "rotated-rt", fresh.GetOpenAIRefreshToken())
	require.Equal(t, "keep", fresh.Extra["unrelated"])
	require.False(t, fresh.Schedulable)
	require.NotNil(t, fresh.GetCredentialAsTime("expires_at"))
	require.True(t, fresh.GetCredentialAsTime("expires_at").Before(time.Now()))
	applied, err = repo.ClearOpenAIOAuth401Pause(ctx, a.ID, "old-at")
	require.NoError(t, err)
	require.False(t, applied, "a rejected AT cannot clear its own pause")
	require.NoError(t, tx.Client().Account.UpdateOneID(a.ID).SetCredentials(map[string]any{"access_token": "new-at", "refresh_token": "new-rt"}).Exec(ctx))
	applied, err = repo.ExpireOpenAIOAuthToken(ctx, fresh, "old-at", marker, until)
	require.NoError(t, err)
	require.False(t, applied, "a delayed old 401 cannot expire new credentials")
	applied, err = repo.ClearOpenAIOAuth401Pause(ctx, a.ID, "new-at")
	require.NoError(t, err)
	require.True(t, applied)
	fresh, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Nil(t, fresh.TempUnschedulableUntil)
	require.Equal(t, "new-rt", fresh.GetOpenAIRefreshToken())
	require.False(t, fresh.Schedulable)
	// A future quota pause survives both the mark and successful refresh cleanup.
	require.NoError(t, tx.Client().Account.UpdateOneID(a.ID).SetTempUnschedulableUntil(until).SetTempUnschedulableReason("quota pause").Exec(ctx))
	marker2 := &service.OpenAIOAuth401{TokenHash: service.OpenAIOAuthTokenHash("new-at"), Count: 2, FirstAt: marker.FirstAt}
	applied, err = repo.ExpireOpenAIOAuthToken(ctx, fresh, "new-at", marker2, until)
	require.NoError(t, err)
	require.True(t, applied)
	require.NoError(t, tx.Client().Account.UpdateOneID(a.ID).SetCredentials(map[string]any{"access_token": "third-at", "refresh_token": "third-rt"}).Exec(ctx))
	applied, err = repo.ClearOpenAIOAuth401Pause(ctx, a.ID, "third-at")
	require.NoError(t, err)
	require.False(t, applied)
	fresh, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "quota pause", fresh.TempUnschedulableReason)
	marker3 := &service.OpenAIOAuth401{TokenHash: service.OpenAIOAuthTokenHash("third-at"), Count: 3, FirstAt: marker.FirstAt}
	applied, err = repo.ExpireOpenAIOAuthToken(ctx, fresh, "third-at", marker3, until)
	require.NoError(t, err)
	require.True(t, applied)
	fresh, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, service.StatusError, fresh.Status)
	var count int
	err = scanSingleRow(ctx, repo.sql, `SELECT count(*) FROM scheduler_outbox WHERE account_id=$1 AND payload='{"cache_only":true}'::jsonb`, []any{a.ID}, &count)
	require.NoError(t, err)
	require.Equal(t, 4, count)
}
