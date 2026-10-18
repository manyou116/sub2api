//go:build unit

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

type deliveryRepo struct {
	mockAccountRepoForGemini
	mu            sync.Mutex
	account       *Account
	beforeCAS     func()
	casCalls      int
	failSave      bool
	failSaveAfter int
}

func (r *deliveryRepo) GetByID(context.Context, int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return snapshotOAuthRefreshAccount(r.account), nil
}
func (r *deliveryRepo) UpdateOpenAIOAuthCredentialsIfUnchanged(_ context.Context, id int64, old map[string]any, proxy *int64, next map[string]any) (bool, error) {
	if r.beforeCAS != nil {
		r.beforeCAS()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.casCalls++
	if r.failSave || (r.failSaveAfter > 0 && r.casCalls > r.failSaveAfter) {
		return false, errors.New("db unavailable")
	}
	if r.account.ID != id || !reflect.DeepEqual(old, r.account.Credentials) || !reflect.DeepEqual(proxy, r.account.ProxyID) {
		return false, nil
	}
	r.account.Credentials = shallowCopyMap(next)
	return true, nil
}

type deliveryCache struct {
	refreshAPICacheStub
	mu    sync.Mutex
	owner string
	fail  bool
	lost  bool
}

func (c *deliveryCache) AcquireRefreshLease(context.Context, string, time.Duration) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return "", errors.New("redis down")
	}
	if c.owner != "" {
		return "", nil
	}
	c.owner = "owner"
	return c.owner, nil
}
func (c *deliveryCache) OwnsRefreshLease(context.Context, string, string) (bool, error) {
	return !c.lost, nil
}
func (c *deliveryCache) ReleaseRefreshLease(context.Context, string, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.owner = ""
	return nil
}

type deliveryExecutor struct {
	started chan struct{}
	proceed chan struct{}
	calls   int
	err     error
}

func (e *deliveryExecutor) CacheKey(a *Account) string                { return OpenAITokenCacheKey(a) }
func (e *deliveryExecutor) CanRefresh(*Account) bool                  { return true }
func (e *deliveryExecutor) NeedsRefresh(*Account, time.Duration) bool { return true }
func (e *deliveryExecutor) Refresh(ctx context.Context, a *Account) (map[string]any, error) {
	e.calls++
	if e.started != nil {
		close(e.started)
	}
	if e.proceed != nil {
		select {
		case <-e.proceed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if e.err != nil {
		return nil, e.err
	}
	next := shallowCopyMap(a.Credentials)
	next["refresh_token"] = "rotated"
	next["access_token"] = "rotated-at"
	return next, nil
}
func deliveryTestCredentials() map[string]any {
	claims := map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "client_id": "client", "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}}
	b, _ := json.Marshal(claims)
	return map[string]any{"access_token": "e30." + base64.RawURLEncoding.EncodeToString(b) + ".sig", "refresh_token": "rt", "client_id": "client", "chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}
}
func newDeliveryFixture() (*OAuthRefreshAPI, *deliveryRepo, *deliveryCache) {
	r := &deliveryRepo{account: &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Credentials: deliveryTestCredentials()}}
	c := &deliveryCache{}
	return NewOAuthRefreshAPI(r, c), r, c
}

func TestOpenAIOAuthDeliveryCASAndIdentity(t *testing.T) {
	api, repo, _ := newDeliveryFixture()
	ctx := context.Background()
	first, err := api.ReadOpenAIOAuthCredentials(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, OpenAIOAuthDeliveryCapability, first.Capability)
	next := deliveryTestCredentials()
	next["refresh_token"] = "new-rt"
	account, err := api.MutateOpenAIOAuthCredentials(ctx, 7, first.CredentialRevision, next, nil)
	require.NoError(t, err)
	require.NotEqual(t, first.CredentialRevision, openAICredentialRevision(account))
	require.Equal(t, "new-rt", repo.account.GetOpenAIRefreshToken())
	_, err = api.MutateOpenAIOAuthCredentials(ctx, 7, first.CredentialRevision, next, nil)
	require.Equal(t, "OAUTH_CREDENTIAL_CONFLICT", infraerrors.Reason(err))
	require.Equal(t, 1, repo.casCalls)
	current := openAICredentialRevision(repo.account)
	next["chatgpt_account_id"] = "wrong"
	_, err = api.MutateOpenAIOAuthCredentials(ctx, 7, current, next, nil)
	require.Equal(t, "OAUTH_CREDENTIAL_IDENTITY_INVALID", infraerrors.Reason(err))
	require.Equal(t, 1, repo.casCalls)
}

func TestOpenAIOAuthDeliveryFailClosed(t *testing.T) {
	for _, kind := range []string{"cache_error", "cache_busy", "lease_lost", "cas_conflict", "save_error"} {
		t.Run(kind, func(t *testing.T) {
			api, repo, cache := newDeliveryFixture()
			revision := openAICredentialRevision(repo.account)
			ex := &deliveryExecutor{}
			switch kind {
			case "cache_error":
				cache.fail = true
			case "cache_busy":
				cache.owner = "other"
			case "lease_lost":
				cache.lost = true
			case "cas_conflict":
				repo.beforeCAS = func() { repo.account.Credentials["refresh_token"] = "concurrent" }
			case "save_error":
				repo.failSave = true
			}
			_, err := api.MutateOpenAIOAuthCredentials(context.Background(), 7, revision, nil, ex)
			require.Error(t, err)
			require.NotEqual(t, "rotated", repo.account.GetOpenAIRefreshToken())
			if strings.HasPrefix(kind, "cache_") {
				require.Zero(t, ex.calls)
			}
		})
	}
}

func TestOpenAIOAuthDeliverySharesBackgroundLock(t *testing.T) {
	api, repo, _ := newDeliveryFixture()
	revision := openAICredentialRevision(repo.account)
	ex := &deliveryExecutor{started: make(chan struct{}), proceed: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := api.RefreshIfNeeded(context.Background(), repo.account, ex, time.Minute)
		done <- err
	}()
	<-ex.started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := api.MutateOpenAIOAuthCredentials(ctx, 7, revision, deliveryTestCredentials(), nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	close(ex.proceed)
	require.NoError(t, <-done)
	require.Equal(t, "rotated", repo.account.GetOpenAIRefreshToken())
	_, err = api.MutateOpenAIOAuthCredentials(context.Background(), 7, revision, deliveryTestCredentials(), nil)
	require.Equal(t, "OAUTH_CREDENTIAL_CONFLICT", infraerrors.Reason(err))
}

func TestOpenAIOAuthBackgroundCASRejectsConcurrentReauthorization(t *testing.T) {
	api, repo, _ := newDeliveryFixture()
	repo.beforeCAS = func() { repo.account.Credentials["refresh_token"] = "reauthorized" }
	_, err := api.RefreshIfNeeded(context.Background(), repo.account, &deliveryExecutor{}, time.Minute)
	require.Equal(t, "OAUTH_CREDENTIAL_CONFLICT", infraerrors.Reason(err))
	require.Equal(t, "reauthorized", repo.account.GetOpenAIRefreshToken())
}

func TestOpenAIOAuthDeliveryRefreshErrorClassification(t *testing.T) {
	for _, tc := range []struct{ body, reason string }{{`{"error":"invalid_grant"}`, "OAUTH_REFRESH_TOKEN_INVALID"}, {`{"error":{"code":"refresh_token_reused"}}`, "OAUTH_REFRESH_TOKEN_INVALID"}, {`{"error":{"code":"account_deactivated"}}`, "OAUTH_ACCOUNT_DEACTIVATED"}, {`{"error":"server_error"}`, "OAUTH_REFRESH_UNAVAILABLE"}, {`<html>invalid_grant</html>`, "OAUTH_REFRESH_UNAVAILABLE"}} {
		err := infraerrors.New(502, "OPENAI_OAUTH_TOKEN_REFRESH_FAILED", "token refresh failed: status 400, body: "+tc.body)
		require.Equal(t, tc.reason, infraerrors.Reason(classifyOpenAIDeliveryRefreshError(err)))
	}
	err := infraerrors.New(502, "OPENAI_OAUTH_TOKEN_REFRESH_FAILED", `token refresh failed: status 429, body: {"error":"invalid_grant"}`)
	require.Equal(t, "OAUTH_REFRESH_RATE_LIMITED", infraerrors.Reason(classifyOpenAIDeliveryRefreshError(err)))
	require.Equal(t, "OAUTH_REFRESH_UNAVAILABLE", infraerrors.Reason(classifyOpenAIDeliveryRefreshError(errors.New("timeout invalid_grant"))))
}

func TestOpenAIOAuthDeliveryUnknownRefreshDoesNotRetryConsumedToken(t *testing.T) {
	api, repo, _ := newDeliveryFixture()
	revision := openAICredentialRevision(repo.account)
	ex := &deliveryExecutor{err: errors.New("connection reset after submission")}
	_, err := api.MutateOpenAIOAuthCredentials(context.Background(), 7, revision, nil, ex)
	require.Equal(t, "OAUTH_REFRESH_UNAVAILABLE", infraerrors.Reason(err))
	require.Equal(t, 1, ex.calls)
	require.NotEmpty(t, repo.account.GetCredential("_oauth_refresh_pending"))
	current := openAICredentialRevision(repo.account)
	_, err = api.MutateOpenAIOAuthCredentials(context.Background(), 7, current, nil, ex)
	require.Equal(t, "OAUTH_REFRESH_RECONCILIATION_REQUIRED", infraerrors.Reason(err))
	require.Equal(t, 1, ex.calls)
	replacement := deliveryTestCredentials()
	replacement["refresh_token"] = "reauthorized"
	_, err = api.MutateOpenAIOAuthCredentials(context.Background(), 7, current, replacement, nil)
	require.NoError(t, err)
	require.Empty(t, repo.account.GetCredential("_oauth_refresh_pending"))
}

func TestOpenAIOAuthDeliveryRejectsIdentityModeAndKeepsInputImmutable(t *testing.T) {
	api, repo, _ := newDeliveryFixture()
	repo.account.Credentials["auth_mode"] = "agentIdentity"
	_, err := api.ReadOpenAIOAuthCredentials(context.Background(), 7)
	require.Equal(t, "NOT_OPENAI_OAUTH", infraerrors.Reason(err))
	delete(repo.account.Credentials, "auth_mode")
	next := deliveryTestCredentials()
	copy := shallowCopyMap(next)
	_, err = api.MutateOpenAIOAuthCredentials(context.Background(), 7, openAICredentialRevision(repo.account), next, nil)
	require.NoError(t, err)
	require.Equal(t, copy, next)
}

func TestOpenAIOAuthDeliverySuccessfulRefreshSaveFailureKeepsPending(t *testing.T) {
	api, repo, _ := newDeliveryFixture()
	repo.failSaveAfter = 1
	executor := &deliveryExecutor{}
	_, err := api.MutateOpenAIOAuthCredentials(context.Background(), 7, openAICredentialRevision(repo.account), nil, executor)
	require.Equal(t, "OAUTH_COORDINATION_UNAVAILABLE", infraerrors.Reason(err))
	require.Equal(t, 1, executor.calls)
	require.NotEmpty(t, repo.account.GetCredential("_oauth_refresh_pending"))
	require.Equal(t, "rt", repo.account.GetOpenAIRefreshToken())
	repo.failSaveAfter = 0
	_, err = api.RefreshIfNeeded(context.Background(), repo.account, executor, time.Minute)
	require.Equal(t, "OAUTH_REFRESH_RECONCILIATION_REQUIRED", infraerrors.Reason(err))
	require.Equal(t, 1, executor.calls)
}

func TestOpenAIOAuthDeliveryExplicitRejectionClearsPending(t *testing.T) {
	api, repo, _ := newDeliveryFixture()
	executor := &deliveryExecutor{err: infraerrors.New(502, "OPENAI_OAUTH_TOKEN_REFRESH_FAILED", `token refresh failed: status 400, body: {"error":"invalid_grant"}`)}
	_, err := api.MutateOpenAIOAuthCredentials(context.Background(), 7, openAICredentialRevision(repo.account), nil, executor)
	require.Equal(t, "OAUTH_REFRESH_TOKEN_INVALID", infraerrors.Reason(err))
	require.Empty(t, repo.account.GetCredential("_oauth_refresh_pending"))
}

func TestOpenAIOAuthDeliveryCreateSchedulingDefault(t *testing.T) {
	disabled, enabled := false, true
	for _, tc := range []struct {
		name  string
		value *bool
		want  bool
	}{{"legacy_default", nil, true}, {"delivery_paused", &disabled, false}, {"explicit_enabled", &enabled, true}} {
		t.Run(tc.name, func(t *testing.T) {
			account, err := buildAccountForCreate(&CreateAccountInput{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: deliveryTestCredentials(), Schedulable: tc.value}, nil)
			require.NoError(t, err)
			require.Equal(t, tc.want, account.Schedulable)
		})
	}
}

func TestOpenAIOAuthBackgroundReturnsConcurrentScheduleState(t *testing.T) {
	api, repo, _ := newDeliveryFixture()
	repo.account.Schedulable = true
	executor := &deliveryExecutor{started: make(chan struct{}), proceed: make(chan struct{})}
	done := make(chan *OAuthRefreshResult, 1)
	failed := make(chan error, 1)
	go func() {
		result, err := api.RefreshIfNeeded(context.Background(), repo.account, executor, time.Minute)
		done <- result
		failed <- err
	}()
	<-executor.started
	repo.mu.Lock()
	repo.account.Schedulable = false
	repo.account.Status = StatusDisabled
	repo.mu.Unlock()
	close(executor.proceed)
	result := <-done
	require.NoError(t, <-failed)
	require.NotNil(t, result.Account)
	require.False(t, result.Account.Schedulable)
	require.Equal(t, StatusDisabled, result.Account.Status)
}

func TestOpenAIOAuthDeliveryLegacySettingsDeleteNonSensitiveKeys(t *testing.T) {
	api, repo, _ := newDeliveryFixture()
	repo.account.Credentials["model_mapping"] = map[string]any{"old": "old"}
	repo.account.Credentials["_oauth_refresh_pending"] = "interrupted"
	_, err := api.UpdateOpenAIOAuthCredentialSettings(context.Background(), 7, map[string]any{"base_url": "https://example.invalid"})
	require.NoError(t, err)
	require.NotContains(t, repo.account.Credentials, "model_mapping")
	require.Equal(t, "rt", repo.account.GetOpenAIRefreshToken())
	require.Equal(t, "interrupted", repo.account.GetCredential("_oauth_refresh_pending"))
	require.Equal(t, "workspace", repo.account.GetCredential("chatgpt_account_id"))
}

type deliveryOAuthClient struct{ openaiOAuthClientRefreshStub }

func (*deliveryOAuthClient) RefreshTokenWithClientID(context.Context, string, string, string) (*openai.TokenResponse, error) {
	return &openai.TokenResponse{AccessToken: "issued-at", RefreshToken: "issued-rt", ExpiresIn: 3600}, nil
}

func TestOpenAIOAuthDeliveryPersistsExchangeWithoutWebEnrichment(t *testing.T) {
	api, repo, _ := newDeliveryFixture()
	oauth := NewOpenAIOAuthService(nil, &deliveryOAuthClient{})
	defer oauth.Stop()
	enriched := false
	oauth.SetPrivacyClientFactory(func(string) (*req.Client, error) { enriched = true; return nil, context.DeadlineExceeded })
	updated, err := api.MutateOpenAIOAuthCredentials(context.Background(), 7, openAICredentialRevision(repo.account), nil, NewOpenAITokenRefresher(oauth, repo))
	require.NoError(t, err)
	require.False(t, enriched)
	require.Equal(t, "issued-rt", updated.GetOpenAIRefreshToken())
	require.Equal(t, "issued-rt", repo.account.GetOpenAIRefreshToken())
	require.Empty(t, repo.account.GetCredential("_oauth_refresh_pending"))
}

func TestOpenAIOAuthDeliveryLegacySettingsRetainsValidation(t *testing.T) {
	api, repo, _ := newDeliveryFixture()
	_, err := api.UpdateOpenAIOAuthCredentialSettings(context.Background(), 7, map[string]any{"header_overrides": "invalid"})
	require.Error(t, err)
	require.Zero(t, repo.casCalls)
}
