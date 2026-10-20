//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type capacityPauseRepo struct {
	rateLimitClearRepoStub
	setCalls int
}

func (r *capacityPauseRepo) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.setCalls++
	r.getByIDAccount.TempUnschedulableUntil = &until
	r.getByIDAccount.TempUnschedulableReason = reason
	return nil
}

func TestCapacityCooldownPersistsExistingTempUnschedStatusAndWebException(t *testing.T) {
	svc, store := capacityGateway(t, true)
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	repo := &capacityPauseRepo{rateLimitClearRepoStub: rateLimitClearRepoStub{getByIDAccount: account}}
	svc.rateLimitService.accountRepo = repo
	selection := &AccountSelectionResult{Account: account}
	require.True(t, svc.admitOpenAICapacity(svc.withOpenAICapacityCooldown(context.Background(), false), selection, "gpt-6.1-sol"))
	store.trip = OpenAICapacityDecision{Tripped: true, Generation: 8, UntilMS: time.Now().Add(10 * time.Minute).UnixMilli(), Failures: 1, Total: 5, Level: 1}
	store.current = store.trip
	observeOpenAICapacity(selection.Account, 1)
	require.Equal(t, 1, repo.setCalls)
	require.False(t, account.IsSchedulable(), "the DB projection must block all text models")
	require.True(t, IsOpenAICapacityCooldownReason(account.TempUnschedulableReason))
	require.True(t, account.IsSchedulableIgnoringTextRateLimit(), "web images bypass only this text pause")
	state, err := svc.rateLimitService.GetTempUnschedStatus(context.Background(), 42)
	require.NoError(t, err)
	require.Equal(t, TempUnschedScopeAccount, state.Scope)
	require.Equal(t, "gpt-6.1-sol", state.Model)
	require.EqualValues(t, 1, state.TriggerCount)
	require.Equal(t, 1, state.TriggerThreshold)
	require.Contains(t, state.ErrorMessage, "Web 生图除外")
	// A later auth/transport block wins over a cached capacity display state.
	account.TempUnschedulableReason = "token refresh retry exhausted: timeout"
	state, err = svc.rateLimitService.GetTempUnschedStatus(context.Background(), 42)
	require.NoError(t, err)
	require.Equal(t, account.TempUnschedulableReason, state.ErrorMessage)
	require.False(t, account.IsSchedulableIgnoringTextRateLimit())
}

func TestCapacityCooldownManualRecoveryResetsCircuitAndFencesLateProjection(t *testing.T) {
	for _, mode := range []string{"recover", "clear-temp", "clear-rate"} {
		t.Run(mode, func(t *testing.T) {
			svc, store := capacityGateway(t, true)
			until := time.Now().Add(time.Hour)
			account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, TempUnschedulableUntil: &until}
			repo := &capacityPauseRepo{rateLimitClearRepoStub: rateLimitClearRepoStub{getByIDAccount: account}}
			svc.rateLimitService.accountRepo = repo
			old := OpenAICapacityDecision{Generation: 2, UntilMS: until.UnixMilli(), Tripped: true}
			store.current = old
			var err error
			switch mode {
			case "recover":
				_, err = svc.rateLimitService.RecoverAccountState(context.Background(), 42, AccountRecoveryOptions{InvalidateToken: true})
			case "clear-temp":
				err = svc.rateLimitService.ClearTempUnschedulable(context.Background(), 42)
			case "clear-rate":
				err = svc.rateLimitService.ClearRateLimit(context.Background(), 42)
			}
			require.NoError(t, err)
			require.Equal(t, 1, store.resets)
			require.Equal(t, 1, repo.clearTempUnschedCalls)
			attempt := &openAICapacityAttempt{store: store, settings: DefaultOpenAICapacityCooldownSettings(), model: "gpt-6.1-sol"}
			require.NoError(t, svc.rateLimitService.persistOpenAICapacityPause(account, attempt, old))
			require.Zero(t, repo.setCalls, "a delayed pre-recovery projection cannot restore the pause")
		})
	}
}

func TestCapacityCooldownRecoveryReportsRedisFailureBeforeClearingDB(t *testing.T) {
	store := &capacityStoreStub{resetErr: errors.New("Redis unavailable")}
	repo := &capacityPauseRepo{rateLimitClearRepoStub: rateLimitClearRepoStub{getByIDAccount: &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}}}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, store)
	require.Error(t, svc.ClearTempUnschedulable(context.Background(), 42))
	require.Zero(t, repo.clearTempUnschedCalls)
}

func TestCapacityCooldownWebExemptionDoesNotBypassOtherPauseReasons(t *testing.T) {
	until := time.Now().Add(time.Hour)
	for _, reason := range []string{"", "token refresh retry exhausted: timeout", `{"source":"authentication","error_message":"unauthorized"}`} {
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, TempUnschedulableUntil: &until, TempUnschedulableReason: reason}
		require.False(t, account.IsSchedulableIgnoringTextRateLimit(), reason)
	}
	state := struct {
		Source string `json:"source"`
		TempUnschedState
	}{openAICapacityCooldownKeyword, TempUnschedState{Scope: TempUnschedScopeAccount}}
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, TempUnschedulableUntil: &until, TempUnschedulableReason: string(raw), WebImageRateLimitResetAt: &until}
	require.False(t, account.IsSchedulableIgnoringTextRateLimit(), "web-image quota cooldown still wins")
}

func TestCapacityCooldownWorkModeWebImagesExemptOnlyTextCapacity(t *testing.T) {
	until := time.Now().Add(time.Hour)
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		TempUnschedulableUntil: &until, TempUnschedulableReason: OpenAICapacityCooldownReasonPrefix + `"error_message":"text capacity"}`,
		Extra: map[string]any{"openai_web_images": map[string]any{"enabled": true, "model_mode": "fixed", "model": "gpt-5.6-luna-wm"}}}
	svc := &OpenAIGatewayService{webImages: NewOpenAIWebImagesService(&config.Config{}, nil, nil)}
	require.Equal(t, "gpt-5.6-luna-wm", svc.webImages.ResolveUpstream(account).UpstreamModel)
	require.True(t, svc.isAccountSchedulableForOpenAIRequest(context.Background(), account, OpenAIImagesCapabilityBasic))
	require.NotNil(t, account.TempUnschedulableUntil, "checking Web eligibility must not alter the shared snapshot")
	require.False(t, svc.isAccountSchedulableForOpenAIRequest(context.Background(), account, ""))
	account.TempUnschedulableReason = "authentication failed"
	require.False(t, svc.isAccountSchedulableForOpenAIRequest(context.Background(), account, OpenAIImagesCapabilityBasic))
	account.TempUnschedulableReason = OpenAICapacityCooldownReasonPrefix + `"error_message":"text capacity"}`
	account.WebImageRateLimitResetAt = &until
	require.False(t, svc.isAccountSchedulableForOpenAIRequest(context.Background(), account, OpenAIImagesCapabilityBasic))
}
