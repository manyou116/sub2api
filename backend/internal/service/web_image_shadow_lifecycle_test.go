//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func webShadowTestParent(t *testing.T) (*adminServiceImpl, *sparkShadowRepoStub, *Account) {
	t.Helper()
	repo := newSparkShadowRepoStub()
	parent := &Account{Name: "parent", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, Concurrency: 20, Priority: 42,
		GroupIDs: []int64{3, 5}, Credentials: map[string]any{"access_token": "parent-token"}}
	require.NoError(t, repo.Create(context.Background(), parent))
	return &adminServiceImpl{accountRepo: repo}, repo, repo.accounts[parent.ID]
}

func TestWebImageShadowCoexistenceAndLifecycle(t *testing.T) {
	ctx := context.Background()
	svc, repo, parent := webShadowTestParent(t)
	web, err := svc.CreateWebImageShadow(ctx, parent.ID, WebImageShadowOptions{})
	require.NoError(t, err)
	require.True(t, web.IsWebImageShadow())
	require.False(t, web.IsSparkShadow())
	require.True(t, web.Schedulable)
	require.Equal(t, 1, web.Concurrency)
	require.Equal(t, parent.Priority, web.Priority)
	require.Equal(t, parent.GroupIDs, web.GroupIDs)
	require.Empty(t, web.GetOpenAIAccessToken())
	spark, err := svc.CreateShadow(ctx, parent.ID, ShadowOptions{})
	require.NoError(t, err)
	require.True(t, spark.IsSparkShadow())
	_, err = svc.CreateShadow(ctx, parent.ID, ShadowOptions{})
	require.Error(t, err)
	_, err = svc.CreateWebImageShadow(ctx, parent.ID, WebImageShadowOptions{})
	require.Error(t, err)
	proxy := int64(9)
	require.NoError(t, svc.propagateProxyToShadows(ctx, parent.ID, &proxy))
	require.Equal(t, &proxy, repo.accounts[web.ID].ProxyID)
	require.Equal(t, &proxy, repo.accounts[spark.ID].ProxyID)
	require.NoError(t, svc.DeleteAccount(ctx, parent.ID))
	require.Empty(t, repo.accounts)
}

func TestWebImageShadowRejectsDuplicateControlsAndMalformedOptions(t *testing.T) {
	ctx := context.Background()
	svc, _, parent := webShadowTestParent(t)
	shadow, err := svc.CreateWebImageShadow(ctx, parent.ID, WebImageShadowOptions{})
	require.NoError(t, err)
	for _, options := range []any{"invalid", map[string]any{"enabled": true}, map[string]any{"max_inflight": 9}, map[string]any{"model_mode": "bad"}, map[string]any{"require_download_attachment": "false"}} {
		extra := map[string]any{"openai_web_images": options}
		_, err := svc.UpdateAccount(ctx, shadow.ID, &UpdateAccountInput{Extra: extra})
		require.Error(t, err)
		_, err = svc.BulkUpdateAccounts(ctx, &BulkUpdateAccountsInput{AccountIDs: []int64{shadow.ID}, Extra: extra})
		require.Error(t, err)
	}
}

func TestWebImageShadowPresenceAndUpdateGuards(t *testing.T) {
	ctx := context.Background()
	svc, repo, parent := webShadowTestParent(t)
	zero, empty := 0, []int64{}
	_, err := svc.CreateWebImageShadow(ctx, parent.ID, WebImageShadowOptions{Concurrency: &zero})
	require.Error(t, err)
	web, err := svc.CreateWebImageShadow(ctx, parent.ID, WebImageShadowOptions{Priority: &zero, GroupIDs: &empty})
	require.NoError(t, err)
	require.Zero(t, web.Priority)
	require.Empty(t, web.GroupIDs)
	for _, input := range []*UpdateAccountInput{
		{Concurrency: &zero}, {Type: AccountTypeAPIKey}, {Credentials: map[string]any{"access_token": "no"}},
	} {
		_, err := svc.UpdateAccount(ctx, web.ID, input)
		require.Error(t, err)
	}
	_, err = svc.BulkUpdateAccounts(ctx, &BulkUpdateAccountsInput{AccountIDs: []int64{web.ID}, Concurrency: &zero})
	require.Error(t, err)
	_, err = svc.UpdateAccount(ctx, parent.ID, &UpdateAccountInput{Type: AccountTypeAPIKey})
	require.Error(t, err)
	_, err = svc.UpdateAccount(ctx, parent.ID, &UpdateAccountInput{Type: AccountTypeSetupToken})
	require.NoError(t, err, "Web parents support setup-token credentials")
	require.Equal(t, AccountTypeSetupToken, repo.accounts[parent.ID].Type)
	resolved, err := resolveCredentialAccount(ctx, repo, web)
	require.NoError(t, err)
	require.Equal(t, parent.ID, resolved.ID)
	require.Equal(t, "parent-token", resolved.GetOpenAIAccessToken())
	_, err = svc.UpdateAccount(ctx, parent.ID, &UpdateAccountInput{Credentials: map[string]any{"auth_mode": OpenAIAuthModeAgentIdentity}})
	require.Error(t, err)
}

func TestWebImageShadowParentCompatibilityAndHealth(t *testing.T) {
	_, _, parent := webShadowTestParent(t)
	for _, typ := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		parent.Type = typ
		require.NoError(t, ValidateWebImageShadowParent(parent))
	}
	future := time.Now().Add(time.Hour)
	parent.TempUnschedulableUntil = &future
	parent.TempUnschedulableReason = "proxy transport failed"
	require.False(t, parent.IsCredentialUsableForWebImages())
	parent.TempUnschedulableReason = OpenAICapacityCooldownReasonPrefix + `"until":1}`
	require.True(t, parent.IsCredentialUsableForWebImages())
	parent.Status = StatusDisabled
	require.False(t, parent.IsCredentialUsableForWebImages())
	parent.Status = StatusActive
	parent.Credentials = map[string]any{}
	require.Error(t, ValidateWebImageShadowParent(parent))
	parent.Credentials = map[string]any{"access_token": "token", "auth_mode": OpenAIAuthModeAgentIdentity}
	require.Error(t, ValidateWebImageShadowParent(parent))
	parent.Credentials = map[string]any{"access_token": "token"}
	parent.ParentAccountID = &parent.ID
	require.Error(t, ValidateWebImageShadowParent(parent))
}

func TestWebImageShadowConversionPreviewApplyAndRecreate(t *testing.T) {
	ctx := context.Background()
	svc, repo, parent := webShadowTestParent(t)
	parent.Priority, parent.GroupIDs = 0, []int64{}
	parent.Extra = map[string]any{"openai_web_images": map[string]any{
		"max_inflight": float64(3), "priority": float64(99), "model": "gpt-5", "model_mode": "fixed", "require_download_attachment": true,
	}}
	before := *parent
	before.Extra, _ = cloneAccountJSONMap(parent.Extra)
	before.Credentials, _ = cloneAccountJSONMap(parent.Credentials)
	_, err := svc.ConvertLegacyWebImageShadow(ctx, parent.ID, WebImageShadowConversionOptions{})
	require.Error(t, err, "inherited enablement requires explicit old global default")
	enabled := true
	opts := WebImageShadowConversionOptions{LegacyDefaultEnabled: &enabled}
	plan, err := svc.ConvertLegacyWebImageShadow(ctx, parent.ID, opts)
	require.NoError(t, err)
	require.Zero(t, plan.ShadowID)
	require.Len(t, repo.accounts, 1)
	require.Equal(t, 3, plan.Concurrency)
	require.Zero(t, plan.Priority)
	require.Empty(t, plan.GroupIDs)
	opts.Apply = true
	plan, err = svc.ConvertLegacyWebImageShadow(ctx, parent.ID, opts)
	require.NoError(t, err)
	require.True(t, plan.Applied)
	shadowID := plan.ShadowID
	require.Equal(t, plan.ModelOptions, repo.accounts[shadowID].Extra["openai_web_images"])
	repeat, err := svc.ConvertLegacyWebImageShadow(ctx, parent.ID, opts)
	require.NoError(t, err)
	require.Equal(t, shadowID, repeat.ShadowID)
	require.False(t, repeat.Applied)
	require.Equal(t, before.Extra, parent.Extra)
	require.Equal(t, before.Credentials, parent.Credentials)
	require.NoError(t, svc.DeleteAccount(ctx, shadowID))
	_, err = svc.CreateWebImageShadow(ctx, parent.ID, WebImageShadowOptions{})
	require.NoError(t, err)
	require.Equal(t, before.Extra, parent.Extra)
}
