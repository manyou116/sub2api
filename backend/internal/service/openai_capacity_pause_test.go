//go:build unit

package service

import (
	"context"
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
