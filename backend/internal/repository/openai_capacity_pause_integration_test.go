//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestCapacityCooldownDBStatusFilterAndCapacity(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newAccountRepositoryWithSQL(client, tx, nil)
	group := mustCreateGroup(t, client, &service.Group{Name: "capacity-pause-inventory", Platform: service.PlatformOpenAI})
	capacity := mustCreateAccount(t, client, &service.Account{Name: "capacity-pause", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Schedulable: true})
	auth := mustCreateAccount(t, client, &service.Account{Name: "authentication-pause", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Schedulable: true})
	require.NoError(t, repo.BindGroups(ctx, capacity.ID, []int64{group.ID}))
	require.NoError(t, repo.BindGroups(ctx, auth.ID, []int64{group.ID}))
	until := time.Now().Add(time.Hour)
	reason := service.OpenAICapacityCooldownReasonPrefix + `"scope":"account","model":"gpt-6.1-sol","error_message":"text overloaded"}`
	require.NoError(t, repo.SetTempUnschedulable(ctx, capacity.ID, until, reason))
	require.NoError(t, repo.SetTempUnschedulable(ctx, auth.ID, until, "token refresh retry exhausted: timeout"))
	filtered, _, err := repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 100}, service.PlatformOpenAI, "", "temp_unschedulable", "", group.ID, "")
	require.NoError(t, err)
	require.Len(t, filtered, 2, "the existing admin filter must include capacity pauses")
	text, err := repo.ListSchedulableByGroupIDAndPlatform(ctx, group.ID, service.PlatformOpenAI)
	require.NoError(t, err)
	require.Empty(t, text)
	rows, err := repo.ListSchedulableCapacityByGroupIDs(ctx, []int64{group.ID})
	require.NoError(t, err)
	require.Empty(t, rows, "paused accounts must not contribute schedulable capacity")
}

func TestCapacityCooldownDBKeepsStricterAuthAndTransportPauses(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newAccountRepositoryWithSQL(client, tx, nil)
	reason := service.OpenAICapacityCooldownReasonPrefix + `"error_message":"text capacity"}`
	for _, order := range []string{"auth-first", "capacity-first"} {
		t.Run(order, func(t *testing.T) {
			account := mustCreateAccount(t, client, &service.Account{Name: order, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Schedulable: true})
			long := time.Now().Add(time.Hour)
			short := time.Now().Add(time.Minute)
			if order == "auth-first" {
				require.NoError(t, repo.SetTempUnschedulable(ctx, account.ID, short, "transport unavailable"))
				require.NoError(t, repo.SetTempUnschedulable(ctx, account.ID, long, reason))
			} else {
				require.NoError(t, repo.SetTempUnschedulable(ctx, account.ID, long, reason))
				require.NoError(t, repo.SetTempUnschedulable(ctx, account.ID, short, "transport unavailable"))
			}
			got, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			require.Equal(t, "transport unavailable", got.TempUnschedulableReason)
			require.WithinDuration(t, long, *got.TempUnschedulableUntil, time.Millisecond)
			require.False(t, got.IsSchedulable())
		})
	}
}
