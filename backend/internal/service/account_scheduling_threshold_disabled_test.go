//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

func TestSchedulingThresholdDisabledWindows(t *testing.T) {
	now := time.Now().UTC()
	for _, override := range []bool{false, true} {
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
			"auto_pause_7d_disabled": true,
			"codex_5h_used_percent":  40.0,
			"codex_7d_used_percent":  99.0,
			"codex_5h_reset_at":      now.Add(time.Hour).Format(time.RFC3339),
			"codex_7d_reset_at":      now.Add(24 * time.Hour).Format(time.RFC3339),
		}}
		if override {
			account.Credentials = map[string]any{accountSchedulingThresholdCredentialKey: 85}
		}
		thresholds := map[string]int{PlatformOpenAI: 90}
		require.False(t, EvaluateAccountSchedulingThreshold(account, thresholds, now).ShouldPause, "disabled 7d must override platform and account thresholds")
		account.Extra["codex_5h_used_percent"] = 95.0
		decision := EvaluateAccountSchedulingThreshold(account, thresholds, now)
		require.True(t, decision.ShouldPause)
		require.Equal(t, "5h", decision.Window)
		account.Extra["auto_pause_5h_disabled"] = true
		require.False(t, EvaluateAccountSchedulingThreshold(account, thresholds, now).ShouldPause)
		account.Extra["auto_pause_7d_disabled"] = false
		require.Equal(t, "7d", EvaluateAccountSchedulingThreshold(account, thresholds, now).Window)
	}
}

type disabledThresholdRepo struct {
	autoResetTestAccountRepo
	casCalls int
	casErr   error
	race     bool
}

func (r *disabledThresholdRepo) CompareAndSwapSchedulingThresholdPause(_ context.Context, _ *Account, until *time.Time, reason string) (bool, error) {
	r.casCalls++
	if r.casErr != nil || r.race {
		return false, r.casErr
	}
	r.account.TempUnschedulableUntil = until
	r.account.TempUnschedulableReason = reason
	return true, nil
}

func (r *disabledThresholdRepo) ListWithFilters(context.Context, pagination.PaginationParams, string, string, string, string, int64, string) ([]Account, *pagination.PaginationResult, error) {
	return []Account{*r.account}, nil, nil
}

func TestSchedulingThresholdDisabledReconciliation(t *testing.T) {
	for _, mode := range []string{"clear", "keep5h", "other", "race", "error", "enabled7d"} {
		t.Run(mode, func(t *testing.T) {
			accountSchedulingThresholdsSF.Forget(SettingKeyAccountSchedulingThresholds)
			accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{})
			t.Cleanup(func() { accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{}) })
			settings := newMockSettingRepo()
			settings.data[SettingKeyAccountSchedulingThresholds] = `{"openai":90}`
			now := time.Now().UTC()
			until := now.Add(24 * time.Hour)
			account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
				TempUnschedulableUntil: &until,
				TempUnschedulableReason: BuildDetailedAccountSchedulingThresholdReason(AccountSchedulingThresholdReasonInput{
					Platform: PlatformOpenAI, Window: "7d", ThresholdPercent: 90, UsedPercent: 99, Until: until, Now: now,
				}),
				Extra: map[string]any{"auto_pause_7d_disabled": true, "codex_7d_used_percent": 99.0, "codex_7d_reset_at": until.Format(time.RFC3339)},
			}
			repo := &disabledThresholdRepo{autoResetTestAccountRepo: autoResetTestAccountRepo{account: account}}
			cache := &tempUnschedCacheRecorder{}
			blocker := &runtimeBlockRecorder{}
			limiter := NewRateLimitService(repo, nil, &config.Config{}, nil, cache)
			limiter.SetSettingService(NewSettingService(settings, &config.Config{}))
			limiter.SetAccountRuntimeBlocker(blocker)
			switch mode {
			case "keep5h":
				account.Extra["codex_5h_used_percent"] = 95.0
				account.Extra["codex_5h_reset_at"] = now.Add(time.Hour).Format(time.RFC3339)
			case "other":
				account.TempUnschedulableReason = "new authentication failure"
			case "race":
				repo.race = true
			case "error":
				repo.casErr = errors.New("database unavailable")
			case "enabled7d":
				account.Extra["auto_pause_7d_disabled"] = false
			}
			// The scanner must include disabled-window accounts even when
			// automatic credit consumption is completely switched off.
			worker := NewOpenAIQuotaAutoResetService(repo, &autoResetTestQuota{}, limiter, nil, nil, nil, nil)
			defer worker.Stop()
			worker.scanEnabledAccounts(context.Background())
			if mode == "enabled7d" {
				require.Empty(t, worker.queue)
				return
			}
			require.Equal(t, account.ID, <-worker.queue)
			err := worker.evaluateAccount(context.Background(), account.ID)
			if mode == "error" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if mode == "clear" {
				require.Nil(t, account.TempUnschedulableUntil)
			} else {
				require.NotNil(t, account.TempUnschedulableUntil)
			}
			if mode == "clear" || mode == "keep5h" {
				require.Equal(t, []int64{42}, cache.deletedIDs)
				require.Equal(t, []int64{42}, blocker.clearedIDs)
			} else {
				require.Empty(t, cache.deletedIDs)
				require.Empty(t, blocker.clearedIDs)
			}
			if mode == "keep5h" {
				reason, ok := parseTempUnschedReasonPayload(account.TempUnschedulableReason)
				require.True(t, ok)
				require.Equal(t, "5h", reason.Window)
				require.WithinDuration(t, now.Add(time.Hour), *account.TempUnschedulableUntil, time.Second)
				require.Len(t, blocker.accounts, 1)
			}
		})
	}
}
