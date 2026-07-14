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
	if shouldSkipAccountTextSlotForWebImages(s.service, ctx, account, req.RequestedModel, req.RequiredImageCapability) {
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

// shouldSkipAccountTextSlotForWebImages reports whether account text concurrency
// must stay free for a ChatGPT Web image request.
func shouldSkipAccountTextSlotForWebImages(
	svc *OpenAIGatewayService,
	ctx context.Context,
	account *Account,
	requestedModel string,
	imageCap OpenAIImagesCapability,
) bool {
	if svc == nil || account == nil || !svc.UsesOpenAIWebImagesPath(account) {
		return false
	}
	if imageCap != "" {
		return true
	}
	if isOpenAIImageGenerationModel(requestedModel) {
		return true
	}
	return OpenAIImageGenerationIntentFromContext(ctx)
}

// ReleaseAccountTextSlotIfWebImages drops a mistakenly acquired text concurrency
// slot when the selected account will serve via ChatGPT Web images.
func ReleaseAccountTextSlotIfWebImages(svc *OpenAIGatewayService, selection *AccountSelectionResult) {
	if selection == nil || selection.Account == nil || svc == nil {
		return
	}
	if !svc.UsesOpenAIWebImagesPath(selection.Account) {
		return
	}
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
		selection.ReleaseFunc = nil
	}
	selection.Acquired = false
}

// tryAcquireAccountSlotForOpenAIRequest skips text slots for ChatGPT Web image accounts.
func (s *OpenAIGatewayService) tryAcquireAccountSlotForOpenAIRequest(
	ctx context.Context,
	account *Account,
	requestedModel string,
	imageCap OpenAIImagesCapability,
) (*AcquireResult, error) {
	if account == nil {
		return nil, nil
	}
	if shouldSkipAccountTextSlotForWebImages(s, ctx, account, requestedModel, imageCap) {
		return &AcquireResult{Acquired: true, ReleaseFunc: func() {}}, nil
	}
	return s.tryAcquireAccountSlot(ctx, account.ID, account.Concurrency)
}
