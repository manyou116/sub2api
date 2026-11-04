package service

import (
	"context"
	"log/slog"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	OpenAIQuotaResetWarningCacheRefreshFailed    = "reset_credit_cache_refresh_failed"
	OpenAIQuotaResetWarningAccountRecoveryFailed = "account_state_recovery_failed"
	OpenAIQuotaResetWarningAccountRefreshFailed  = "account_state_refresh_failed"
)

type openAIQuotaResetWorkflowQuota interface {
	QueryUsage(ctx context.Context, accountID int64) (*OpenAIQuotaUsage, error)
	CachePostResetSnapshot(ctx context.Context, accountID int64, usage *OpenAIQuotaUsage) error
}

type openAIQuotaResetWorkflowRecoverer interface {
	RecoverAccountState(ctx context.Context, accountID int64, options AccountRecoveryOptions) (*SuccessfulTestRecoveryResult, error)
}

// OpenAIQuotaResetPostProcessResult 汇总手动和自动用卡后的共享恢复结果。
// WarningCode 允许上游消费成功但本地恢复部分失败时仍然准确呈现状态。
type OpenAIQuotaResetPostProcessResult struct {
	Quota                 *OpenAIQuotaUsage
	Account               *Account
	CacheRefreshed        bool
	AccountStateRecovered bool
	WarningCode           string
}

// RunOpenAIQuotaResetPostProcess 解除限流、刷新额度缓存，再核对刷新期间重新出现的
// 阈值停调，避免手动和自动用卡后仍需人工重置账号状态。
func RunOpenAIQuotaResetPostProcess(
	ctx context.Context,
	accountID int64,
	quota openAIQuotaResetWorkflowQuota,
	recoverer openAIQuotaResetWorkflowRecoverer,
	loadAccount func(context.Context, int64) (*Account, error),
) OpenAIQuotaResetPostProcessResult {
	result := OpenAIQuotaResetPostProcessResult{}
	if recoverer == nil {
		result.WarningCode = OpenAIQuotaResetWarningAccountRecoveryFailed
		return result
	}
	if _, err := recoverer.RecoverAccountState(ctx, accountID, AccountRecoveryOptions{InvalidateToken: true}); err != nil {
		slog.Warn("openai_quota_reset_account_recovery_failed", "account_id", accountID, "error_code", infraerrors.Reason(err))
		result.WarningCode = OpenAIQuotaResetWarningAccountRecoveryFailed
		return result
	}
	result.AccountStateRecovered = true
	usageRefreshed := false

	if quota != nil {
		usage, usageErr := quota.QueryUsage(ctx, accountID)
		switch {
		case usageErr != nil || usage == nil:
			slog.Warn("openai_quota_reset_cache_refresh_failed", "account_id", accountID, "error_code", infraerrors.Reason(usageErr))
			result.WarningCode = OpenAIQuotaResetWarningCacheRefreshFailed
		default:
			if err := quota.CachePostResetSnapshot(ctx, accountID, usage); err != nil {
				usageRefreshed = infraerrors.Reason(err) == "OPENAI_QUOTA_RESET_CREDITS_REFRESH_FAILED"
				slog.Warn("openai_quota_reset_cache_refresh_failed", "account_id", accountID, "error_code", infraerrors.Reason(err))
				result.WarningCode = OpenAIQuotaResetWarningCacheRefreshFailed
			} else {
				usageRefreshed = true
				result.Quota = usage
				result.CacheRefreshed = true
			}
		}
	}

	if loadAccount == nil {
		return result
	}
	account, err := loadAccount(ctx, accountID)
	// The initial recovery exposes the account while its old exhausted usage
	// is still cached. A concurrent scheduler can persist another threshold
	// pause during the upstream refresh; clear that pause after the new usage
	// has been written. Other newly observed failures must remain intact.
	if usageRefreshed && err == nil && shouldRecoverPostResetThresholdPause(account) {
		if _, recoveryErr := recoverer.RecoverAccountState(ctx, accountID, AccountRecoveryOptions{InvalidateToken: true}); recoveryErr != nil {
			slog.Warn("openai_quota_reset_account_recovery_failed", "account_id", accountID, "error_code", infraerrors.Reason(recoveryErr))
			result.AccountStateRecovered = false
			result.WarningCode = OpenAIQuotaResetWarningAccountRecoveryFailed
			result.Account = account
			return result
		}
		account, err = loadAccount(ctx, accountID)
	}
	if err != nil {
		slog.Warn("openai_quota_reset_account_refresh_failed", "account_id", accountID, "error_code", infraerrors.Reason(err))
		if result.WarningCode == "" {
			result.WarningCode = OpenAIQuotaResetWarningAccountRefreshFailed
		}
		return result
	}
	result.Account = account
	return result
}

func shouldRecoverPostResetThresholdPause(account *Account) bool {
	if account == nil || account.TempUnschedulableUntil == nil {
		return false
	}
	reason, ok := parseTempUnschedReasonPayload(account.TempUnschedulableReason)
	if !ok || reason.Source != AccountSchedulingThresholdReasonSource {
		return false
	}
	// A different window can remain exhausted after a partial upstream reset.
	// Reuse the threshold evaluator so a valid pause is not cleared as stale.
	decision := EvaluateAccountSchedulingThreshold(account, map[string]int{account.Platform: reason.ThresholdPercent}, time.Now())
	return reason.ThresholdPercent < 1 || !decision.ShouldPause
}
