package service

import (
	"context"
	"errors"
	"time"
)

// Both timestamps fence recovery against a newer failure with the same reset time.
type WebImageCooldown struct {
	Until      *time.Time
	ObservedAt *time.Time
	Reason     string
}

type webImageCooldownRepository interface {
	WebImageCooldowns(context.Context, []int64) (map[int64]WebImageCooldown, error)
	ClearWebImageCooldownIfObserved(context.Context, int64, WebImageCooldown) error
}

type webImageQuotaBatchCache interface {
	GetMany(context.Context, []int64) (map[int64]*WebImageStatus, error)
}

type WebImageStatusBatch struct {
	Statuses map[int64]*WebImageStatus `json:"statuses"`
	Errors   map[int64]string          `json:"errors"`
}

func (s *OpenAIWebImageService) readCooldowns(ctx context.Context, ids []int64) (map[int64]WebImageCooldown, error) {
	if repo, ok := s.accounts.(webImageCooldownRepository); ok {
		return repo.WebImageCooldowns(ctx, ids)
	}
	result := make(map[int64]WebImageCooldown, len(ids))
	if repo, ok := s.accounts.(WebImageStateRepository); ok {
		for _, id := range ids {
			until, err := repo.WebImageCooldown(ctx, id)
			if err != nil {
				return nil, err
			}
			result[id] = WebImageCooldown{Until: until, Reason: "legacy_cooldown"}
		}
	}
	return result, nil
}

func (s *OpenAIWebImageService) parentStatuses(ctx context.Context, ids []int64) (map[int64]*WebImageStatus, error) {
	statuses := make(map[int64]*WebImageStatus, len(ids))
	var err error
	if cache, ok := s.quotaCache.(webImageQuotaBatchCache); ok {
		statuses, err = cache.GetMany(ctx, ids)
		if err != nil {
			return nil, err
		}
	} else if s.quotaCache != nil {
		for _, id := range ids {
			statuses[id], err = s.quotaCache.Get(ctx, id)
			if err != nil {
				return nil, err
			}
		}
	}
	cooldowns, err := s.readCooldowns(ctx, ids)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, id := range ids {
		status := statuses[id]
		if status == nil {
			status = &WebImageStatus{ParentAccountID: id}
			statuses[id] = status
		}
		if status.CooldownUntil != nil && !status.CooldownUntil.After(now) {
			status.CooldownUntil = nil
			status.CooldownReason = ""
		}
		if cooldown := cooldowns[id]; cooldown.Until != nil && cooldown.Until.After(now) && (status.CooldownUntil == nil || cooldown.Until.After(*status.CooldownUntil)) {
			status.CooldownUntil, status.CooldownReason = cooldown.Until, cooldown.Reason
			if status.CooldownReason == "" {
				status.CooldownReason = "legacy_cooldown"
			}
		}
		if status.ResetAt != nil && !status.ResetAt.After(now) {
			status.QuotaKnown = false
			status.Remaining = nil
			status.ResetAt = nil
		}
		if status.QuotaKnown && status.Remaining != nil && *status.Remaining <= 0 && status.CooldownReason == "" {
			status.CooldownReason = "quota_exhausted"
			if status.CooldownUntil == nil {
				status.CooldownUntil = status.ResetAt
			}
		}
		status.RetryAfterSeconds = webImageRetrySeconds(status.CooldownUntil)
	}
	return statuses, nil
}

func (s *OpenAIWebImageService) BatchStatus(ctx context.Context, ids []int64) (*WebImageStatusBatch, error) {
	if len(ids) == 0 || len(ids) > 200 {
		return nil, errors.New("account_ids must contain between 1 and 200 IDs")
	}
	for _, id := range ids {
		if id <= 0 {
			return nil, errors.New("account IDs must be positive")
		}
	}
	accounts, err := s.accounts.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	result := &WebImageStatusBatch{Statuses: map[int64]*WebImageStatus{}, Errors: map[int64]string{}}
	for _, id := range ids {
		result.Errors[id] = "Web image shadow not found"
	}
	parents := map[int64]int64{}
	parentIDs := []int64{}
	for _, account := range accounts {
		if account.IsWebImageShadow() {
			parents[account.ID] = *account.ParentAccountID
			parentIDs = append(parentIDs, *account.ParentAccountID)
		}
	}
	statuses, err := s.parentStatuses(ctx, parentIDs)
	if err != nil {
		return nil, err
	}
	for id, parentID := range parents {
		result.Statuses[id] = statuses[parentID]
		delete(result.Errors, id)
	}
	return result, nil
}

type webImageCandidateKey struct{}
type webImageCandidateState struct {
	parents  map[int64]*Account
	statuses map[int64]*WebImageStatus
	err      error
}

// One batch per candidate pass; generate still re-reads the chosen parent and state.
func (s *OpenAIGatewayService) webImageCandidateContext(ctx context.Context, accounts []Account) context.Context {
	if s.webImages == nil || !OpenAIImageGenerationIntentFromContext(ctx) {
		return ctx
	}
	ids := []int64{}
	for _, account := range accounts {
		if account.IsWebImageShadow() {
			ids = append(ids, *account.ParentAccountID)
		}
	}
	if len(ids) == 0 {
		return ctx
	}
	state := &webImageCandidateState{parents: map[int64]*Account{}}
	parents, err := s.accountRepo.GetByIDs(ctx, ids)
	state.err = err
	for _, parent := range parents {
		state.parents[parent.ID] = parent
	}
	if err == nil {
		state.statuses, state.err = s.webImages.parentStatuses(ctx, ids)
	}
	return context.WithValue(ctx, webImageCandidateKey{}, state)
}
