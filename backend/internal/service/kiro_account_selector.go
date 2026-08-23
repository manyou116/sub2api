package service

import (
	"context"
	"errors"
	"math/rand"
	"sort"
	"strings"
)

// SelectKiroAccount picks a schedulable Kiro account for the gateway handler.
//
// Order (aligned with OpenAI sticky + light multi-candidate acquire):
//  1. sticky session hit when present, healthy, and not excluded
//  2. acquire sticky slot; if busy and wait queue room remains → WaitPlan
//  3. else fall through to pool (do not pin forever on a saturated sticky account)
//  4. filter pool: schedulable + IsKiro + quarantine + model + excluded
//  5. sort: Priority desc, KiroRemainingQuota desc (same tier shuffled)
//  6. try acquire across ranked candidates; first success wins
//  7. if all busy → WaitPlan on the top-ranked account
//
// reqModel is the client-facing model id. Quarantine keys use the mapped
// internal id (via resolveKiroInternalModel); whitelist accepts either form.
//
// Capped usage snapshots are NOT hard-filtered; remaining quota only ranks.
func (s *OpenAIGatewayService) SelectKiroAccount(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	excludedIDs map[int64]struct{},
	reqModel string,
) (*AccountSelectionResult, error) {
	if s.accountRepo == nil {
		return nil, errors.New("account repo not initialized")
	}

	cfg := s.schedulingConfig()
	sessionHash = strings.TrimSpace(sessionHash)
	reqModel = strings.TrimSpace(reqModel)

	// ---- sticky layer ----
	if sessionHash != "" {
		if hit := s.tryKiroStickyAccount(ctx, groupID, sessionHash, excludedIDs, reqModel); hit != nil {
			result, err := s.tryAcquireAccountSlot(ctx, hit.ID, hit.Concurrency)
			if err == nil && result != nil && result.Acquired {
				_ = s.setStickySessionAccountID(ctx, groupID, sessionHash, hit.ID, openaiStickySessionTTL)
				return s.newSelectionResult(ctx, hit, true, result.ReleaseFunc, nil)
			}
			// Align with OpenAI: only wait on sticky while queue room remains.
			if s.concurrencyService != nil {
				waitingCount, _ := s.concurrencyService.GetAccountWaitingCount(ctx, hit.ID)
				maxWaiting := cfg.StickySessionMaxWaiting
				if maxWaiting <= 0 {
					maxWaiting = 3
				}
				if waitingCount < maxWaiting {
					return s.newSelectionResult(ctx, hit, false, nil, &AccountWaitPlan{
						AccountID:      hit.ID,
						MaxConcurrency: hit.Concurrency,
						Timeout:        cfg.StickySessionWaitTimeout,
						MaxWaiting:     maxWaiting,
					})
				}
			}
			// Queue full / acquire error → fall through to other accounts.
		}
	}

	// ---- pool layer ----
	var (
		raw []Account
		err error
	)
	if groupID != nil {
		raw, err = s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, *groupID, PlatformKiro)
	} else {
		raw, err = s.accountRepo.ListSchedulableUngroupedByPlatform(ctx, PlatformKiro)
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, ErrNoAvailableAccounts
	}

	candidates := make([]*Account, 0, len(raw))
	for i := range raw {
		acct := &raw[i]
		if !s.kiroAccountEligible(acct, excludedIDs, reqModel) {
			continue
		}
		candidates = append(candidates, acct)
	}
	if len(candidates) == 0 {
		return nil, ErrNoAvailableAccounts
	}

	rand.Shuffle(len(candidates), func(i, j int) {
		candidates[i], candidates[j] = candidates[j], candidates[i]
	})
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority > candidates[j].Priority
		}
		return candidates[i].KiroRemainingQuota() > candidates[j].KiroRemainingQuota()
	})

	var firstBusy *Account
	for _, acct := range candidates {
		result, err := s.tryAcquireAccountSlot(ctx, acct.ID, acct.Concurrency)
		if err == nil && result != nil && result.Acquired {
			if sessionHash != "" {
				_ = s.setStickySessionAccountID(ctx, groupID, sessionHash, acct.ID, openaiStickySessionTTL)
			}
			return s.newSelectionResult(ctx, acct, true, result.ReleaseFunc, nil)
		}
		if firstBusy == nil {
			firstBusy = acct
		}
	}

	if firstBusy == nil {
		return nil, ErrNoAvailableAccounts
	}
	return s.newSelectionResult(ctx, firstBusy, false, nil, &AccountWaitPlan{
		AccountID:      firstBusy.ID,
		MaxConcurrency: firstBusy.Concurrency,
		Timeout:        cfg.FallbackWaitTimeout,
		MaxWaiting:     cfg.FallbackMaxWaiting,
	})
}

func (s *OpenAIGatewayService) tryKiroStickyAccount(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	excludedIDs map[int64]struct{},
	reqModel string,
) *Account {
	accountID, err := s.getStickySessionAccountID(ctx, groupID, sessionHash)
	if err != nil || accountID <= 0 {
		return nil
	}
	if excludedIDs != nil {
		if _, skip := excludedIDs[accountID]; skip {
			return nil
		}
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}
	if !s.kiroAccountEligible(account, excludedIDs, reqModel) {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}
	_ = s.refreshStickySessionTTL(ctx, groupID, sessionHash, openaiStickySessionTTL)
	return account
}

// kiroAccountEligible filters schedulable Kiro accounts for selection.
// reqModel is client-facing; model quarantine uses the mapped internal id.
func (s *OpenAIGatewayService) kiroAccountEligible(acct *Account, excludedIDs map[int64]struct{}, reqModel string) bool {
	if acct == nil || !acct.IsSchedulable() || !acct.IsKiro() {
		return false
	}
	if excludedIDs != nil {
		if _, skip := excludedIDs[acct.ID]; skip {
			return false
		}
	}
	if IsKiroAccountQuarantined(acct.ID) {
		return false
	}
	if reqModel != "" {
		mapped := resolveKiroInternalModel(acct, reqModel)
		if mapped != "" && IsKiroModelQuarantined(acct.ID, mapped) {
			return false
		}
		// Whitelist: accept client name or mapped internal id.
		if !acct.IsModelSupported(reqModel) && (mapped == "" || !acct.IsModelSupported(mapped)) {
			return false
		}
	}
	return true
}
