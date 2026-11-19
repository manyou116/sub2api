//go:build integration

package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestBPSShadowDatabase(t *testing.T) {
	ctx := t.Context()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	parent := &service.Account{Name: "bps-parent", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeOAuth, Status: service.StatusActive, Concurrency: 1}
	require.NoError(t, repo.Create(ctx, parent))
	newChild := func(dimension string) *service.Account {
		return &service.Account{Name: dimension, Platform: service.PlatformOpenAI,
			Type: service.AccountTypeOAuth, Status: service.StatusActive, Concurrency: 1,
			ParentAccountID: &parent.ID, QuotaDimension: dimension}
	}
	for _, mode := range []string{"spark", "web_images", "work_images"} {
		require.NoError(t, repo.Create(ctx, newChild(mode)))
	}
	child := newChild("bps")
	require.NoError(t, repo.Create(ctx, child))
	require.True(t, child.IsBPSShadow())
	_, err := tx.ExecContext(ctx, "SAVEPOINT duplicate_bps")
	require.NoError(t, err)
	require.ErrorContains(t, repo.Create(ctx, newChild("bps")), "accounts_bps_shadow_unique_idx")
	_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT duplicate_bps")
	require.NoError(t, err)
	require.NoError(t, repo.Delete(ctx, child.ID))
	require.NoError(t, repo.Create(ctx, newChild("bps")), "soft delete allows a new binding")
}
