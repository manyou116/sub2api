//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type expiryRepo struct {
	AccountRepository
	mu    sync.Mutex
	a     *Account
	marks int
}

func expiryClone(a *Account) *Account {
	encoded, _ := json.Marshal(a)
	var out Account
	_ = json.Unmarshal(encoded, &out)
	return &out
}
func (r *expiryRepo) GetByID(context.Context, int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return expiryClone(r.a), nil
}
func (r *expiryRepo) UpdateCredentials(_ context.Context, _ int64, credentials map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.a.Credentials = shallowCopyMap(credentials)
	return nil
}
func (r *expiryRepo) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.a.TempUnschedulableUntil, r.a.TempUnschedulableReason = &until, reason
	return nil
}
func (r *expiryRepo) SetError(_ context.Context, _ int64, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.a.Status, r.a.ErrorMessage = StatusError, reason
	return nil
}
func (r *expiryRepo) ExpireOpenAIOAuthToken(_ context.Context, a *Account, token string, marker *OpenAIOAuth401, until time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.a.IsActive() || r.a.GetOpenAIAccessToken() != token || !reflect.DeepEqual(openAIOAuth401(r.a), openAIOAuth401(a)) {
		return false, nil
	}
	r.a.Credentials["expires_at"] = time.Now().Add(-time.Second).Format(time.RFC3339Nano)
	r.a.Extra[OpenAIOAuth401Key] = *marker
	if r.a.TempUnschedulableUntil == nil || r.a.TempUnschedulableUntil.Before(time.Now()) || strings.HasPrefix(r.a.TempUnschedulableReason, "OAuth 401:") {
		r.a.TempUnschedulableUntil, r.a.TempUnschedulableReason = &until, OpenAIOAuth401Reason
	}
	if marker.Count >= 3 {
		r.a.Status = StatusError
	}
	r.marks++
	return true, nil
}
func (r *expiryRepo) ClearOpenAIOAuth401Pause(_ context.Context, _ int64, token string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.a.GetOpenAIAccessToken() != token || openAIOAuthTokenRejected(r.a) || r.a.TempUnschedulableReason != OpenAIOAuth401Reason {
		return false, nil
	}
	r.a.TempUnschedulableUntil, r.a.TempUnschedulableReason = nil, ""
	return true, nil
}

type expiryHTTP struct {
	status int
	body   string
}

func (h *expiryHTTP) Do(*http.Request, string, int64, int) (*http.Response, error) {
	return &http.Response{StatusCode: h.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(h.body))}, nil
}
func (h *expiryHTTP) DoWithTLS(r *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return h.Do(r, p, id, n)
}

type expiryExecutor struct {
	*OpenAITokenRefresher
	calls     atomic.Int32
	err       error
	sameAT    bool
	onRefresh func()
}

func (e *expiryExecutor) Refresh(_ context.Context, a *Account) (map[string]any, error) {
	e.calls.Add(1)
	if e.onRefresh != nil {
		e.onRefresh()
	}
	if e.err != nil {
		return nil, e.err
	}
	at := "new-at"
	if e.sameAT {
		at = a.GetOpenAIAccessToken()
	}
	return MergeCredentials(a.Credentials, map[string]any{"access_token": at, "refresh_token": "new-rt", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}), nil
}

const expiryBody = `{"error":{"code":"token_expired","message":"Your authentication token has expired. Please try refreshing it."}}`

func expiryFixture() (*expiryRepo, *OpenAIGatewayService, *TokenRefreshService, *expiryExecutor) {
	repo := &expiryRepo{a: &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "old-at", "refresh_token": "old-rt", "expires_at": time.Now().Add(24 * time.Hour).Format(time.RFC3339)}, Extra: map[string]any{}}}
	cache := newOpenAITokenCacheStub()
	api := NewOAuthRefreshAPI(repo, cache)
	cfg := &config.Config{TokenRefresh: config.TokenRefreshConfig{Enabled: true, OpenAI401RecoveryEnabled: true, ProviderQPS: 100}}
	provider := NewOpenAITokenProvider(repo, cache, nil)
	provider.SetRefreshAPI(api, nil)
	g := &OpenAIGatewayService{cfg: cfg, accountRepo: repo, openAITokenProvider: provider, httpUpstream: &expiryHTTP{401, expiryBody}}
	e := &expiryExecutor{OpenAITokenRefresher: NewOpenAITokenRefresher(nil, repo)}
	ctx, cancel := context.WithCancel(context.Background())
	w := &TokenRefreshService{cfg: &cfg.TokenRefresh, accountRepo: repo, refreshAPI: api, registrations: []tokenRefreshRegistration{{platform: PlatformOpenAI, refresher: e, executor: e}}, refreshPolicy: DefaultBackgroundRefreshPolicy(), runCtx: ctx, runCancel: cancel, stopCh: make(chan struct{})}
	return repo, g, w, e
}
func expiryRequest(token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(`{"input":"hello"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}
func expirySend(t *testing.T, g *OpenAIGatewayService, a *Account, token string) *http.Response {
	t.Helper()
	resp, err := g.doOpenAIUpstream(expiryRequest(token), "", a)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, expiryBody, string(body))
	require.NoError(t, resp.Body.Close())
	return resp
}

func TestOpenAIOAuthExpiryExistingRefreshPath(t *testing.T) {
	r, g, w, e := expiryFixture()
	a, _ := r.GetByID(context.Background(), 1)
	require.False(t, e.NeedsRefresh(a, time.Minute))
	resp := expirySend(t, g, a, "old-at")
	require.True(t, g.handleFailoverSideEffects(context.Background(), resp, a, []byte(expiryBody)))
	current, _ := r.GetByID(context.Background(), 1)
	require.True(t, e.NeedsRefresh(current, time.Minute))
	require.False(t, current.IsSchedulable())
	_, err := g.openAITokenProvider.GetAccessToken(context.Background(), current)
	require.Error(t, err)
	id := <-w.refreshRequestChannel()
	w.refreshRequestedAccount(context.Background(), id)
	current, _ = r.GetByID(context.Background(), 1)
	require.Equal(t, int32(1), e.calls.Load())
	require.Equal(t, "new-rt", current.GetOpenAIRefreshToken())
	require.True(t, current.IsSchedulable())
	require.Nil(t, current.TempUnschedulableUntil)
	w.refreshRequestedAccount(context.Background(), 1)
	require.Equal(t, int32(1), e.calls.Load())
}

func TestOpenAIOAuthExpiryConcurrentAndLate401(t *testing.T) {
	r, g, w, e := expiryFixture()
	a, _ := r.GetByID(context.Background(), 1)
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() { defer wg.Done(); expirySend(t, g, a, "old-at") }()
	}
	wg.Wait()
	require.Equal(t, 1, r.marks)
	require.Len(t, w.refreshAPI.refreshRequests, 1)
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); w.refreshRequestedAccount(context.Background(), 1) }()
	}
	wg.Wait()
	require.Equal(t, int32(1), e.calls.Load())
	resp := expirySend(t, g, a, "old-at")
	require.True(t, g.handleFailoverSideEffects(context.Background(), resp, a, []byte(expiryBody)))
	current, _ := r.GetByID(context.Background(), 1)
	require.Equal(t, "new-rt", current.GetOpenAIRefreshToken())
	require.True(t, current.IsSchedulable())
	require.Equal(t, 1, r.marks)
}

func TestOpenAIOAuthExpiryFailuresStayRejected(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		redis bool
		same  bool
	}{{"network", errors.New("connection refused"), false, false}, {"permanent", errors.New("invalid_grant"), false, false}, {"redis", nil, true, false}, {"same AT", nil, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			r, g, w, e := expiryFixture()
			e.err, e.sameAT = tc.err, tc.same
			if tc.redis {
				w.refreshAPI.tokenCache.(*openAITokenCacheStub).lockErr = errors.New("redis unavailable")
			}
			a, _ := r.GetByID(context.Background(), 1)
			expirySend(t, g, a, "old-at")
			w.refreshRequestedAccount(context.Background(), 1)
			current, _ := r.GetByID(context.Background(), 1)
			current.TempUnschedulableUntil = nil
			require.False(t, current.IsSchedulable())
			_, err := g.openAITokenProvider.GetAccessToken(context.Background(), current)
			require.Error(t, err)
			if tc.redis {
				require.Zero(t, e.calls.Load())
			}
		})
	}
}

func TestOpenAIOAuthExpiryPreservesManualAndOtherPauses(t *testing.T) {
	r, g, w, _ := expiryFixture()
	until := time.Now().Add(time.Hour)
	r.a.TempUnschedulableUntil = &until
	r.a.TempUnschedulableReason = "quota pause"
	r.a.Schedulable = false
	a, _ := r.GetByID(context.Background(), 1)
	expirySend(t, g, a, "old-at")
	w.refreshRequestedAccount(context.Background(), 1)
	a, _ = r.GetByID(context.Background(), 1)
	require.Equal(t, "new-at", a.GetOpenAIAccessToken())
	require.False(t, a.Schedulable)
	require.Equal(t, "quota pause", a.TempUnschedulableReason)
	require.NotNil(t, a.TempUnschedulableUntil)
}

func TestOpenAIOAuthExpiryBoundsRepeatedRotations(t *testing.T) {
	r, g, _, _ := expiryFixture()
	for i := 1; i <= 3; i++ {
		token := fmt.Sprintf("at-%d", i)
		r.a.Credentials["access_token"] = token
		a, _ := r.GetByID(context.Background(), 1)
		expirySend(t, g, a, token)
	}
	require.Equal(t, StatusError, r.a.Status)
	require.Equal(t, 3, openAIOAuth401(r.a).Count)
}

func TestOpenAIOAuthExpiryScope(t *testing.T) {
	for _, name := range []string{"disabled", "refresh disabled", "custom upstream", "host override", "GET", "PAT", "agent identity", "API key", "unknown 401", "revoked", "missing RT"} {
		t.Run(name, func(t *testing.T) {
			r, g, _, _ := expiryFixture()
			req := expiryRequest("old-at")
			switch name {
			case "disabled":
				g.cfg.TokenRefresh.OpenAI401RecoveryEnabled = false
			case "refresh disabled":
				g.cfg.TokenRefresh.Enabled = false
			case "custom upstream":
				req.URL.Host = "custom.example"
			case "host override":
				req.Host = "custom.example"
			case "GET":
				req.Method = http.MethodGet
			case "PAT":
				r.a.Credentials[openAIAuthModeCredentialKey] = OpenAIAuthModePersonalAccessToken
			case "agent identity":
				r.a.Credentials[openAIAuthModeCredentialKey] = OpenAIAuthModeAgentIdentity
			case "API key":
				r.a.Type = AccountTypeAPIKey
			case "unknown 401":
				g.httpUpstream = &expiryHTTP{401, `{"error":{"message":"Unauthorized"}}`}
			case "revoked":
				g.httpUpstream = &expiryHTTP{401, `{"error":{"code":"token_revoked"}}`}
			case "missing RT":
				delete(r.a.Credentials, "refresh_token")
			}
			a, _ := r.GetByID(context.Background(), 1)
			resp, err := g.doOpenAIUpstream(req, "", a)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Zero(t, r.marks)
		})
	}
}

func TestOpenAIOAuthExpiryWakeUsesExistingLifecycle(t *testing.T) {
	r, g, w, e := expiryFixture()
	w.Start()
	defer w.Stop()
	a, _ := r.GetByID(context.Background(), 1)
	expirySend(t, g, a, "old-at")
	require.Eventually(t, func() bool {
		a, _ := r.GetByID(context.Background(), 1)
		return a.IsSchedulable() && a.GetOpenAIAccessToken() == "new-at"
	}, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, int32(1), e.calls.Load())
}

func TestOpenAIOAuthExpiryDuringExistingRefresh(t *testing.T) {
	r, g, w, e := expiryFixture()
	r.a.Credentials["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
	e.onRefresh = func() {
		a, _ := r.GetByID(context.Background(), 1)
		expirySend(t, g, a, "old-at")
	}
	w.refreshRequestedAccount(context.Background(), 1)
	a, _ := r.GetByID(context.Background(), 1)
	require.Equal(t, "new-rt", a.GetOpenAIRefreshToken())
	require.Nil(t, a.TempUnschedulableUntil)
	require.True(t, a.IsSchedulable())
}

func TestOpenAIOAuthExpirySkipsRejectedCachedAT(t *testing.T) {
	r, g, w, _ := expiryFixture()
	a, _ := r.GetByID(context.Background(), 1)
	expirySend(t, g, a, "old-at")
	w.refreshRequestedAccount(context.Background(), 1)
	a, _ = r.GetByID(context.Background(), 1)
	cache := g.openAITokenProvider.tokenCache
	require.NoError(t, cache.SetAccessToken(context.Background(), OpenAITokenCacheKey(a), "old-at", time.Hour))
	token, err := g.openAITokenProvider.GetAccessToken(context.Background(), a)
	require.NoError(t, err)
	require.Equal(t, "new-at", token, "a stale Redis value must not resurrect the rejected AT")
}
