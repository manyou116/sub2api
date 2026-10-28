package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

const OpenAIRateLimitReconcileStateExtraKey = "openai_rate_limit_reconcile_state"

const (
	openAIReconcileBatchSize   = 20
	openAIReconcileConcurrency = 2
	openAIReconcileLockKey     = "jobs:openai-rate-limit-reconcile"
)

// Only sanitized operational metadata is persisted; never upstream bodies or tokens.
type OpenAIRateLimitReconcileState struct {
	Status              string `json:"status"`
	CheckedAt           string `json:"checked_at,omitempty"`
	NextCheckUnix       int64  `json:"next_check_unix"`
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
	RecoveredAt         string `json:"recovered_at,omitempty"`
	ErrorCode           string `json:"error_code,omitempty"`
	Lease               string `json:"lease,omitempty"`
}

type openAIRateLimitReconcileRepository interface {
	ClaimOpenAIRateLimitReconcileAccounts(context.Context, time.Time, int, string) ([]*Account, error)
	CompleteOpenAIRateLimitReconcile(context.Context, *Account, map[string]any, bool) (bool, error)
}

type openAIRateLimitReconcileQuota interface {
	QueryUsageForReconciliation(context.Context, int64) (*OpenAIQuotaUsage, error)
}

func (s *OpenAIQuotaAutoResetService) runRateLimitReconciler() {
	defer s.wg.Done()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
			s.reconcileRateLimitedAccounts(s.ctx)
			timer.Reset(time.Minute)
		}
	}
}

// The persistent due time provides fairness across restarts. A separate small
// batch avoids flooding the reset-credit queue when many accounts opt in.
func (s *OpenAIQuotaAutoResetService) reconcileRateLimitedAccounts(parent context.Context) {
	repo, ok := s.accountRepo.(openAIRateLimitReconcileRepository)
	if !ok {
		return
	}
	if _, ok := s.quota.(openAIRateLimitReconcileQuota); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 4*time.Minute)
	defer cancel()
	owner := uuid.NewString()
	if s.leaderLock != nil {
		locked, err := s.leaderLock.TryAcquireLeaderLock(ctx, openAIReconcileLockKey, owner, 5*time.Minute)
		if err != nil || !locked {
			return // Fail closed: a Redis outage must not amplify upstream traffic.
		}
		defer func() {
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer releaseCancel()
			_ = s.leaderLock.ReleaseLeaderLock(releaseCtx, openAIReconcileLockKey, owner)
		}()
		// Keep this TTL after completion so staggered replicas share the same cap.
		locked, err = s.leaderLock.TryAcquireLeaderLock(ctx, openAIReconcileLockKey+":cadence", owner, time.Minute)
		if err != nil || !locked {
			return
		}
	}
	accounts, err := repo.ClaimOpenAIRateLimitReconcileAccounts(ctx, time.Now(), openAIReconcileBatchSize, owner)
	if err != nil {
		slog.Warn("openai_rate_limit_reconcile_claim_failed")
		return
	}
	var wg sync.WaitGroup
	for worker := 0; worker < openAIReconcileConcurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := worker; i < len(accounts) && ctx.Err() == nil; i += openAIReconcileConcurrency {
				account := accounts[i]
				if readOpenAIReconcileState(account).Lease != owner {
					continue
				}
				probeCtx, probeCancel := context.WithTimeout(ctx, 25*time.Second)
				s.reconcileOpenAIRateLimit(probeCtx, account)
				probeCancel()
			}
		}()
	}
	wg.Wait()
}

func (s *OpenAIQuotaAutoResetService) reconcileOpenAIRateLimit(ctx context.Context, account *Account) {
	if !ResolveOpenAIRateLimitReconcileEnabled(account) || account.Status != StatusActive || !account.Schedulable ||
		!account.IsCredentialUsableForShadow() || account.IsOverloaded() ||
		account.RateLimitedAt == nil || account.RateLimitResetAt == nil || !time.Now().Before(*account.RateLimitResetAt) {
		return
	}
	repo, ok := s.accountRepo.(openAIRateLimitReconcileRepository)
	quota, queryOK := s.quota.(openAIRateLimitReconcileQuota)
	if !ok || !queryOK || readOpenAIReconcileState(account).Lease == "" {
		return
	}
	var gateway *OpenAIGatewayService
	if recovery, ok := s.recoverer.(*RateLimitService); ok {
		gateway, _ = recovery.runtimeBlocker.(*OpenAIGatewayService)
	}
	observedBlock := gateway.peekOpenAIAccountRuntimeBlock(account)
	usage, err := quota.QueryUsageForReconciliation(withOpenAIAutoResetContext(ctx), account.ID)
	now := time.Now()
	state := readOpenAIReconcileState(account)
	state.Status, state.CheckedAt, state.ErrorCode = "limited", now.UTC().Format(time.RFC3339), ""
	state.ConsecutiveFailures = 0
	updates := map[string]any{}
	recovered := false
	switch {
	case err != nil:
		state.Status, state.ErrorCode = "failed", infraerrors.Reason(err)
		state.ConsecutiveFailures = min(7, readOpenAIReconcileState(account).ConsecutiveFailures+1)
	case gateway.peekOpenAIAccountRuntimeBlock(account) != observedBlock:
		state.Status = "state_changed"
	default:
		updates = buildOpenAIAutoResetUsageUpdates(usage, now)
		if updates == nil {
			updates = map[string]any{}
		}
		recovered = openAIQuotaUsageConfirmsAvailable(usage)
		if recovered {
			state.Status, state.RecoveredAt = "recovered", state.CheckedAt
		}
	}
	state.NextCheckUnix = now.Add(openAIReconcileRetryDelay(account.ID, state.ConsecutiveFailures)).Unix()
	updates[OpenAIRateLimitReconcileStateExtraKey] = state
	// Persist backoff even when the probe's own deadline has expired.
	writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer writeCancel()
	applied, writeErr := repo.CompleteOpenAIRateLimitReconcile(writeCtx, account, updates, recovered)
	if writeErr != nil {
		slog.Warn("openai_rate_limit_reconcile_write_failed", "account_id", account.ID)
	} else if applied && recovered {
		gateway.clearOpenAIAccountRuntimeBlockIfUnchanged(account.ID, observedBlock)
		slog.Info("openai_rate_limit_reconciled", "account_id", account.ID)
	}
}

func readOpenAIReconcileState(account *Account) (state OpenAIRateLimitReconcileState) {
	if account != nil {
		data, _ := json.Marshal(account.Extra[OpenAIRateLimitReconcileStateExtraKey])
		_ = json.Unmarshal(data, &state)
	}
	state.ConsecutiveFailures = max(0, min(7, state.ConsecutiveFailures))
	return state
}

func openAIReconcileRetryDelay(accountID int64, failures int) time.Duration {
	minutes := min(60, 10*(1<<max(0, min(failures-1, 3))))
	return time.Duration(minutes)*time.Minute + time.Duration(accountID%60)*time.Second
}

func openAIQuotaUsageConfirmsAvailable(usage *OpenAIQuotaUsage) bool {
	if usage == nil || usage.RateLimit == nil || !usage.RateLimit.Allowed || usage.RateLimit.LimitReached {
		return false
	}
	count := 0
	for _, window := range []*OpenAIRateLimitWindow{usage.RateLimit.PrimaryWindow, usage.RateLimit.SecondaryWindow} {
		if window == nil {
			continue
		}
		count++
		if math.IsNaN(window.UsedPercent) || window.UsedPercent < 0 || window.UsedPercent >= 100 || window.LimitWindowSeconds <= 0 {
			return false
		}
	}
	return count > 0
}

// Missing/null numbers must never decode as a fabricated zero-percent window.
func validOpenAIReconcilePayload(data []byte) bool {
	type window struct {
		Used  *float64 `json:"used_percent"`
		Limit *int64   `json:"limit_window_seconds"`
		After *int64   `json:"reset_after_seconds"`
		At    *int64   `json:"reset_at"`
	}
	var payload struct {
		Rate *struct {
			Allowed   *bool   `json:"allowed"`
			Reached   *bool   `json:"limit_reached"`
			Primary   *window `json:"primary_window"`
			Secondary *window `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Rate == nil || payload.Rate.Allowed == nil || payload.Rate.Reached == nil {
		return false
	}
	count := 0
	for _, w := range []*window{payload.Rate.Primary, payload.Rate.Secondary} {
		if w == nil {
			continue
		}
		count++
		if w.Used == nil || *w.Used < 0 || *w.Used > 100 || w.Limit == nil || *w.Limit <= 0 || w.After == nil || *w.After < 0 || w.At == nil || *w.At < 0 {
			return false
		}
	}
	return count > 0
}
