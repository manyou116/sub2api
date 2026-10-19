package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type reconcileTestRepository struct {
	AccountRepository
	claim        func(time.Time, int, string) ([]*Account, error)
	complete     func(*Account, map[string]any, bool) (bool, error)
	writeContext func(context.Context)
}

func (r *reconcileTestRepository) ClaimOpenAIRateLimitReconcileAccounts(_ context.Context, now time.Time, limit int, lease string) ([]*Account, error) {
	return r.claim(now, limit, lease)
}

func (r *reconcileTestRepository) CompleteOpenAIRateLimitReconcile(ctx context.Context, a *Account, updates map[string]any, recovered bool) (bool, error) {
	if r.writeContext != nil {
		r.writeContext(ctx)
	}
	return r.complete(a, updates, recovered)
}

type reconcileTestQuota struct {
	openAIAutoResetQuota // Other methods must never be invoked by reconciliation.
	query                func(context.Context, int64) (*OpenAIQuotaUsage, error)
}

func (q *reconcileTestQuota) QueryUsageForReconciliation(ctx context.Context, id int64) (*OpenAIQuotaUsage, error) {
	return q.query(ctx, id)
}

func reconcileTestAccount(id int64) *Account {
	now, reset := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	return &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Schedulable: true, RateLimitedAt: &now, RateLimitResetAt: &reset,
		Extra: map[string]any{OpenAIRateLimitReconcileEnabledExtraKey: true,
			OpenAIRateLimitReconcileStateExtraKey: OpenAIRateLimitReconcileState{Lease: "probe"}},
	}
}

func reconcileTestUsage() *OpenAIQuotaUsage {
	return &OpenAIQuotaUsage{RateLimit: &OpenAIRateLimit{Allowed: true,
		PrimaryWindow:   &OpenAIRateLimitWindow{UsedPercent: 0, LimitWindowSeconds: 18000, ResetAfterSeconds: 3600},
		SecondaryWindow: &OpenAIRateLimitWindow{UsedPercent: 25, LimitWindowSeconds: 604800, ResetAfterSeconds: 60000},
	}}
}

func TestOpenAIReconcileOptInDefaultsAndValidation(t *testing.T) {
	a := reconcileTestAccount(1)
	delete(a.Extra, OpenAIRateLimitReconcileEnabledExtraKey)
	require.False(t, ResolveOpenAIRateLimitReconcileEnabled(a))
	for _, value := range []any{false, nil, "true", 1} {
		a.Extra[OpenAIRateLimitReconcileEnabledExtraKey] = value
		require.False(t, ResolveOpenAIRateLimitReconcileEnabled(a))
	}
	a.Extra[OpenAIRateLimitReconcileEnabledExtraKey] = true
	require.True(t, ResolveOpenAIRateLimitReconcileEnabled(a))
	a.ParentAccountID = &a.ID
	require.False(t, ResolveOpenAIRateLimitReconcileEnabled(a))
	a.ParentAccountID = nil
	a.Platform = PlatformGrok
	require.False(t, ResolveOpenAIRateLimitReconcileEnabled(a))
	for _, enabled := range []bool{true, false} {
		normalized, err := normalizeOpenAIAutoResetCreditExtra(PlatformOpenAI, AccountTypeOAuth, false, map[string]any{
			OpenAIRateLimitReconcileEnabledExtraKey: enabled,
			OpenAIRateLimitReconcileStateExtraKey:   map[string]any{"lease": "client-forged"},
		})
		require.NoError(t, err)
		require.Equal(t, map[string]any{OpenAIRateLimitReconcileEnabledExtraKey: enabled}, normalized)
	}
}

func TestOpenAIReconcileRecoveryAndRuntimeGeneration(t *testing.T) {
	for _, scenario := range []string{"available", "still-limited", "query-failure", "runtime-changed-during-query", "runtime-changed-during-write", "cas-lost"} {
		t.Run(scenario, func(t *testing.T) {
			account := reconcileTestAccount(1)
			gateway := &OpenAIGatewayService{}
			gateway.BlockAccountScheduling(account, *account.RateLimitResetAt, "429")
			var written map[string]any
			var recoverFlag bool
			repo := &reconcileTestRepository{complete: func(a *Account, updates map[string]any, recovered bool) (bool, error) {
				require.Same(t, account, a)
				written, recoverFlag = updates, recovered
				if scenario == "runtime-changed-during-write" {
					gateway.BlockAccountScheduling(account, account.RateLimitResetAt.Add(time.Hour), "new429")
				}
				return scenario != "cas-lost", nil
			}}
			quota := &reconcileTestQuota{query: func(ctx context.Context, _ int64) (*OpenAIQuotaUsage, error) {
				require.True(t, isOpenAIAutoResetContext(ctx))
				if scenario == "query-failure" {
					return nil, infraerrors.New(502, "OPENAI_QUOTA_REQUEST_FAILED", "secret upstream body")
				}
				if scenario == "runtime-changed-during-query" {
					gateway.BlockAccountScheduling(account, account.RateLimitResetAt.Add(time.Hour), "new429")
				}
				usage := reconcileTestUsage()
				if scenario == "still-limited" {
					usage.RateLimit.SecondaryWindow.UsedPercent = 100
				}
				return usage, nil
			}}
			svc := &OpenAIQuotaAutoResetService{accountRepo: repo, quota: quota, recoverer: &RateLimitService{runtimeBlocker: gateway}}
			svc.reconcileOpenAIRateLimit(context.Background(), account)
			require.NotNil(t, written)
			state, ok := written[OpenAIRateLimitReconcileStateExtraKey].(OpenAIRateLimitReconcileState)
			require.True(t, ok)
			require.GreaterOrEqual(t, state.NextCheckUnix, time.Now().Add(599*time.Second).Unix())
			require.NotEmpty(t, state.CheckedAt)
			encoded, err := json.Marshal(written)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "secret")
			wantRecovery := scenario == "available" || scenario == "runtime-changed-during-write" || scenario == "cas-lost"
			require.Equal(t, wantRecovery, recoverFlag)
			require.Equal(t, scenario != "available", gateway.peekOpenAIAccountRuntimeBlock(account).blocked)
			if scenario == "query-failure" || scenario == "runtime-changed-during-query" {
				require.Len(t, written, 1, "failed/stale reads must not overwrite usage snapshots")
			} else {
				require.Greater(t, len(written), 1, "official windows are committed with the recovery CAS")
			}
		})
	}
}

func TestOpenAIReconcileSkipsOffAndIneligibleAccounts(t *testing.T) {
	cases := []func(*Account){
		func(a *Account) { delete(a.Extra, OpenAIRateLimitReconcileEnabledExtraKey) },
		func(a *Account) { a.Schedulable = false },
		func(a *Account) { a.Status = StatusError },
		func(a *Account) { a.RateLimitedAt = nil },
		func(a *Account) { a.RateLimitResetAt = nil },
		func(a *Account) { past := time.Now().Add(-time.Second); a.RateLimitResetAt = &past },
		func(a *Account) { a.ParentAccountID = &a.ID },
		func(a *Account) { a.Type = AccountTypeAPIKey },
		func(a *Account) { delete(a.Extra, OpenAIRateLimitReconcileStateExtraKey) },
	}
	for _, mutate := range cases {
		a := reconcileTestAccount(1)
		mutate(a)
		svc := &OpenAIQuotaAutoResetService{accountRepo: &reconcileTestRepository{}, quota: &reconcileTestQuota{}}
		svc.reconcileOpenAIRateLimit(context.Background(), a)
	}
}

func TestOpenAIReconcileBoundedConcurrencyAndReplicaCadence(t *testing.T) {
	var queried, active, peak atomic.Int32
	entered, release := make(chan struct{}, 20), make(chan struct{})
	repo := &reconcileTestRepository{claim: func(_ time.Time, limit int, lease string) ([]*Account, error) {
		require.Equal(t, 20, limit)
		accounts := make([]*Account, limit)
		for i := range accounts {
			accounts[i] = reconcileTestAccount(int64(i + 1))
			accounts[i].Extra[OpenAIRateLimitReconcileStateExtraKey] = OpenAIRateLimitReconcileState{Lease: lease}
		}
		return accounts, nil
	}, complete: func(*Account, map[string]any, bool) (bool, error) { return true, nil }}
	quota := &reconcileTestQuota{query: func(ctx context.Context, _ int64) (*OpenAIQuotaUsage, error) {
		n := active.Add(1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		queried.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		active.Add(-1)
		return reconcileTestUsage(), nil
	}}
	cache := &reconcileTestLock{held: make(map[string]string)}
	svc := &OpenAIQuotaAutoResetService{accountRepo: repo, quota: quota, leaderLock: cache}
	done := make(chan struct{})
	go func() { svc.reconcileRateLimitedAccounts(context.Background()); close(done) }()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("workers did not start")
		}
	}
	svc.reconcileRateLimitedAccounts(context.Background()) // Peer cannot join an in-flight batch.
	require.EqualValues(t, 2, queried.Load())
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("batch did not finish")
	}
	require.EqualValues(t, 20, queried.Load())
	require.EqualValues(t, 2, peak.Load())
	svc.reconcileRateLimitedAccounts(context.Background()) // Cadence survives execution-lock release.
	require.EqualValues(t, 20, queried.Load())
	cache.err = errors.New("redis unavailable")
	svc.reconcileRateLimitedAccounts(context.Background())
	require.EqualValues(t, 20, queried.Load())
}

type reconcileTestLock struct {
	mu   sync.Mutex
	held map[string]string
	err  error
}

func (c *reconcileTestLock) TryAcquireLeaderLock(_ context.Context, key, owner string, _ time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return false, c.err
	}
	if _, exists := c.held[key]; exists {
		return false, nil
	}
	c.held[key] = owner
	return true, nil
}
func (c *reconcileTestLock) ReleaseLeaderLock(_ context.Context, key, owner string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held[key] == owner {
		delete(c.held, key)
	}
	return nil
}

func TestOpenAIReconcileBackoff(t *testing.T) {
	for failures, minutes := range []int{10, 10, 20, 40, 60, 60, 60, 60, 60} {
		require.Equal(t, time.Duration(minutes)*time.Minute+17*time.Second, openAIReconcileRetryDelay(77, failures))
	}
}

func TestOpenAIReconcileQueryOnlyUsageAndRejectsIncompleteWindows(t *testing.T) {
	valid := `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":3600,"reset_at":1999999999}}}`
	cases := []struct {
		name, body string
		valid      bool
	}{
		{"complete", valid, true},
		{"incomplete", `{}`, false},
		{"missing-number", strings.ReplaceAll(valid, `"used_percent":0,`, ""), false},
		{"null-number", strings.ReplaceAll(valid, `"used_percent":0`, `"used_percent":null`), false},
		{"missing-reached", strings.ReplaceAll(valid, `"limit_reached":false,`, ""), false},
		{"empty-window", `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{}}}`, false},
		{"null-windows", `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":null}}`, false},
		{"invalid-percent", strings.ReplaceAll(valid, `"used_percent":0`, `"used_percent":101`), false},
		{"limited", strings.ReplaceAll(valid, `"used_percent":0`, `"used_percent":100`), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := reconcileTestAccount(100)
			account.Credentials = map[string]any{"chatgpt_account_id": "org-test"}
			repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{100: account}}
			provider := NewOpenAITokenProvider(repo, &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "fake-token"}}, nil)
			var calls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, "/backend-api/wham/usage", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			svc := NewOpenAIQuotaService(repo, nil, provider, newQuotaRedirectingFactory(srv), nil)
			usage, err := svc.QueryUsageForReconciliation(context.Background(), 100)
			if tc.valid {
				require.NoError(t, err)
				require.NotNil(t, usage)
			} else {
				require.Error(t, err)
				require.Nil(t, usage)
			}
			require.Equal(t, 1, calls, "no credit-detail/model/reset request is allowed")
		})
	}
}

func TestOpenAIReconcilePersistsBackoffAfterProbeTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := &reconcileTestRepository{
		writeContext: func(ctx context.Context) { require.NoError(t, ctx.Err()) },
		complete: func(_ *Account, updates map[string]any, recovered bool) (bool, error) {
			require.False(t, recovered)
			state, ok := updates[OpenAIRateLimitReconcileStateExtraKey].(OpenAIRateLimitReconcileState)
			require.True(t, ok)
			require.Equal(t, "failed", state.Status)
			require.Equal(t, 3, state.ConsecutiveFailures)
			require.Greater(t, state.NextCheckUnix, time.Now().Add(39*time.Minute).Unix())
			return true, nil
		},
	}
	a := reconcileTestAccount(1)
	a.Extra[OpenAIRateLimitReconcileStateExtraKey] = OpenAIRateLimitReconcileState{Lease: "probe", ConsecutiveFailures: 2}
	svc := &OpenAIQuotaAutoResetService{accountRepo: repo, quota: &reconcileTestQuota{query: func(ctx context.Context, _ int64) (*OpenAIQuotaUsage, error) { return nil, ctx.Err() }}}
	svc.reconcileOpenAIRateLimit(ctx, a)
}
