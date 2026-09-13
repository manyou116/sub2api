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
		Concurrency: 1,
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

	svc := &OpenAIGatewayService{
		accountRepo: &mockAccountRepoForPlatform{accounts: []Account{account}},
	}

	selection, err := svc.SelectKiroAccount(context.Background(), nil, "", nil, "")
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, account.ID, selection.Account.ID)
}

func TestKiroAccountEligible_ModelQuarantineUsesMappedInternalID(t *testing.T) {
	t.Cleanup(func() {
		ClearKiroQuarantine(201)
		ClearKiroQuarantine(202)
	})

	acct := &Account{
		ID:          201,
		Platform:    PlatformKiro,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
	}
	// Client asks for gpt-5.4 → MapKiroModel → claude-opus-4.6
	mapped := MapKiroModel("gpt-5.4")
	require.Equal(t, "claude-opus-4.6", mapped)
	HitModelCapacity(acct.ID, mapped)

	svc := &OpenAIGatewayService{}
	require.False(t, svc.kiroAccountEligible(acct, nil, "gpt-5.4"),
		"quarantine on mapped id must block client alias")
	require.True(t, svc.kiroAccountEligible(acct, nil, "claude-sonnet-4.5"),
		"other models on same account remain eligible")

	other := &Account{
		ID:          202,
		Platform:    PlatformKiro,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
	}
	require.True(t, svc.kiroAccountEligible(other, nil, "gpt-5.4"),
		"same model on other accounts is not quarantined")
}

func TestKiroAccountEligible_WhitelistAcceptsClientOrMapped(t *testing.T) {
	acct := &Account{
		ID:          301,
		Platform:    PlatformKiro,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"claude-sonnet-4.5": "claude-sonnet-4.5",
			},
		},
	}
	svc := &OpenAIGatewayService{}
	require.True(t, svc.kiroAccountEligible(acct, nil, "claude-sonnet-4.5"))
	require.False(t, svc.kiroAccountEligible(acct, nil, "claude-opus-4.7"))
}

// kiroBusyHighPrioCache makes account 401 always busy so pool fallthrough can be asserted.
type kiroBusyHighPrioCache struct {
	stubConcurrencyCacheForTest
}

func (c *kiroBusyHighPrioCache) AcquireAccountSlot(_ context.Context, accountID int64, _ int, _ string) (bool, error) {
	return accountID != 401, nil
}

func TestSelectKiroAccount_PoolFallthroughWhenTopBusy(t *testing.T) {
	busy := Account{
		ID:          401,
		Name:        "busy-high-prio",
		Platform:    PlatformKiro,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Priority:    100,
		Concurrency: 1,
	}
	free := Account{
		ID:          402,
		Name:        "free-low-prio",
		Platform:    PlatformKiro,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Priority:    1,
		Concurrency: 1,
	}

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{busy, free},
		accountsByID: map[int64]*Account{
			401: &busy,
			402: &free,
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo:        repo,
		concurrencyService: NewConcurrencyService(&kiroBusyHighPrioCache{}),
	}

	selection, err := svc.SelectKiroAccount(context.Background(), nil, "", nil, "")
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(402), selection.Account.ID)
	require.True(t, selection.Acquired)
}

func TestKiroProxyURL(t *testing.T) {
	require.Equal(t, "", (*Account)(nil).KiroProxyURL())
	require.Equal(t, "", (&Account{}).KiroProxyURL())
	acct := &Account{Proxy: &Proxy{Protocol: "http", Host: "127.0.0.1", Port: 7890}}
	require.Equal(t, "http://127.0.0.1:7890", acct.KiroProxyURL())
}
