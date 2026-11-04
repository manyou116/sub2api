package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type resetRecoveryQuota struct {
	*OpenAIQuotaService
	query func() (*OpenAIQuotaUsage, error)
}

func (q resetRecoveryQuota) QueryUsage(context.Context, int64) (*OpenAIQuotaUsage, error) {
	return q.query()
}

type resetRecoveryRecoverer struct {
	account *Account
	calls   int
	failAt  int
}

func (r *resetRecoveryRecoverer) RecoverAccountState(context.Context, int64, AccountRecoveryOptions) (*SuccessfulTestRecoveryResult, error) {
	r.calls++
	if r.calls == r.failAt {
		return nil, errors.New("account recovery unavailable")
	}
	r.account.TempUnschedulableUntil = nil
	r.account.TempUnschedulableReason = ""
	return &SuccessfulTestRecoveryResult{ClearedRateLimit: true}, nil
}

func postResetRecoveryUsage() *OpenAIQuotaUsage {
	return &OpenAIQuotaUsage{
		RateLimit: &OpenAIRateLimit{
			PrimaryWindow: &OpenAIRateLimitWindow{
				UsedPercent: 0, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 5 * 60 * 60,
			},
			SecondaryWindow: &OpenAIRateLimitWindow{
				UsedPercent: 0, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAfterSeconds: 7 * 24 * 60 * 60,
			},
		},
	}
}

func TestCachePostResetSnapshot_UsageSurvivesMissingCreditDetails(t *testing.T) {
	for _, credits := range []*OpenAIRateLimitResetCredits{nil, {AvailableCount: 1}} {
		repo := &stubQuotaAccountRepo{}
		quota := &OpenAIQuotaService{accountRepo: repo}
		usage := postResetRecoveryUsage()
		usage.RateLimitResetCredits = credits
		err := quota.CachePostResetSnapshot(context.Background(), 42, usage)
		require.Error(t, err, "missing card details must still be reported")
		require.Equal(t, 0.0, repo.extraUpdates[42]["codex_5h_used_percent"], "new usage must replace the exhausted pre-reset snapshot")
		require.Equal(t, 0.0, repo.extraUpdates[42]["codex_7d_used_percent"])
		require.NotContains(t, repo.extraUpdates[42], openaiQuotaResetCreditsKey, "preserve the previous card snapshot")
	}
}

func TestQuotaResetPostProcess_ClearsThresholdPauseReappliedDuringRefresh(t *testing.T) {
	for _, missingDetails := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete credits", true: "missing credit details"}[missingDetails], func(t *testing.T) {
			now := time.Now().UTC()
			account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
				Extra: map[string]any{"codex_5h_used_percent": 100.0, "codex_5h_reset_at": now.Add(time.Hour).Format(time.RFC3339)},
			}
			repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{42: account}}
			recoverer := &resetRecoveryRecoverer{account: account}
			usage := postResetRecoveryUsage()
			if !missingDetails {
				usage.RateLimitResetCredits = &OpenAIRateLimitResetCredits{AvailableCount: 0}
			}
			quota := resetRecoveryQuota{OpenAIQuotaService: &OpenAIQuotaService{accountRepo: repo}, query: func() (*OpenAIQuotaUsage, error) {
				// A scheduler reads the exhausted snapshot after the first recovery,
				// while the post-reset upstream query is still in progress.
				decision := EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformOpenAI: 90}, now)
				require.True(t, decision.ShouldPause)
				account.TempUnschedulableUntil = decision.Until
				account.TempUnschedulableReason = BuildDetailedAccountSchedulingThresholdReason(AccountSchedulingThresholdReasonInput{
					Platform: PlatformOpenAI, Window: "5h", ThresholdPercent: 90, UsedPercent: 100, Until: *decision.Until, Now: now,
				})
				return usage, nil
			}}
			load := func(context.Context, int64) (*Account, error) {
				for key, value := range repo.extraUpdates[42] {
					account.Extra[key] = value
				}
				return account, nil
			}
			result := RunOpenAIQuotaResetPostProcess(context.Background(), 42, quota, recoverer, load)
			require.True(t, result.AccountStateRecovered)
			require.NotNil(t, result.Account)
			require.Nil(t, result.Account.TempUnschedulableUntil, "a successful reset must not require a manual account-state reset")
			require.False(t, EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformOpenAI: 90}, now).ShouldPause)
			require.Equal(t, missingDetails, result.WarningCode == OpenAIQuotaResetWarningCacheRefreshFailed)
		})
	}
}

func TestQuotaResetPostProcess_PreservesUnrelatedPause(t *testing.T) {
	account := &Account{ID: 42}
	recoverer := &resetRecoveryRecoverer{account: account}
	repo := &stubQuotaAccountRepo{}
	quota := resetRecoveryQuota{OpenAIQuotaService: &OpenAIQuotaService{accountRepo: repo}, query: func() (*OpenAIQuotaUsage, error) {
		until := time.Now().Add(time.Hour)
		account.TempUnschedulableUntil = &until
		account.TempUnschedulableReason = "new upstream authentication failure"
		usage := postResetRecoveryUsage()
		usage.RateLimitResetCredits = &OpenAIRateLimitResetCredits{}
		return usage, nil
	}}
	result := RunOpenAIQuotaResetPostProcess(context.Background(), 42, quota, recoverer, func(context.Context, int64) (*Account, error) { return account, nil })
	require.NotNil(t, result.Account.TempUnschedulableUntil)
	require.Equal(t, 1, recoverer.calls)
}

func TestPostResetThresholdPause_StillExhaustedWindow(t *testing.T) {
	now := time.Now().UTC()
	until := now.Add(time.Hour)
	account := &Account{Platform: PlatformOpenAI, TempUnschedulableUntil: &until,
		TempUnschedulableReason: BuildDetailedAccountSchedulingThresholdReason(AccountSchedulingThresholdReasonInput{
			Platform: PlatformOpenAI, Window: "5h", ThresholdPercent: 90, UsedPercent: 100, Until: until, Now: now,
		}),
		Extra: map[string]any{
			"codex_5h_used_percent": 0.0, "codex_5h_reset_at": until.Format(time.RFC3339),
			"codex_7d_used_percent": 95.0, "codex_7d_reset_at": until.Format(time.RFC3339),
		},
	}
	require.False(t, shouldRecoverPostResetThresholdPause(account), "another exhausted window must keep the account paused")
	account.Extra["codex_7d_used_percent"] = 0.0
	require.True(t, shouldRecoverPostResetThresholdPause(account))
}

func TestQuotaResetPostProcess_ReportsFinalRecoveryFailure(t *testing.T) {
	until := time.Now().Add(time.Hour)
	account := &Account{ID: 42}
	recoverer := &resetRecoveryRecoverer{account: account, failAt: 2}
	quota := resetRecoveryQuota{OpenAIQuotaService: &OpenAIQuotaService{accountRepo: &stubQuotaAccountRepo{}}, query: func() (*OpenAIQuotaUsage, error) {
		account.TempUnschedulableUntil = &until
		account.TempUnschedulableReason = BuildAccountSchedulingThresholdReason("old usage")
		usage := postResetRecoveryUsage()
		usage.RateLimitResetCredits = &OpenAIRateLimitResetCredits{}
		return usage, nil
	}}
	result := RunOpenAIQuotaResetPostProcess(context.Background(), 42, quota, recoverer, func(context.Context, int64) (*Account, error) { return account, nil })
	require.False(t, result.AccountStateRecovered)
	require.Equal(t, OpenAIQuotaResetWarningAccountRecoveryFailed, result.WarningCode)
	require.NotNil(t, result.Account.TempUnschedulableUntil)
}
