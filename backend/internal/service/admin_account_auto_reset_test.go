package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBulkUpdateAccountsAutoResetPreservesUnselectedWindow(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
			1: {ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			2: {ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
		}}
		result, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
			AccountIDs: []int64{1, 2},
			Extra: map[string]any{
				OpenAIAutoResetCredit5hEnabledExtraKey:   enabled,
				OpenAIAutoResetCredit5hThresholdExtraKey: 0.755,
				OpenAIAutoResetCreditStateExtraKey:       map[string]any{"status": "success"},
			},
		})
		require.NoError(t, err)
		require.Equal(t, 2, result.Success)
		require.Len(t, repo.bulkUpdates, 1)
		require.Equal(t, map[string]any{
			OpenAIAutoResetCredit5hEnabledExtraKey:   enabled,
			OpenAIAutoResetCredit5hThresholdExtraKey: 0.755,
		}, repo.bulkUpdates[0].Extra)
	}
}

func TestBulkUpdateAccountsAutoResetRejectsIneligibleTargetsBeforeWrite(t *testing.T) {
	parentID := int64(1)
	for _, target := range []*Account{
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeSetupToken},
		{ID: 2, Platform: PlatformGrok, Type: AccountTypeOAuth},
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parentID},
	} {
		repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
			1: {ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			2: target,
		}}
		_, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
			AccountIDs: []int64{1, 2},
			Extra:      map[string]any{OpenAIAutoResetCredit7dEnabledExtraKey: true},
		})
		require.Error(t, err)
		require.Empty(t, repo.bulkUpdates)
	}
}

func TestBulkUpdateAccountsAutoResetRejectsMissingTargetBeforeWrite(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		1: {ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
	}}
	_, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{1, 2},
		Extra:      map[string]any{OpenAIAutoResetCredit7dThresholdExtraKey: 0.95},
	})
	require.ErrorIs(t, err, ErrAccountNotFound)
	require.Empty(t, repo.bulkUpdates)
}

func TestBulkUpdateAccountsAutoResetRejectsInvalidThresholdBeforeWrite(t *testing.T) {
	for _, value := range []any{0, 0.0009, 1.01, true, "invalid", nil} {
		repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
			1: {ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
		}}
		_, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
			AccountIDs: []int64{1},
			Extra:      map[string]any{OpenAIAutoResetCredit7dThresholdExtraKey: value},
		})
		require.Error(t, err, "value=%v", value)
		require.Empty(t, repo.bulkUpdates)
	}
}

func TestBulkUpdateAccountsRateLimitReconcileWritesAccountOptIn(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		1: {ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
		2: {ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
	}}
	result, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{1, 2},
		Extra:      map[string]any{OpenAIRateLimitReconcileEnabledExtraKey: true},
	})
	require.NoError(t, err)
	require.Equal(t, 2, result.Success)
	require.Len(t, repo.bulkUpdates, 1)
	require.Equal(t, map[string]any{OpenAIRateLimitReconcileEnabledExtraKey: true, OpenAIRateLimitReconcileStateExtraKey: nil}, repo.bulkUpdates[0].Extra)
}

func TestBulkUpdateAccountsRateLimitReconcileRejectsInvalidTargets(t *testing.T) {
	parentID := int64(1)
	for _, target := range []*Account{
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		{ID: 2, Platform: PlatformGrok, Type: AccountTypeOAuth},
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parentID},
	} {
		repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
			1: {ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			2: target,
		}}
		_, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
			AccountIDs: []int64{1, 2},
			Extra:      map[string]any{OpenAIRateLimitReconcileEnabledExtraKey: true},
		})
		require.Error(t, err)
		require.Empty(t, repo.bulkUpdates)
	}

	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		1: {ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
	}}
	_, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{1},
		Extra:      map[string]any{OpenAIRateLimitReconcileEnabledExtraKey: "true"},
	})
	require.Error(t, err)
	require.Empty(t, repo.bulkUpdates)
}
