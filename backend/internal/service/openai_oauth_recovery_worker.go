package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

type openAIRecoveryExecutor struct {
	OAuthRefreshExecutor
	generation string
}

func (e *openAIRecoveryExecutor) NeedsRefresh(a *Account, _ time.Duration) bool {
	state := openAIRecoveryState(a)
	return openAIRecoveryMatches(a, state) && state.Generation == e.generation && state.Phase == "refreshing"
}

// One bounded loop shares the existing service lifecycle and provider gates.
// Durable scans compensate for coalesced wakeups and process restarts.
func (s *TokenRefreshService) startOpenAIRecoveryLoop() {
	if s.cfg == nil || !s.cfg.OpenAI401RecoveryEnabled || s.refreshAPI == nil {
		return
	}
	if _, ok := s.accountRepo.(OpenAIOAuthRecoveryRepository); !ok {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		var afterID int64
		for {
			afterID = s.processOpenAIRecoveryPage(s.runCtx, afterID)
			select {
			case <-s.runCtx.Done():
				return
			case <-s.refreshAPI.openAIRecoveryWake:
			case <-ticker.C:
			}
		}
	}()
}

func (s *TokenRefreshService) processOpenAIRecoveryPage(parent context.Context, afterID int64) int64 {
	repo, ok := s.accountRepo.(OpenAIOAuthRecoveryRepository)
	if !ok {
		return 0
	}
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	s.flushOpenAIRecoveryRejected(ctx, repo)
	accounts, err := repo.ListOpenAIOAuthRecoveryCandidates(ctx, afterID, 100)
	if err != nil {
		slog.Warn("openai_oauth_recovery_scan_failed")
		return afterID
	}
	var wg sync.WaitGroup
	for i := range accounts {
		a := accounts[i]
		if a.ID > afterID {
			afterID = a.ID
		}
		state := openAIRecoveryState(&a)
		if state == nil || state.RetryAt > time.Now().UnixMilli() || state.LeaseUntil > time.Now().UnixMilli() {
			continue
		}
		release, acquireErr := s.providerConcurrencyGate(PlatformOpenAI).acquire(ctx)
		if acquireErr != nil {
			break
		}
		wg.Add(1)
		go func() { defer wg.Done(); defer release(); s.processOpenAIRecoveryAccount(ctx, &a) }()
	}
	wg.Wait()
	if len(accounts) < 100 {
		return 0
	}
	return afterID
}

func (s *TokenRefreshService) processOpenAIRecoveryAccount(ctx context.Context, a *Account) {
	repo, ok := s.accountRepo.(OpenAIOAuthRecoveryRepository)
	if !ok || s.refreshAPI == nil {
		return
	}
	state := openAIRecoveryState(a)
	if state == nil || !a.IsActive() {
		return
	}
	if !openAIRecoveryMatches(a, state) {
		_, _ = repo.TransitionOpenAIOAuthRecovery(ctx, a, state, nil, nil)
		return
	}
	if state.Phase == "probing" {
		next := *state
		next.Phase = "verify"
		next.LeaseUntil = 0
		_, _ = repo.TransitionOpenAIOAuthRecovery(ctx, a, state, &next, nil)
		return
	}
	if state.Phase == "refreshing" {
		// A crashed rotation may have consumed the RT: require reconciliation.
		next := *state
		next.Phase = "attention"
		next.Reason = "interrupted_rotation"
		next.LeaseUntil = 0
		_, _ = repo.TransitionOpenAIOAuthRecovery(ctx, a, state, &next, nil)
		return
	}
	if state.Phase != "pending" {
		return
	}
	next := *state
	next.Phase = "refreshing"
	next.Attempts++
	next.LeaseUntil = time.Now().Add(s.attemptTimeout() + 15*time.Second).UnixMilli()
	applied, err := repo.TransitionOpenAIOAuthRecovery(ctx, a, state, &next, nil)
	if err != nil || !applied {
		return
	}
	fresh, err := s.accountRepo.GetByID(ctx, a.ID)
	if err != nil {
		return
	}
	var executor OAuthRefreshExecutor
	for _, r := range s.registrations {
		if r.platform == PlatformOpenAI {
			executor = r.executor
			break
		}
	}
	if executor == nil {
		return
	}
	executor = &rateLimitedOAuthRefreshExecutor{OAuthRefreshExecutor: &openAIRecoveryExecutor{executor, next.Generation}, acquireRate: s.providerRateGate(PlatformOpenAI).acquire}
	attemptCtx, cancel := context.WithTimeout(context.WithValue(ctx, openAIRecoveryStrictKey{}, true), s.attemptTimeout())
	result, refreshErr := s.refreshAPI.RefreshIfNeeded(attemptCtx, fresh, executor, 0)
	cancel()
	if refreshErr == nil && result != nil && !result.LockHeld {
		slog.Info("openai_oauth_recovery_rotated", "account_id", a.ID, "refreshed", result.Refreshed)
		return
	}
	cleanup, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelCleanup()
	latest, readErr := s.accountRepo.GetByID(cleanup, a.ID)
	if readErr != nil {
		return
	}
	current := openAIRecoveryState(latest)
	if current == nil || current.Generation != next.Generation || current.Phase != "refreshing" || !openAIRecoveryMatches(latest, current) {
		return
	}
	retry := *current
	retry.Phase = "pending"
	retry.LeaseUntil = 0
	retry.Reason = "refresh_transient"
	retry.RetryAt = time.Now().Add(30 * time.Second * time.Duration(1<<uint(min(retry.Attempts-1, 4)))).UnixMilli()
	switch {
	case errors.Is(refreshErr, errOAuthRefreshCredentialPersist):
		retry.Phase = "attention"
		retry.Reason = "credential_persist_unknown"
	case isNonRetryableRefreshError(refreshErr):
		retry.Phase = "reauth"
		retry.Reason = "refresh_credential_invalid"
	case result != nil && result.LockHeld:
		retry.Attempts--
		retry.Reason = "lock_busy"
	case retry.Attempts >= 3:
		retry.Phase = "attention"
		retry.Reason = "refresh_retry_exhausted"
	}
	_, _ = repo.TransitionOpenAIOAuthRecovery(cleanup, latest, current, &retry, nil)
	slog.Warn("openai_oauth_recovery_deferred", "account_id", a.ID, "phase", retry.Phase, "reason", retry.Reason)
}

// Retain local quarantine while a durable write/outbox is temporarily unavailable.
// Drain bounded batches; failures remain parked until storage recovers.
func (s *TokenRefreshService) flushOpenAIRecoveryRejected(ctx context.Context, repo OpenAIOAuthRecoveryRepository) {
	if s.refreshAPI == nil {
		return
	}
	n := 0
	s.refreshAPI.openAIRecoveryRejected.Range(func(key, value any) bool {
		n++
		k, keyOK := key.(openAIRecoveryRejectedKey)
		state, stateOK := value.(*OpenAIOAuthRecovery)
		if !keyOK || !stateOK {
			return n < 100
		}
		a, err := s.accountRepo.GetByID(ctx, k.AccountID)
		if err != nil || a == nil {
			return n < 100 && ctx.Err() == nil
		}
		current := openAIRecoveryState(a)
		if !a.IsActive() || openAIRecoveryTokenHash(a.GetOpenAIAccessToken()) != k.TokenHash || (current != nil && openAIRecoveryMatches(a, current)) {
			s.refreshAPI.openAIRecoveryRejected.CompareAndDelete(key, value)
		} else if applied, writeErr := repo.TransitionOpenAIOAuthRecovery(ctx, a, current, state, nil); writeErr == nil && applied {
			s.refreshAPI.openAIRecoveryRejected.CompareAndDelete(key, value)
		}
		return n < 100 && ctx.Err() == nil
	})
}
