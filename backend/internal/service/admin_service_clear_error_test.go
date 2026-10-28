//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type accountRepoStubForClearAccountError struct {
	mockAccountRepoForGemini
	account                  *Account
	clearErrorCalls          int
	clearRateLimitCalls      int
	clearAntigravityCalls    int
	clearModelRateLimitCalls int
	clearTempUnschedCalls    int
}

func TestAdminService_ClearAccountError_ResetsCapacityCircuitAndTempCache(t *testing.T) {
	for _, keyword := range []string{openAICapacityCooldownKeyword, "openai_slow_first_output"} {
		for _, hidden := range []bool{false, true} {
			t.Run(keyword+fmt.Sprint("/hidden=", hidden), func(t *testing.T) {
				gateway, store := capacityGateway(t, true)
				until := time.Now().Add(time.Hour)
				account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusError,
					Schedulable: false, ErrorMessage: "old error"}
				if !hidden {
					account.TempUnschedulableUntil = &until
					account.TempUnschedulableReason = OpenAICapacityCooldownReasonPrefix + `"matched_keyword":"` + keyword + `"}`
				}
				repo := &accountRepoStubForClearAccountError{account: account}
				gateway.rateLimitService.accountRepo = repo
				gateway.rateLimitService.SetAccountRuntimeBlocker(gateway)
				gateway.BlockAccountScheduling(account, until, "old pause")
				store.current = OpenAICapacityDecision{Generation: 7, UntilMS: until.UnixMilli(), Level: 4}
				store.cached = &TempUnschedState{UntilUnix: until.Unix(), MatchedKeyword: keyword}
				old := store.current
				admin := &adminServiceImpl{accountRepo: repo, runtimeBlocker: gateway}
				updated, err := admin.ClearAccountError(context.Background(), account.ID)
				require.NoError(t, err)
				require.Equal(t, 1, store.resets)
				require.Greater(t, store.current.Generation, old.Generation)
				require.Zero(t, store.current.UntilMS)
				require.Nil(t, store.cached)
				require.Nil(t, updated.TempUnschedulableUntil)
				require.Empty(t, updated.TempUnschedulableReason)
				require.Equal(t, StatusActive, updated.Status)
				require.Empty(t, updated.ErrorMessage)
				require.False(t, updated.Schedulable, "runtime reset must preserve the manual scheduling switch")
				require.False(t, gateway.isOpenAIAccountRuntimeBlocked(updated))
				status, err := gateway.rateLimitService.GetTempUnschedStatus(context.Background(), account.ID)
				require.NoError(t, err)
				require.Nil(t, status, "neither slow-output nor overload details may survive the reset")
				projectionRepo := &capacityPauseRepo{rateLimitClearRepoStub: rateLimitClearRepoStub{getByIDAccount: account}}
				gateway.rateLimitService.accountRepo = projectionRepo
				attempt := &openAICapacityAttempt{store: store, settings: DefaultOpenAICapacityCooldownSettings(), model: "gpt-6.1-sol"}
				require.NoError(t, gateway.rateLimitService.persistOpenAICapacityPause(account, attempt, old))
				require.Zero(t, projectionRepo.setCalls, "old results cannot reapply a pause after this admin entry point resets it")
			})
		}
	}
}

func TestAdminService_ClearAccountError_ReportsRecoveryFailureBeforeClearingDB(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprint("unavailable=", unavailable), func(t *testing.T) {
			gateway, store := capacityGateway(t, true)
			repo := &accountRepoStubForClearAccountError{account: &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusError, ErrorMessage: "keep diagnostic"}}
			gateway.rateLimitService.accountRepo = repo
			store.resetErr = errors.New("Redis unavailable")
			if unavailable {
				gateway.rateLimitService = nil
			}
			admin := &adminServiceImpl{accountRepo: repo, runtimeBlocker: gateway}
			_, err := admin.ClearAccountError(context.Background(), 42)
			require.Error(t, err)
			require.Zero(t, repo.clearErrorCalls)
			require.Zero(t, repo.clearRateLimitCalls)
			require.Zero(t, repo.clearTempUnschedCalls)
			require.Equal(t, "keep diagnostic", repo.account.ErrorMessage)
		})
	}
}

func (r *accountRepoStubForClearAccountError) GetByID(ctx context.Context, id int64) (*Account, error) {
	return r.account, nil
}

func (r *accountRepoStubForClearAccountError) ClearError(ctx context.Context, id int64) error {
	r.clearErrorCalls++
	r.account.Status = StatusActive
	r.account.ErrorMessage = ""
	return nil
}

func (r *accountRepoStubForClearAccountError) ClearRateLimit(ctx context.Context, id int64) error {
	r.clearRateLimitCalls++
	r.account.RateLimitedAt = nil
	r.account.RateLimitResetAt = nil
	return nil
}
func (r *accountRepoStubForClearAccountError) SetWebImageRateLimited(context.Context, int64, time.Time) error {
	return nil
}
func (r *accountRepoStubForClearAccountError) ClearWebImageRateLimit(context.Context, int64) error {
	return nil
}

func (r *accountRepoStubForClearAccountError) ClearAntigravityQuotaScopes(ctx context.Context, id int64) error {
	r.clearAntigravityCalls++
	return nil
}

func (r *accountRepoStubForClearAccountError) ClearModelRateLimits(ctx context.Context, id int64) error {
	r.clearModelRateLimitCalls++
	return nil
}

func (r *accountRepoStubForClearAccountError) ClearTempUnschedulable(ctx context.Context, id int64) error {
	r.clearTempUnschedCalls++
	r.account.TempUnschedulableUntil = nil
	r.account.TempUnschedulableReason = ""
	return nil
}

func TestAdminService_ClearAccountError_AlsoClearsRecoverableRuntimeState(t *testing.T) {
	until := time.Now().Add(10 * time.Minute)
	resetAt := time.Now().Add(5 * time.Minute)
	repo := &accountRepoStubForClearAccountError{
		account: &Account{
			ID:                      31,
			Platform:                PlatformOpenAI,
			Type:                    AccountTypeOAuth,
			Status:                  StatusError,
			ErrorMessage:            "refresh failed",
			RateLimitResetAt:        &resetAt,
			TempUnschedulableUntil:  &until,
			TempUnschedulableReason: "missing refresh token",
		},
	}
	blocker := &runtimeBlockRecorder{}
	svc := &adminServiceImpl{accountRepo: repo, runtimeBlocker: blocker}

	updated, err := svc.ClearAccountError(context.Background(), 31)
	require.NoError(t, err)
	require.NotNil(t, updated)
	require.Equal(t, 1, repo.clearErrorCalls)
	require.Equal(t, 1, repo.clearRateLimitCalls)
	require.Equal(t, 1, repo.clearAntigravityCalls)
	require.Equal(t, 1, repo.clearModelRateLimitCalls)
	require.Equal(t, 1, repo.clearTempUnschedCalls)
	require.Nil(t, updated.RateLimitResetAt)
	require.Nil(t, updated.TempUnschedulableUntil)
	require.Empty(t, updated.TempUnschedulableReason)
	require.Equal(t, []int64{31}, blocker.clearedIDs)
}
