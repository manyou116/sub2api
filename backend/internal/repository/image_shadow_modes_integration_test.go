//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestConfiguredImageShadowModes(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newAccountRepositoryWithSQL(client, tx, nil)
	group := mustCreateGroup(t, client, &service.Group{Name: "mode-route-group", Platform: service.PlatformOpenAI})
	parent := &service.Account{Name: "mode-route-parent", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Concurrency: 1}
	require.NoError(t, repo.Create(ctx, parent))
	child := func(dimension, status string) *service.Account {
		a := &service.Account{Name: dimension, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: status,
			Concurrency: 1, ParentAccountID: &parent.ID, QuotaDimension: dimension, Schedulable: false}
		require.NoError(t, repo.Create(ctx, a))
		return a
	}
	web := child(service.QuotaDimensionWebImages, service.StatusError)
	work := child(service.QuotaDimensionWorkImages, service.StatusDisabled)
	require.NoError(t, repo.BindGroups(ctx, work.ID, []int64{group.ID}))
	modes, err := repo.ConfiguredImageShadowModes(ctx, &group.ID, false)
	require.NoError(t, err)
	require.Equal(t, []string{service.ImageShadowModeWork}, modes, "disabled configured mode survives health filtering")
	modes, err = repo.ConfiguredImageShadowModes(ctx, nil, false)
	require.NoError(t, err)
	require.Equal(t, []string{service.ImageShadowModeWeb}, modes, "ungrouped routing ignores other groups")
	modes, err = repo.ConfiguredImageShadowModes(ctx, nil, true)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{service.ImageShadowModeWeb, service.ImageShadowModeWork}, modes)
	require.NoError(t, repo.BindGroups(ctx, web.ID, []int64{group.ID}))
	modes, err = repo.ConfiguredImageShadowModes(ctx, &group.ID, false)
	require.NoError(t, err)
	require.Len(t, modes, 2, "paused and errored modes must not silently disappear")
	require.NoError(t, repo.Delete(ctx, work.ID))
	modes, err = repo.ConfiguredImageShadowModes(ctx, &group.ID, false)
	require.NoError(t, err)
	require.Equal(t, []string{service.ImageShadowModeWeb}, modes, "deleted mode is removed")
}
