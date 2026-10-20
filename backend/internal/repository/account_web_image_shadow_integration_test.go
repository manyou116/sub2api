//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestWebImageShadowDatabase(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newAccountRepositoryWithSQL(client, tx, nil)
	group := mustCreateGroup(t, client, &service.Group{Name: "web-shadow-database", Platform: service.PlatformOpenAI})
	parent := &service.Account{Name: "web-shadow-parent", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeOAuth, Status: service.StatusActive, Concurrency: 10,
		Credentials: map[string]any{"access_token": "integration-parent-token"}}
	require.NoError(t, repo.Create(ctx, parent))
	require.NoError(t, repo.BindGroups(ctx, parent.ID, []int64{group.ID}))
	newChild := func(dimension string, concurrency int) *service.Account {
		return &service.Account{Name: "child-" + dimension, Platform: service.PlatformOpenAI,
			Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true,
			ParentAccountID: &parent.ID, QuotaDimension: dimension, Concurrency: concurrency,
			Credentials: map[string]any{"model_mapping": map[string]any{"gpt-image-2": "gpt-image-2"}}}
	}
	spark, web := newChild(service.QuotaDimensionSpark, 3), newChild(service.QuotaDimensionWebImages, 2)
	require.NoError(t, repo.Create(ctx, spark))
	require.NoError(t, repo.Create(ctx, web), "migration 246 must allow a Web child alongside Spark")
	for _, child := range []*service.Account{spark, web} {
		require.NoError(t, repo.BindGroups(ctx, child.ID, []int64{group.ID}))
	}
	children, err := repo.ListShadowsByParent(ctx, parent.ID)
	require.NoError(t, err)
	require.Len(t, children, 2)
	childIDs := []int64{children[0].ID, children[1].ID}
	require.ElementsMatch(t, []int64{spark.ID, web.ID}, childIDs)

	// Expected PostgreSQL errors must not abort the surrounding test transaction.
	expectConstraint := func(name string, write func() error) {
		t.Helper()
		_, err := tx.ExecContext(ctx, "SAVEPOINT web_shadow_constraint")
		require.NoError(t, err)
		writeErr := write()
		_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT web_shadow_constraint")
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, "RELEASE SAVEPOINT web_shadow_constraint")
		require.NoError(t, err)
		require.ErrorContains(t, writeErr, name)
	}
	expectConstraint("uq_accounts_web_image_shadow_per_parent", func() error {
		return repo.Create(ctx, newChild(service.QuotaDimensionWebImages, 1))
	})
	for _, invalid := range []int{0, -1} {
		expectConstraint("chk_accounts_web_image_shadow_concurrency", func() error {
			_, err := tx.ExecContext(ctx, "UPDATE accounts SET concurrency = $1 WHERE id = $2", invalid, web.ID)
			return err
		})
	}

	capacity := func() map[int64]service.GroupAccountCapacityRow {
		t.Helper()
		rows, err := repo.ListSchedulableCapacityByGroupIDs(ctx, []int64{group.ID})
		require.NoError(t, err, "execute the production capacity SQL against PostgreSQL")
		byID := make(map[int64]service.GroupAccountCapacityRow, len(rows))
		for _, row := range rows {
			require.Equal(t, group.ID, row.GroupID)
			byID[row.AccountID] = row
		}
		return byID
	}
	rows := capacity()
	require.Len(t, rows, 2, "a manually paused parent must not pause either child")
	require.Equal(t, service.QuotaDimensionSpark, rows[spark.ID].QuotaDimension)
	require.Equal(t, service.QuotaDimensionWebImages, rows[web.ID].QuotaDimension)
	require.Equal(t, 2, rows[web.ID].Concurrency)
	require.NoError(t, repo.SetSchedulable(ctx, spark.ID, false))
	rows = capacity()
	require.Len(t, rows, 1)
	require.Contains(t, rows, web.ID, "only the image-purpose child contributes capacity")

	future := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	_, err = client.Account.UpdateOneID(parent.ID).SetRateLimitResetAt(future).SetOverloadUntil(future).Save(ctx)
	require.NoError(t, err)
	require.Contains(t, capacity(), web.ID, "ordinary images ignore parent text quota and overload")
	_, err = client.Account.UpdateOneID(parent.ID).SetTempUnschedulableUntil(future).
		SetTempUnschedulableReason(service.OpenAICapacityCooldownReasonPrefix + `"error_message":"text capacity"}`).Save(ctx)
	require.NoError(t, err)
	require.Contains(t, capacity(), web.ID, "the recognized text capacity pause does not block images")
	_, err = client.Account.UpdateOneID(parent.ID).SetTempUnschedulableReason("authentication failed").Save(ctx)
	require.NoError(t, err)
	require.Empty(t, capacity(), "shared authentication failure blocks the image child")
	_, err = client.Account.UpdateOneID(parent.ID).ClearTempUnschedulableUntil().ClearTempUnschedulableReason().
		SetStatus("inactive").Save(ctx)
	require.NoError(t, err)
	require.Empty(t, capacity(), "an inactive parent must not contribute child capacity")
	_, err = client.Account.UpdateOneID(parent.ID).SetStatus(service.StatusActive).Save(ctx)
	require.NoError(t, err)
	require.Contains(t, capacity(), web.ID)

	require.NoError(t, repo.SetWebImageCooldown(ctx, parent.ID, future))
	require.Empty(t, capacity(), "parent image cooldown excludes the child")
	require.NoError(t, repo.Delete(ctx, web.ID))
	deletedRows, err := tx.QueryContext(ctx, "SELECT deleted_at IS NOT NULL FROM accounts WHERE id = $1", web.ID)
	require.NoError(t, err)
	require.True(t, deletedRows.Next())
	var softDeleted bool
	require.NoError(t, deletedRows.Scan(&softDeleted))
	require.NoError(t, deletedRows.Close())
	require.True(t, softDeleted, "deletion must retain the historical account row")
	recreated := newChild(service.QuotaDimensionWebImages, 4)
	require.NoError(t, repo.Create(ctx, recreated), "partial uniqueness must permit recreation after soft deletion")
	require.NotEqual(t, web.ID, recreated.ID)
	require.NoError(t, repo.BindGroups(ctx, recreated.ID, []int64{group.ID}))
	until, err := repo.WebImageCooldown(ctx, parent.ID)
	require.NoError(t, err)
	require.NotNil(t, until)
	require.WithinDuration(t, future, *until, time.Microsecond)
	require.Empty(t, capacity(), "recreating the shadow must not reset its parent's image cooldown")

	_, err = tx.ExecContext(ctx, "UPDATE accounts SET web_image_rate_limit_reset_at = $1 WHERE id = $2", time.Now().Add(-time.Minute), parent.ID)
	require.NoError(t, err)
	rows = capacity()
	require.Len(t, rows, 1)
	require.Equal(t, 4, rows[recreated.ID].Concurrency)
	_, err = client.Account.UpdateOneID(parent.ID).SetDeletedAt(time.Now()).Save(ctx)
	require.NoError(t, err)
	require.Empty(t, capacity(), "a deleted parent must not leave schedulable child capacity")
}
