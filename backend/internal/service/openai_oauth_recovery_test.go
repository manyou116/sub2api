//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
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

type recoveryRepo struct {
	AccountRepository
	mu                  sync.Mutex
	a                   *Account
	transitions         int
	persistFailures     int
	quarantineErr       bool
	rotateBeforePersist bool
}

func recoveryClone(a *Account) *Account {
	b, _ := json.Marshal(a)
	var v Account
	_ = json.Unmarshal(b, &v)
	return &v
}
func (r *recoveryRepo) GetByID(context.Context, int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return recoveryClone(r.a), nil
}
func (r *recoveryRepo) TransitionOpenAIOAuthRecovery(_ context.Context, a *Account, expected, next *OpenAIOAuthRecovery, creds map[string]any) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if creds == nil && expected == nil && r.quarantineErr {
		return false, errors.New("outbox unavailable")
	}
	if creds != nil && r.rotateBeforePersist {
		r.a.Credentials["access_token"] = "admin-at"
		r.a.Credentials["refresh_token"] = "admin-rt"
		r.rotateBeforePersist = false
		return false, nil
	}
	if creds != nil && r.persistFailures > 0 {
		r.persistFailures--
		return false, errors.New("database unavailable")
	}
	canSaveRotation := creds != nil && expected != nil && expected.Phase == "refreshing"
	if (!r.a.IsActive() && !canSaveRotation) || !reflect.DeepEqual(r.a.Credentials, a.Credentials) || !reflect.DeepEqual(openAIRecoveryState(r.a), expected) || !sameOptionalInt64(r.a.ProxyID, a.ProxyID) {
		return false, nil
	}
	if r.a.Extra == nil {
		r.a.Extra = map[string]any{}
	}
	if next == nil {
		delete(r.a.Extra, OpenAIOAuthRecoveryKey)
	} else {
		v := *next
		r.a.Extra[OpenAIOAuthRecoveryKey] = v
	}
	if creds != nil {
		r.a.Credentials = shallowCopyMap(creds)
	}
	if next != nil && next.Phase == "reauth" {
		r.a.Status = StatusError
	}
	r.a = recoveryClone(r.a)
	r.transitions++
	return true, nil
}
func (r *recoveryRepo) ListOpenAIOAuthRecoveryCandidates(context.Context, int64, int) ([]Account, error) {
	a, _ := r.GetByID(context.Background(), 1)
	return []Account{*a}, nil
}

type recoveryHTTP struct {
	fn func(*http.Request) (*http.Response, error)
}

func (h *recoveryHTTP) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return h.fn(r)
}
func (h *recoveryHTTP) DoWithTLS(r *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return h.Do(r, p, id, n)
}
func recoveryResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

const recoveryExpired = `{"error":{"code":"token_expired","message":"Your authentication token has expired. Please try refreshing it."}}`

func recoveryFixture() (*recoveryRepo, *OpenAIGatewayService, *TokenRefreshService, *refreshAPIExecutorStub) {
	a := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "old-at", "refresh_token": "old-rt", "expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)}, Extra: map[string]any{}}
	repo := &recoveryRepo{a: a}
	cache := newOpenAITokenCacheStub()
	api := NewOAuthRefreshAPI(repo, cache)
	cfg := &config.Config{}
	cfg.TokenRefresh.Enabled = true
	cfg.TokenRefresh.OpenAI401RecoveryEnabled = true
	provider := NewOpenAITokenProvider(repo, cache, nil)
	provider.SetRefreshAPI(api, nil)
	gateway := &OpenAIGatewayService{accountRepo: repo, openAITokenProvider: provider, cfg: cfg}
	executor := &refreshAPIExecutorStub{credentials: map[string]any{"access_token": "new-at", "refresh_token": "new-rt"}}
	worker := &TokenRefreshService{accountRepo: repo, cfg: &config.TokenRefreshConfig{ProviderConcurrency: 2, ProviderQPS: 100}, refreshAPI: api, registrations: []tokenRefreshRegistration{{platform: PlatformOpenAI, executor: executor}}, providerGates: map[string]*tokenRefreshRateGate{}, providerPools: map[string]*tokenRefreshConcurrencyGate{}}
	return repo, gateway, worker, executor
}
func recoveryRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(`{"input":"hello"}`))
	r.Header.Set("Authorization", "Bearer old-at")
	return r
}

func TestOpenAIOAuthRecoveryEndToEnd(t *testing.T) {
	repo, gateway, worker, executor := recoveryFixture()
	var calls int
	gateway.httpUpstream = &recoveryHTTP{fn: func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return recoveryResponse(401, recoveryExpired), nil
		}
		require.Equal(t, "Bearer new-at", r.Header.Get("Authorization"))
		return recoveryResponse(200, "accepted"), nil
	}}
	a, _ := repo.GetByID(context.Background(), 1)
	resp, err := gateway.doOpenAIUpstream(recoveryRequest(), "", a)
	require.NoError(t, err)
	original, _ := io.ReadAll(resp.Body)
	require.Equal(t, recoveryExpired, string(original))
	_ = resp.Body.Close()
	current, _ := repo.GetByID(context.Background(), 1)
	require.Equal(t, "pending", openAIRecoveryState(current).Phase)
	require.False(t, current.IsSchedulable())
	require.False(t, NewOpenAITokenRefresher(nil, repo).NeedsRefresh(current, 30*time.Minute))
	// Existing side effects must not add a timed quarantine to the repaired owner.
	require.True(t, gateway.handleFailoverSideEffects(context.Background(), resp, a, original))
	require.Nil(t, repo.a.TempUnschedulableUntil)
	worker.processOpenAIRecoveryAccount(context.Background(), current)
	current, _ = repo.GetByID(context.Background(), 1)
	require.Equal(t, 1, executor.refreshCalls)
	require.Equal(t, "verify", openAIRecoveryState(current).Phase)
	require.True(t, current.IsSchedulable())
	require.Equal(t, "new-rt", current.GetOpenAIRefreshToken())
	// A stale scheduler/AT cache is rechecked immediately before dispatch.
	resp, err = gateway.doOpenAIUpstream(recoveryRequest(), "", a)
	require.NoError(t, err)
	_ = resp.Body.Close()
	current, _ = repo.GetByID(context.Background(), 1)
	require.Nil(t, openAIRecoveryState(current))
	require.Equal(t, 2, calls)
}

func TestOpenAIOAuthRecoveryConcurrent401AndHalfOpen(t *testing.T) {
	repo, gateway, worker, executor := recoveryFixture()
	a, _ := repo.GetByID(context.Background(), 1)
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gateway.observeOpenAIRecoveryResponse(context.Background(), repo, a, nil, "old-at", recoveryResponse(401, recoveryExpired), nil)
		}()
	}
	wg.Wait()
	require.Equal(t, 1, repo.transitions)
	current, _ := repo.GetByID(context.Background(), 1)
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); worker.processOpenAIRecoveryAccount(context.Background(), current) }()
	}
	wg.Wait()
	require.Equal(t, 1, executor.refreshCalls)
	started, release := make(chan struct{}), make(chan struct{})
	var sent atomic.Int32
	gateway.httpUpstream = &recoveryHTTP{fn: func(*http.Request) (*http.Response, error) {
		if sent.Add(1) == 1 {
			close(started)
		}
		<-release
		return recoveryResponse(200, "accepted"), nil
	}}
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		resp, _ := gateway.doOpenAIUpstream(recoveryRequest(), "", a)
		if resp != nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("half-open request was not admitted")
	}
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := gateway.doOpenAIUpstream(recoveryRequest(), "", a)
			require.Error(t, err)
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), sent.Load())
	close(release)
	<-firstDone
}

func TestOpenAIOAuthRecoveryFencesLateResponses(t *testing.T) {
	repo, gateway, _, _ := recoveryFixture()
	old, _ := repo.GetByID(context.Background(), 1)
	repo.a.Credentials["access_token"] = "replacement"
	resp := recoveryResponse(401, recoveryExpired)
	gateway.observeOpenAIRecoveryResponse(context.Background(), repo, old, nil, "old-at", resp, nil)
	require.Nil(t, openAIRecoveryState(repo.a))
	require.Equal(t, 0, repo.transitions)
	require.True(t, gateway.handleFailoverSideEffects(context.Background(), resp, old, []byte(recoveryExpired)))
	// A stale success cannot clear a newer generation's quarantine.
	state := &OpenAIOAuthRecovery{Phase: "pending", Generation: "newer", TokenHash: openAIRecoveryTokenHash("replacement")}
	repo.a.Extra[OpenAIOAuthRecoveryKey] = state
	gateway.observeOpenAIRecoveryResponse(context.Background(), repo, old, &OpenAIOAuthRecovery{Phase: "probing", Generation: "older"}, "replacement", recoveryResponse(200, ""), nil)
	require.Equal(t, "newer", openAIRecoveryState(repo.a).Generation)
}

func TestOpenAIOAuthRecoveryFailureDecisions(t *testing.T) {
	for _, tc := range []struct{ name, body, phase string }{{"revoked", `{"error":{"code":"token_revoked"}}`, "reauth"}, {"unknown", `{"detail":"Unauthorized"}`, "attention"}, {"expired", recoveryExpired, "pending"}} {
		t.Run(tc.name, func(t *testing.T) {
			repo, g, _, _ := recoveryFixture()
			a, _ := repo.GetByID(context.Background(), 1)
			g.observeOpenAIRecoveryResponse(context.Background(), repo, a, nil, "old-at", recoveryResponse(401, tc.body), nil)
			require.Equal(t, tc.phase, openAIRecoveryState(repo.a).Phase)
		})
	}
	t.Run("replacement rejected", func(t *testing.T) {
		repo, g, _, _ := recoveryFixture()
		st := &OpenAIOAuthRecovery{Phase: "probing", Generation: "g", TokenHash: openAIRecoveryTokenHash("old-at")}
		repo.a.Extra[OpenAIOAuthRecoveryKey] = st
		a, _ := repo.GetByID(context.Background(), 1)
		g.observeOpenAIRecoveryResponse(context.Background(), repo, a, st, "old-at", recoveryResponse(401, recoveryExpired), nil)
		require.Equal(t, "attention", openAIRecoveryState(repo.a).Phase)
	})
}

func TestOpenAIOAuthRecoveryRefreshFailuresAndPersistence(t *testing.T) {
	for _, tc := range []struct {
		name            string
		refreshErr      error
		persistFailures int
		wantPhase       string
		lockErr         error
	}{{"network", errors.New("connection refused"), 0, "pending", nil}, {"revoked RT", errors.New("invalid_grant"), 0, "reauth", nil}, {"database", nil, 3, "attention", nil}, {"retry persistence only", nil, 1, "verify", nil}, {"redis", nil, 0, "pending", errors.New("redis down")}} {
		t.Run(tc.name, func(t *testing.T) {
			repo, g, w, e := recoveryFixture()
			e.err = tc.refreshErr
			repo.persistFailures = tc.persistFailures
			w.refreshAPI.tokenCache.(*openAITokenCacheStub).lockErr = tc.lockErr
			a, _ := repo.GetByID(context.Background(), 1)
			g.observeOpenAIRecoveryResponse(context.Background(), repo, a, nil, "old-at", recoveryResponse(401, recoveryExpired), nil)
			a, _ = repo.GetByID(context.Background(), 1)
			w.processOpenAIRecoveryAccount(context.Background(), a)
			require.Equal(t, tc.wantPhase, openAIRecoveryState(repo.a).Phase)
			if tc.lockErr != nil {
				require.Zero(t, e.refreshCalls)
			} else {
				require.Equal(t, 1, e.refreshCalls)
			}
		})
	}
	t.Run("admin rotation wins", func(t *testing.T) {
		repo, g, w, e := recoveryFixture()
		repo.rotateBeforePersist = true
		a, _ := repo.GetByID(context.Background(), 1)
		g.observeOpenAIRecoveryResponse(context.Background(), repo, a, nil, "old-at", recoveryResponse(401, recoveryExpired), nil)
		a, _ = repo.GetByID(context.Background(), 1)
		w.processOpenAIRecoveryAccount(context.Background(), a)
		require.Equal(t, "admin-at", repo.a.GetOpenAIAccessToken())
		require.Equal(t, "admin-rt", repo.a.GetOpenAIRefreshToken())
		require.Equal(t, 1, e.refreshCalls)
	})
}

func TestOpenAIOAuthRecoveryRestartAndOtherCooldowns(t *testing.T) {
	repo, g, w, _ := recoveryFixture()
	a, _ := repo.GetByID(context.Background(), 1)
	g.observeOpenAIRecoveryResponse(context.Background(), repo, a, nil, "old-at", recoveryResponse(401, recoveryExpired), nil)
	// A new worker discovers persisted state without receiving the original wakeup.
	w.processOpenAIRecoveryPage(context.Background(), 0)
	require.Equal(t, "verify", openAIRecoveryState(repo.a).Phase)
	until := time.Now().Add(time.Hour)
	repo.a.TempUnschedulableUntil = &until
	repo.a.Schedulable = false
	a, _ = repo.GetByID(context.Background(), 1)
	require.False(t, a.IsSchedulable())
	require.NotNil(t, a.TempUnschedulableUntil)
}

func TestOpenAIOAuthRecoveryPersistsRotationAfterCancellationAndSameAT(t *testing.T) {
	for _, sameAT := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled caller", true: "same AT still rotates RT"}[sameAT], func(t *testing.T) {
			repo, g, w, e := recoveryFixture()
			if sameAT {
				e.credentials["access_token"] = "old-at"
			}
			a, _ := repo.GetByID(context.Background(), 1)
			g.observeOpenAIRecoveryResponse(context.Background(), repo, a, nil, "old-at", recoveryResponse(401, recoveryExpired), nil)
			ctx, cancel := context.WithCancel(context.Background())
			e.onRefresh = cancel
			a, _ = repo.GetByID(ctx, 1)
			w.processOpenAIRecoveryAccount(ctx, a)
			latest, _ := repo.GetByID(context.Background(), 1)
			require.Equal(t, "new-rt", latest.GetOpenAIRefreshToken())
			if sameAT {
				require.Equal(t, "attention", openAIRecoveryState(latest).Phase)
			} else {
				require.Equal(t, "verify", openAIRecoveryState(latest).Phase)
			}
		})
	}
}

func TestOpenAIOAuthRecoveryExpiredLeaseDoesNotReuseRT(t *testing.T) {
	repo, _, w, e := recoveryFixture()
	repo.a.Extra[OpenAIOAuthRecoveryKey] = &OpenAIOAuthRecovery{Phase: "refreshing", Generation: "interrupted", TokenHash: openAIRecoveryTokenHash("old-at"), LeaseUntil: time.Now().Add(-time.Minute).UnixMilli()}
	w.processOpenAIRecoveryPage(context.Background(), 0)
	require.Zero(t, e.refreshCalls)
	require.Equal(t, "attention", openAIRecoveryState(repo.a).Phase)
}

func TestOpenAIOAuthRecoveryQuarantineWriteFailureRemainsLocallyBlocked(t *testing.T) {
	repo, g, w, _ := recoveryFixture()
	repo.quarantineErr = true
	a, _ := repo.GetByID(context.Background(), 1)
	g.observeOpenAIRecoveryResponse(context.Background(), repo, a, nil, "old-at", recoveryResponse(401, recoveryExpired), nil)
	require.Nil(t, openAIRecoveryState(repo.a))
	g.httpUpstream = &recoveryHTTP{fn: func(*http.Request) (*http.Response, error) {
		t.Fatal("rejected token must not be sent while quarantine persistence is unavailable")
		return nil, nil
	}}
	_, err := g.doOpenAIUpstream(recoveryRequest(), "", a)
	require.Error(t, err)
	repo.quarantineErr = false
	w.processOpenAIRecoveryPage(context.Background(), 0)
	require.Equal(t, "verify", openAIRecoveryState(repo.a).Phase)
}

func TestOpenAIOAuthRecoveryReauthorizationRestoresRegularRefresh(t *testing.T) {
	repo, _, _, _ := recoveryFixture()
	repo.a.Extra[OpenAIOAuthRecoveryKey] = &OpenAIOAuthRecovery{Phase: "attention", Generation: "old", TokenHash: openAIRecoveryTokenHash("replaced-at")}
	repo.a.Credentials["expires_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
	require.True(t, NewOpenAITokenRefresher(nil, repo).NeedsRefresh(repo.a, 30*time.Minute))
	require.False(t, openAIRecoveryBlocks(repo.a))
}

func TestOpenAIOAuthRecoveryDisabledRefreshServiceKeepsLegacyResponse(t *testing.T) {
	repo, g, _, _ := recoveryFixture()
	g.cfg.TokenRefresh.Enabled = false
	g.httpUpstream = &recoveryHTTP{fn: func(*http.Request) (*http.Response, error) {
		return recoveryResponse(401, recoveryExpired), nil
	}}
	a, _ := repo.GetByID(context.Background(), 1)
	response, err := g.doOpenAIUpstream(recoveryRequest(), "", a)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	require.Nil(t, openAIRecoveryState(repo.a))
}

func TestOpenAIOAuthRecoveryPersistsRotationWhileAdminStopsAccount(t *testing.T) {
	repo, g, w, e := recoveryFixture()
	a, _ := repo.GetByID(context.Background(), 1)
	g.observeOpenAIRecoveryResponse(context.Background(), repo, a, nil, "old-at", recoveryResponse(401, recoveryExpired), nil)
	e.onRefresh = func() {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		repo.a.Status = StatusError
		repo.a.Schedulable = false
	}
	a, _ = repo.GetByID(context.Background(), 1)
	w.processOpenAIRecoveryAccount(context.Background(), a)
	latest, _ := repo.GetByID(context.Background(), 1)
	require.Equal(t, "new-rt", latest.GetOpenAIRefreshToken())
	require.Equal(t, "verify", openAIRecoveryState(latest).Phase)
	require.Equal(t, StatusError, latest.Status)
	require.False(t, latest.IsSchedulable())
}

func TestOpenAIOAuthRecoveryConcurrentCredentialMetadata(t *testing.T) {
	for _, identityChanged := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserve metadata", true: "identity replacement wins"}[identityChanged], func(t *testing.T) {
			repo, g, w, e := recoveryFixture()
			a, _ := repo.GetByID(context.Background(), 1)
			g.observeOpenAIRecoveryResponse(context.Background(), repo, a, nil, "old-at", recoveryResponse(401, recoveryExpired), nil)
			e.onRefresh = func() {
				repo.mu.Lock()
				defer repo.mu.Unlock()
				key := "admin_note"
				if identityChanged {
					key = "client_id"
				}
				repo.a.Credentials[key] = "edited-during-refresh"
			}
			a, _ = repo.GetByID(context.Background(), 1)
			w.processOpenAIRecoveryAccount(context.Background(), a)
			latest, _ := repo.GetByID(context.Background(), 1)
			require.Equal(t, 1, e.refreshCalls)
			if identityChanged {
				require.Equal(t, "old-at", latest.GetOpenAIAccessToken())
				require.Equal(t, "edited-during-refresh", latest.GetCredential("client_id"))
			} else {
				require.Equal(t, "new-rt", latest.GetOpenAIRefreshToken())
				require.Equal(t, "edited-during-refresh", latest.GetCredential("admin_note"))
			}
		})
	}
}
