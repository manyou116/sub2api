//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

type capacityStoreStub struct {
	TempUnschedCache
	mu       sync.Mutex
	decision OpenAICapacityDecision
	admitErr error
	ids      []string
	outcomes []int
	reads    int
	trip     OpenAICapacityDecision
	current  OpenAICapacityDecision
	resetErr error
	resets   int
	cached   *TempUnschedState
	released chan struct{}
}

func (c *capacityStoreStub) AdmitOpenAICapacity(context.Context, int64, string, string, int) (OpenAICapacityDecision, error) {
	return c.decision, c.admitErr
}
func (c *capacityStoreStub) ObserveOpenAICapacity(_ context.Context, _ int64, _ string, id string, _ int64, outcome int, _ OpenAICapacityCooldownSettings) (OpenAICapacityDecision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, id)
	c.outcomes = append(c.outcomes, outcome)
	return c.trip, nil
}
func (c *capacityStoreStub) LockOpenAICapacity(context.Context, int64) (func(), error) {
	return func() {}, nil
}
func (c *capacityStoreStub) CurrentOpenAICapacity(context.Context, int64) (OpenAICapacityDecision, error) {
	return c.current, nil
}
func (c *capacityStoreStub) ResetOpenAICapacity(context.Context, int64) error {
	c.resets++
	if c.resetErr == nil {
		c.current.Generation++
		c.current.UntilMS = 0
	}
	return c.resetErr
}
func (c *capacityStoreStub) GetTempUnsched(context.Context, int64) (*TempUnschedState, error) {
	return c.cached, nil
}
func (c *capacityStoreStub) SetTempUnsched(_ context.Context, _ int64, state *TempUnschedState) error {
	c.cached = state
	return nil
}
func (c *capacityStoreStub) DeleteTempUnsched(context.Context, int64) error {
	c.cached = nil
	return nil
}
func (c *capacityStoreStub) ReleaseOpenAICapacity(context.Context, int64, string, string, int64) error {
	c.released <- struct{}{}
	return nil
}
func (c *capacityStoreStub) BlockedOpenAICapacityAccounts(context.Context, string) ([]int64, error) {
	c.reads++
	return []int64{42}, nil
}

func capacityGateway(t *testing.T, enabled bool) (*OpenAIGatewayService, *capacityStoreStub) {
	t.Helper()
	repo := newMockSettingRepo()
	v := DefaultOpenAICapacityCooldownSettings()
	v.Enabled = enabled
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	repo.data[openAICapacityCooldownSettingKey] = string(raw)
	settings := NewSettingService(repo, &config.Config{})
	store := &capacityStoreStub{decision: OpenAICapacityDecision{Allowed: true, Generation: 7}, released: make(chan struct{}, 4)}
	rate := NewRateLimitService(nil, nil, &config.Config{}, nil, store)
	return &OpenAIGatewayService{settingService: settings, rateLimitService: rate}, store
}

func TestCapacityCooldownSettingDefaultsValidationAndImmediateUpdate(t *testing.T) {
	repo := newMockSettingRepo()
	svc := NewSettingService(repo, &config.Config{})
	v, err := svc.GetOpenAICapacityCooldownSettings(context.Background())
	require.NoError(t, err)
	require.False(t, v.Enabled)
	require.False(t, v.SlowFirstOutputEnabled)
	require.Equal(t, 10, v.SlowFirstOutputSeconds)
	require.Equal(t, 30, v.SlowFirstOutputHighEffortSeconds)
	require.Equal(t, 2, v.SlowFirstOutputThreshold)
	require.Equal(t, 1, v.FailureThreshold)
	for _, change := range []func(*OpenAICapacityCooldownSettings){
		func(v *OpenAICapacityCooldownSettings) { v.WindowSeconds = 0 }, func(v *OpenAICapacityCooldownSettings) { v.WindowSeconds = 301 },
		func(v *OpenAICapacityCooldownSettings) { v.FailureThreshold = 0 }, func(v *OpenAICapacityCooldownSettings) { v.FailureThreshold = 101 },
		func(v *OpenAICapacityCooldownSettings) { v.CooldownSeconds = 0 }, func(v *OpenAICapacityCooldownSettings) { v.MaxCooldownSeconds = 1 },
		func(v *OpenAICapacityCooldownSettings) { v.MaxCooldownSeconds = 7201 },
		func(v *OpenAICapacityCooldownSettings) { v.SlowFirstOutputSeconds = 0 },
		func(v *OpenAICapacityCooldownSettings) { v.SlowFirstOutputSeconds = 301 },
		func(v *OpenAICapacityCooldownSettings) { v.SlowFirstOutputHighEffortSeconds = 9 },
		func(v *OpenAICapacityCooldownSettings) { v.SlowFirstOutputHighEffortSeconds = 601 },
		func(v *OpenAICapacityCooldownSettings) { v.SlowFirstOutputWindowSeconds = 0 },
		func(v *OpenAICapacityCooldownSettings) { v.SlowFirstOutputWindowSeconds = 301 },
		func(v *OpenAICapacityCooldownSettings) { v.SlowFirstOutputThreshold = 0 },
		func(v *OpenAICapacityCooldownSettings) { v.SlowFirstOutputThreshold = 101 },
	} {
		bad := v
		change(&bad)
		require.Error(t, svc.SetOpenAICapacityCooldownSettings(context.Background(), bad))
	}
	v.Enabled = true
	require.NoError(t, svc.SetOpenAICapacityCooldownSettings(context.Background(), v))
	got, err := svc.GetOpenAICapacityCooldownSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, v, got)
	repo2 := newMockSettingRepo()
	repo2.data[openAICapacityCooldownSettingKey] = `{"enabled":true,"window_seconds":-1}`
	got, err = NewSettingService(repo2, &config.Config{}).GetOpenAICapacityCooldownSettings(context.Background())
	require.NoError(t, err)
	require.False(t, got.Enabled)
}

func TestCapacityCooldownLegacyRatioIsIgnoredAndSavedCountsArePreserved(t *testing.T) {
	for _, threshold := range []int{1, 3} {
		repo := newMockSettingRepo()
		raw, err := json.Marshal(map[string]any{
			"enabled": true, "switch_account_on_overload": true, "window_seconds": 120,
			"failure_threshold": threshold, "failure_rate_percent": 0,
			"cooldown_seconds": 600, "max_cooldown_seconds": 7200,
		})
		require.NoError(t, err)
		repo.data[openAICapacityCooldownSettingKey] = string(raw)
		svc := NewSettingService(repo, &config.Config{})
		got, err := svc.GetOpenAICapacityCooldownSettings(context.Background())
		require.NoError(t, err)
		require.True(t, got.Enabled, "an obsolete ratio must not disable valid count-based protection")
		require.Equal(t, threshold, got.FailureThreshold)
		require.Equal(t, 120, got.WindowSeconds)
		require.Equal(t, 600, got.CooldownSeconds)
		require.Equal(t, 7200, got.MaxCooldownSeconds)
		require.False(t, got.SlowFirstOutputEnabled, "upgrades must not enable latency protection")
		require.Equal(t, 10, got.SlowFirstOutputSeconds)
		require.Equal(t, 2, got.SlowFirstOutputThreshold)
		require.NoError(t, svc.SetOpenAICapacityCooldownSettings(context.Background(), got))
		require.NotContains(t, repo.data[openAICapacityCooldownSettingKey], "failure_rate_percent")
	}
}

func TestCapacityCooldownAdmissionClonesAndPreservesLogicalRequest(t *testing.T) {
	svc, store := capacityGateway(t, true)
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "logical-request")
	ctx = svc.withOpenAICapacityCooldown(ctx, false)
	original := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	selection := &AccountSelectionResult{Account: original}
	require.True(t, svc.admitOpenAICapacity(ctx, selection, "gpt-6.1-sol"))
	require.NotSame(t, original, selection.Account)
	require.Nil(t, original.capacityAttempt)
	observeOpenAICapacity(selection.Account, 1)
	observeOpenAICapacity(selection.Account, 1)
	observeOpenAICapacityResult(selection.Account, true)
	require.Equal(t, []string{"logical-request", "logical-request", "logical-request"}, store.ids)
	require.Equal(t, []int{1, 1, 2}, store.outcomes)
	require.True(t, openAICapacityBlocked(ctx, original, "gpt-6.1-sol"))
	require.True(t, openAICapacityBlocked(ctx, original, "gpt-6.1-sol"))
	require.Equal(t, 1, store.reads, "one Redis index read per model, not per candidate")
}

func TestCapacityCooldownDisabledAndNonTextPathsLeaveAccountsUntouched(t *testing.T) {
	svc, _ := capacityGateway(t, false)
	ctx := svc.withOpenAICapacityCooldown(context.Background(), false)
	a := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	sel := &AccountSelectionResult{Account: a}
	require.True(t, svc.admitOpenAICapacity(ctx, sel, "gpt-6.1-sol"))
	require.Same(t, a, sel.Account)
	require.False(t, openAICapacityEligible(&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, "gpt-6.1-sol"))
	require.False(t, openAICapacityEligible(a, "gpt-image-2"))
	require.False(t, openAICapacityEligible(&Account{Platform: PlatformGrok, Type: AccountTypeOAuth}, "grok"))
}

func TestCapacityCooldownCancelledProbeReleasesLease(t *testing.T) {
	svc, store := capacityGateway(t, true)
	store.decision.Probe = true
	ctx, cancel := context.WithCancel(context.Background())
	sel := &AccountSelectionResult{Account: &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}}
	require.True(t, svc.admitOpenAICapacity(svc.withOpenAICapacityCooldown(ctx, false), sel, "gpt-6.1-sol"))
	cancel()
	select {
	case <-store.released:
	case <-time.After(time.Second):
		t.Fatal("probe lease was not released on cancellation")
	}
}

func TestCapacityCooldownCanonicalModelAndRedisFailure(t *testing.T) {
	svc, store := capacityGateway(t, true)
	a := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
		"model_mapping": map[string]any{"alias-a": "gpt-5.4", "alias-b": "gpt-5.4", "picture": "gpt-image-2"},
	}}
	ctx := svc.withOpenAICapacityCooldown(context.Background(), false)
	require.True(t, openAICapacityBlocked(ctx, a, "alias-a"))
	require.True(t, openAICapacityBlocked(ctx, a, "alias-b"))
	require.Equal(t, 1, store.reads)
	a.Credentials["compact_model_mapping"] = map[string]any{"alias-a": "gpt-5.4-mini"}
	compactSelection := &AccountSelectionResult{Account: a}
	require.True(t, svc.admitOpenAICapacity(svc.withOpenAICapacityCooldown(context.Background(), true), compactSelection, "alias-a"))
	require.Equal(t, "gpt-5.4-mini", compactSelection.Account.capacityAttempt.model, "the trigger reason must retain the actual compact model")
	sel := &AccountSelectionResult{Account: a}
	require.True(t, svc.admitOpenAICapacity(ctx, sel, "picture"))
	require.Same(t, a, sel.Account, "mapped image model must stay outside the text circuit")
	store.admitErr = errors.New("Redis unavailable")
	require.True(t, svc.admitOpenAICapacity(ctx, sel, "alias-a"), "the optional circuit must fail open")
	require.Equal(t, "gpt-5.4", sel.Account.capacityAttempt.model)
	observeOpenAICapacity(sel.Account, 1)
	require.Empty(t, store.outcomes, "failed admission has no generation safe to record against")
	require.True(t, svc.shouldSwitchOpenAICapacity(sel.Account, []byte(`{"error":{"code":"server_is_overloaded"}}`)), "request-local switching remains available during a Redis outage")
}

func TestCapacityCooldownSwitchPolicyDoesNotChangeQuotaOrDisabledBehavior(t *testing.T) {
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	payload := []byte(`{"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`)
	for _, enabled := range []bool{false, true} {
		svc, _ := capacityGateway(t, enabled)
		sel := &AccountSelectionResult{Account: account}
		require.True(t, svc.admitOpenAICapacity(svc.withOpenAICapacityCooldown(context.Background(), false), sel, "gpt-5.4"))
		e := svc.newOpenAIAccountFailoverError(sel.Account, 503, http.Header{}, payload, "overloaded", false, false)
		require.Equal(t, !enabled, e.RetryableOnSameAccount)
		require.True(t, e.RequestScopedTransient)
		require.True(t, e.ShouldRetryNextAccount())
		quota := svc.newOpenAIAccountFailoverError(account, 429, http.Header{}, []byte(`{"error":{"message":"rate limited"}}`), "rate limited", false, false)
		require.True(t, quota.RetryableOnSameAccount, "transient OAuth 429 retains its retry window")
		generic := svc.newOpenAIAccountFailoverError(account, 503, http.Header{}, []byte(`{"error":{"message":"other upstream error"}}`), "other error", false, false)
		require.False(t, generic.RetryableOnSameAccount)
	}
	svc, _ := capacityGateway(t, true)
	v := DefaultOpenAICapacityCooldownSettings()
	v.Enabled = true
	v.SwitchAccountOnOverload = false
	require.NoError(t, svc.settingService.SetOpenAICapacityCooldownSettings(context.Background(), v))
	sel := &AccountSelectionResult{Account: account}
	require.True(t, svc.admitOpenAICapacity(svc.withOpenAICapacityCooldown(context.Background(), false), sel, "gpt-5.4"))
	e := svc.newOpenAIAccountFailoverError(sel.Account, 503, nil, payload, "overloaded", false, false)
	require.True(t, e.RetryableOnSameAccount)
}
