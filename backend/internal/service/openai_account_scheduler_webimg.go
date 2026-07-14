package service

import "context"

// Fork-owned OpenAI scheduler hooks for ChatGPT Web image path.
// Text concurrency slots are not consumed; webimg uses OpenAIWebImagesService inflight.

// acquireAccountSlotForSchedule acquires text concurrency unless this is a ChatGPT Web image request.
// Web images use OpenAIWebImagesService inflight instead of account concurrency slots.
func (s *defaultOpenAIAccountScheduler) acquireAccountSlotForSchedule(ctx context.Context, account *Account, req OpenAIAccountScheduleRequest) (*AccountSelectionResult, error) {
	if account == nil {
		return nil, nil
	}
	if req.RequiredImageCapability != "" && s.service != nil && s.service.UsesOpenAIWebImagesPath(account) {
		return &AccountSelectionResult{Account: account, Acquired: true, ReleaseFunc: func() {}}, nil
	}
	result, err := s.service.tryAcquireAccountSlot(ctx, account.ID, account.Concurrency)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return &AccountSelectionResult{Account: account}, nil
	}
	return &AccountSelectionResult{
		Account:     account,
		Acquired:    result.Acquired,
		ReleaseFunc: result.ReleaseFunc,
	}, nil
}

// accountBlockedByWebImageCooldown is the durable DB cooldown gate used by
// isAccountRequestCompatible. Kept here so the main scheduler file only calls it.
func accountBlockedByWebImageCooldown(account *Account, req OpenAIAccountScheduleRequest) bool {
	if account == nil || req.RequiredImageCapability == "" {
		return false
	}
	return account.IsWebImageRateLimited()
}
