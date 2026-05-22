//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSelectKiroAccount_DoesNotFilterCappedUsageSnapshot(t *testing.T) {
	account := Account{
		ID:          101,
		Name:        "kiro-capped-by-snapshot",
		Platform:    PlatformKiro,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Priority:    10,
		Extra: map[string]any{
			"kiro_usage_data": map[string]any{
				"usageBreakdownList": []any{
					map[string]any{
						"currentUsage": 100.0,
						"usageLimit":   100.0,
						"unit":         "INVOCATIONS",
						"overageConfiguration": map[string]any{
							"overageEnabled": false,
						},
					},
				},
			},
		},
	}

	service := &OpenAIGatewayService{
		accountRepo: &mockAccountRepoForPlatform{accounts: []Account{account}},
	}

	selection, err := service.SelectKiroAccount(context.Background(), nil, nil, "")
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, account.ID, selection.Account.ID)
	require.True(t, selection.Acquired)
}
